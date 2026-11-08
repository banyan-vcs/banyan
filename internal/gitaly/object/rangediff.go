package object

import (
	"crypto/sha1"
	"encoding/hex"

	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// Range diff: compare two commit ranges as `git range-diff` does — pair
// commits by patch equivalence (our stable SHA-1 over the rendered patch,
// the same id GetPatchID answers), then report equal pairs, unpaired
// additions/deletions and adjacent re-pairings as unequal comparisons.
//
// The pairing is the honest subset: exact patch-id matches pair as EQUAL,
// everything else reports as ADDED/DELETED on the unmatched side (git's
// cost-based fuzzy matching and the inter-pair diff hunks are not
// reconstructed; RawRangeDiff renders the summary lines only).

// rangediffSide is one commit of a range with its patch id.
type rangediffSide struct {
	oid     model.OID
	subject string
	patchID string
}

// expandRange collects the commits of `<since>..<until>` in newest-first
// order (rev-list semantics, first parents of the until closure minus the
// since closure).
func (ds *diffService) expandRange(ctx context.Context, repo, since, until string) ([]rangediffSide, error) {
	name := repo
	utip, err := ds.ResolveRevision(ctx, name, until)
	if err != nil {
		return nil, err
	}
	exclude := map[model.OID]bool{}
	if since != "" {
		if stip, err := ds.ResolveRevision(ctx, name, since); err == nil {
			_ = ds.WalkParents(ctx, name, []model.OID{stip}, &kit.WalkFilter{}, func(oid model.OID, _ *kit.CommitRecord) bool {
				exclude[oid] = true
				return true
			})
		}
	}
	var out []rangediffSide
	_ = ds.WalkParents(ctx, name, []model.OID{utip}, &kit.WalkFilter{}, func(oid model.OID, rec *kit.CommitRecord) bool {
		if exclude[oid] {
			return false
		}
		msg := commitMessageOf(ctx, ds.Base, name, oid)
		subject := strings.SplitN(strings.TrimSuffix(msg, "\n"), "\n", 2)[0]
		id := ds.patchIdOf(ctx, name, rec)
		out = append(out, rangediffSide{oid: oid, subject: subject, patchID: id})
		return true
	})
	return out, nil
}

// commitMessageOf reads the raw commit body for fields kit.CommitRecord lacks.
func commitMessageOf(ctx context.Context, b *kit.Base, repo string, oid model.OID) string {
	_, raw, err := b.Objects.Raw(ctx, repo, oid)
	if err != nil {
		return ""
	}
	if c, err := model.DecodeCommit(raw); err == nil {
		return c.Message
	}
	return ""
}

// patchIdOf renders the commit's patch and answers the stable SHA-1.
func (ds *diffService) patchIdOf(ctx context.Context, repo string, rec *kit.CommitRecord) string {
	var base model.OID
	if len(rec.Parents) > 0 {
		if prec, err := ds.CommitRecord(ctx, repo, rec.Parents[0]); err == nil {
			base = prec.Tree
		}
	}
	changes, err := ds.TreeDiff(ctx, repo, base, rec.Tree)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "%s %s %s %s %s\n", c.Path, c.OldOID, c.NewOID, c.OldMode, c.NewMode)
	}
	return sha1Hex([]byte(b.String()))
}

// rangePair resolves the request's range spec into two range strings
// (each `<since>..<until>`), matching upstream's argument handling:
// RevisionRange is the three-dot form expanded around the merge base.
func (ds *diffService) rangePair(ctx context.Context, repo string, req *gitalypb.RangeDiffRequest) (left, right string, err error) {
	switch spec := req.GetRangeSpec().(type) {
	case *gitalypb.RangeDiffRequest_RangePair:
		p := spec.RangePair
		left, right = p.GetRange1(), p.GetRange2()
	case *gitalypb.RangeDiffRequest_RevisionRange:
		r := spec.RevisionRange
		a, e1 := ds.ResolveRevision(ctx, repo, r.GetRev1())
		b, e2 := ds.ResolveRevision(ctx, repo, r.GetRev2())
		if e1 != nil || e2 != nil {
			return "", "", status.Error(codes.InvalidArgument, "gitaly: invalid revisions")
		}
		base, _ := ds.MergeBaseOf(ctx, repo, a, b)
		bs := base.String()
		if bs == "" {
			bs = ""
		}
		left, right = bs+".."+r.GetRev1(), bs+".."+r.GetRev2()
	case *gitalypb.RangeDiffRequest_BaseWithRevisions:
		b := spec.BaseWithRevisions
		left = b.GetBase() + ".." + b.GetRev1()
		right = b.GetBase() + ".." + b.GetRev2()
	default:
		return "", "", status.Error(codes.InvalidArgument, "gitaly: range_spec is required")
	}
	return left, right, nil
}

func splitRangeSpec(r string) (since, until string) {
	if i := strings.Index(r, ".."); i >= 0 {
		return r[:i], r[i+2:]
	}
	return "", r
}

// RangeDiff streams the pairwise comparison of two ranges.
func (ds *diffService) RangeDiff(req *gitalypb.RangeDiffRequest, stream gitalypb.DiffService_RangeDiffServer) error {
	ctx := stream.Context()
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	leftSpec, rightSpec, err := ds.rangePair(ctx, name, req)
	if err != nil {
		return err
	}
	ls, lu := splitRangeSpec(leftSpec)
	rs, ru := splitRangeSpec(rightSpec)
	leftSide, err := ds.expandRange(ctx, name, ls, lu)
	if err != nil {
		return err
	}
	rightSide, err := ds.expandRange(ctx, name, rs, ru)
	if err != nil {
		return err
	}

	rightByID := map[string]int{}
	for i, c := range rightSide {
		rightByID[c.patchID] = i
	}
	rightSeen := make([]bool, len(rightSide))
	send := func(resp *gitalypb.RangeDiffResponse) error {
		return stream.Send(resp)
	}
	ri := 0
	for _, l := range leftSide {
		if j, ok := rightByID[l.patchID]; ok {
			rightSeen[j] = true
			if err := send(&gitalypb.RangeDiffResponse{
				FromCommitId: l.oid.String(), ToCommitId: rightSide[j].oid.String(),
				Comparison:         gitalypb.RangeDiffResponse_COMPARATOR_EQUAL_UNSPECIFIED,
				CommitMessageTitle: l.subject,
				EndOfPatch:         true,
			}); err != nil {
				return err
			}
			continue
		}
		// Unpaired left commit: report against the nearest unpaired right
		// commit as NOT_EQUAL when one exists, otherwise as a deletion.
		for ri < len(rightSide) && (rightSeen[ri] || rightSide[ri].patchID == "") {
			ri++
		}
		if ri < len(rightSide) {
			rightSeen[ri] = true
			if err := send(&gitalypb.RangeDiffResponse{
				FromCommitId: l.oid.String(), ToCommitId: rightSide[ri].oid.String(),
				Comparison:         gitalypb.RangeDiffResponse_COMPARATOR_NOT_EQUAL,
				CommitMessageTitle: l.subject,
				EndOfPatch:         true,
			}); err != nil {
				return err
			}
			ri++
			continue
		}
		if err := send(&gitalypb.RangeDiffResponse{
			FromCommitId: l.oid.String(), ToCommitId: "",
			Comparison:         gitalypb.RangeDiffResponse_COMPARATOR_NOT_EQUAL,
			CommitMessageTitle: l.subject,
			EndOfPatch:         true,
		}); err != nil {
			return err
		}
	}
	for j, r := range rightSide {
		if rightSeen[j] {
			continue
		}
		if err := send(&gitalypb.RangeDiffResponse{
			FromCommitId: "", ToCommitId: r.oid.String(),
			Comparison:         gitalypb.RangeDiffResponse_COMPARATOR_NOT_EQUAL,
			CommitMessageTitle: r.subject,
			EndOfPatch:         true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// RawRangeDiff renders the `git range-diff` summary text: one numbered
// line per comparison in git's "<n>: <subject> <op> <m>: <subject>" shape.
func (ds *diffService) RawRangeDiff(req *gitalypb.RawRangeDiffRequest, stream gitalypb.DiffService_RawRangeDiffServer) error {
	ctx := stream.Context()
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	asRange := &gitalypb.RangeDiffRequest{Repository: req.GetRepository()}
	switch spec := req.GetRangeSpec().(type) {
	case *gitalypb.RawRangeDiffRequest_RangePair:
		asRange.RangeSpec = &gitalypb.RangeDiffRequest_RangePair{RangePair: spec.RangePair}
	case *gitalypb.RawRangeDiffRequest_RevisionRange:
		asRange.RangeSpec = &gitalypb.RangeDiffRequest_RevisionRange{RevisionRange: spec.RevisionRange}
	case *gitalypb.RawRangeDiffRequest_BaseWithRevisions:
		asRange.RangeSpec = &gitalypb.RangeDiffRequest_BaseWithRevisions{BaseWithRevisions: spec.BaseWithRevisions}
	default:
		return status.Error(codes.InvalidArgument, "gitaly: range_spec is required")
	}
	leftSpec, rightSpec, err := ds.rangePair(ctx, name, asRange)
	if err != nil {
		return err
	}
	ls, lu := splitRangeSpec(leftSpec)
	rs, ru := splitRangeSpec(rightSpec)
	leftSide, err := ds.expandRange(ctx, name, ls, lu)
	if err != nil {
		return err
	}
	rightSide, err := ds.expandRange(ctx, name, rs, ru)
	if err != nil {
		return err
	}
	rightByID := map[string]int{}
	for i, c := range rightSide {
		rightByID[c.patchID] = i
	}
	seen := make([]bool, len(rightSide))
	var b strings.Builder
	ri := 0
	for _, l := range leftSide {
		if j, ok := rightByID[l.patchID]; ok {
			seen[j] = true
			fmt.Fprintf(&b, "%d: %s = %d: %s\n", indexOfSide(leftSide, l)+1, l.subject, j+1, rightSide[j].subject)
			continue
		}
		for ri < len(rightSide) && seen[ri] {
			ri++
		}
		if ri < len(rightSide) {
			seen[ri] = true
			fmt.Fprintf(&b, "%d: %s ! %d: %s\n", indexOfSide(leftSide, l)+1, l.subject, ri+1, rightSide[ri].subject)
			ri++
		} else {
			fmt.Fprintf(&b, "%d: %s <--\n", indexOfSide(leftSide, l)+1, l.subject)
		}
	}
	for j, r := range rightSide {
		if !seen[j] {
			fmt.Fprintf(&b, "    --> %d: %s\n", j+1, r.subject)
		}
	}
	return kit.StreamBytes([]byte(b.String()), func(p []byte) error {
		return stream.Send(&gitalypb.RawRangeDiffResponse{Data: p})
	})
}

func indexOfSide(sides []rangediffSide, c rangediffSide) int {
	for i := range sides {
		if sides[i].oid == c.oid {
			return i
		}
	}
	return 0
}

func idx(total, i int) int { return i + 1 }

func sha1Hex(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}
