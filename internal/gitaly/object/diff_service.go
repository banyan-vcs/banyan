package object

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// diffService implements the file-level diff slice of gitaly.DiffService
// over treeDiff (internal/gitaly/treediff.go). RangeDiff/RawRangeDiff
// (comparing commit ranges against each other) stay pending.
type diffService struct {
	gitalypb.UnimplementedDiffServiceServer
	*kit.Base
}

// diffTreesBetween resolves two revisions to their trees and diffs them.
func (ds *diffService) diffBetween(ctx context.Context, repo, left, right string) ([]kit.TreeChange, error) {
	l, err := ds.ResolveRevision(ctx, repo, left)
	if err != nil {
		return nil, err
	}
	r, err := ds.ResolveRevision(ctx, repo, right)
	if err != nil {
		return nil, err
	}
	lt, err := ds.CommitRecord(ctx, repo, l)
	if err != nil {
		return nil, err
	}
	rt, err := ds.CommitRecord(ctx, repo, r)
	if err != nil {
		return nil, err
	}
	return ds.TreeDiff(ctx, repo, lt.Tree, rt.Tree)
}

// blobContent fetches raw bytes for an OID ("" for the absent side).
func (ds *diffService) blobContent(ctx context.Context, repo string, oid model.OID) []byte {
	if oid.IsZero() {
		return nil
	}
	_, raw, err := ds.Objects.Raw(ctx, repo, oid)
	if err != nil {
		return nil
	}
	return raw
}

// patchFor renders the unified patch of one change (nil = binary).
func (ds *diffService) patchFor(ctx context.Context, repo string, c kit.TreeChange) []byte {
	return kit.UnifiedPatch(ds.blobContent(ctx, repo, c.OldOID), ds.blobContent(ctx, repo, c.NewOID),
		c.Path, c.Path)
}

func (ds *diffService) CommitDelta(req *gitalypb.CommitDeltaRequest, stream gitalypb.DiffService_CommitDeltaServer) error {
	ctx := stream.Context()
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	changes, err := ds.diffBetween(ctx, name, req.GetLeftCommitId(), req.GetRightCommitId())
	if err != nil {
		return err
	}
	for _, c := range changes {
		delta := &gitalypb.CommitDelta{
			FromPath: []byte(c.Path), ToPath: []byte(c.Path),
			FromId:  c.OldOID.String(),
			ToId:    c.NewOID.String(),
			OldMode: kit.ModeValue(c.OldMode),
			NewMode: kit.ModeValue(c.NewMode),
		}
		if err := stream.Send(&gitalypb.CommitDeltaResponse{Deltas: []*gitalypb.CommitDelta{delta}}); err != nil {
			return err
		}
	}
	return nil
}

// maxPatchBytes caps one file's patch; larger files answer TooLarge with
// the truncated body.
const maxPatchBytes = 1 << 20

func (ds *diffService) CommitDiff(req *gitalypb.CommitDiffRequest, stream gitalypb.DiffService_CommitDiffServer) error {
	ctx := stream.Context()
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	changes, err := ds.diffBetween(ctx, name, req.GetLeftCommitId(), req.GetRightCommitId())
	if err != nil {
		return err
	}
	filter := kit.PathFilter(req.GetPaths())
	for _, c := range changes {
		if !filter.Matches(c.Path) {
			continue
		}
		oldContent := ds.blobContent(ctx, name, c.OldOID)
		newContent := ds.blobContent(ctx, name, c.NewOID)
		binary := kit.LooksBinary(oldContent) || kit.LooksBinary(newContent)
		added, removed := int32(0), int32(0)
		var patch []byte
		if !binary {
			added, removed = kit.PatchStats(oldContent, newContent)
			patch = kit.UnifiedPatch(oldContent, newContent, c.Path, c.Path)
		}
		tooLarge := len(patch) > maxPatchBytes
		if tooLarge {
			patch = patch[:maxPatchBytes]
		}
		head := &gitalypb.CommitDiffResponse{
			FromPath: []byte(c.Path), ToPath: []byte(c.Path),
			FromId: c.OldOID.String(), ToId: c.NewOID.String(),
			OldMode: kit.ModeValue(c.OldMode), NewMode: kit.ModeValue(c.NewMode),
			Binary: binary, TooLarge: tooLarge,
			LinesAdded: added, LinesRemoved: removed,
			RawPatchData: patch,
			EndOfPatch:   true,
		}
		if err := stream.Send(head); err != nil {
			return err
		}
	}
	return nil
}

func (ds *diffService) DiffStats(req *gitalypb.DiffStatsRequest, stream gitalypb.DiffService_DiffStatsServer) error {
	ctx := stream.Context()
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	changes, err := ds.diffBetween(ctx, name, req.GetLeftCommitId(), req.GetRightCommitId())
	if err != nil {
		return err
	}
	stats := make([]*gitalypb.DiffStats, 0, len(changes))
	for _, c := range changes {
		added, removed := kit.PatchStats(ds.blobContent(ctx, name, c.OldOID), ds.blobContent(ctx, name, c.NewOID))
		stats = append(stats, &gitalypb.DiffStats{
			Path: []byte(c.Path), OldPath: []byte(c.Path),
			Additions: added, Deletions: removed,
		})
	}
	return stream.Send(&gitalypb.DiffStatsResponse{Stats: stats})
}

// rawDiff renders the whole tree diff as git-diff-style text.
func (ds *diffService) rawDiffText(ctx context.Context, name, left, right string) (string, error) {
	changes, err := ds.diffBetween(ctx, name, left, right)
	if err != nil {
		return "", err
	}
	var b []byte
	for _, c := range changes {
		b = append(b, ds.gitFormatPatch(ctx, name, c)...)
	}
	return string(b), nil
}

func (ds *diffService) RawDiff(req *gitalypb.RawDiffRequest, stream gitalypb.DiffService_RawDiffServer) error {
	ctx := stream.Context()
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	text, err := ds.rawDiffText(ctx, name, req.GetLeftCommitId(), req.GetRightCommitId())
	if err != nil {
		return err
	}
	return kit.StreamBytes([]byte(text), func(b []byte) error {
		return stream.Send(&gitalypb.RawDiffResponse{Data: b})
	})
}

// RawPatch mirrors RawDiff (the format-packet headers of `git format-patch`
// are a presentation concern of the caller; the patch body is identical).
func (ds *diffService) RawPatch(req *gitalypb.RawPatchRequest, stream gitalypb.DiffService_RawPatchServer) error {
	ctx := stream.Context()
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	text, err := ds.rawDiffText(ctx, name, req.GetLeftCommitId(), req.GetRightCommitId())
	if err != nil {
		return err
	}
	return kit.StreamBytes([]byte(text), func(b []byte) error {
		return stream.Send(&gitalypb.RawPatchResponse{Data: b})
	})
}

func (ds *diffService) DiffBlobs(req *gitalypb.DiffBlobsRequest, stream gitalypb.DiffService_DiffBlobsServer) error {
	ctx := stream.Context()
	if _, err := ds.ExistingRepo(ctx, req.GetRepository()); err != nil {
		return err
	}
	for _, pair := range req.GetBlobPairs() {
		patch := kit.UnifiedPatch(pair.GetLeftBlob(), pair.GetRightBlob(), "blob", "blob")
		binary := patch == nil
		added, removed := int32(0), int32(0)
		if !binary {
			added, removed = kit.PatchStats(pair.GetLeftBlob(), pair.GetRightBlob())
		}
		resp := &gitalypb.DiffBlobsResponse{
			Binary:       binary,
			Patch:        patch,
			PatchSize:    int32(len(patch)),
			LinesAdded:   added,
			LinesRemoved: removed,
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// GetPatchID answers a best-effort stable id: SHA-1 over the rendered
// patch body (context-free hashing of git patch-id is a refinement).
func (ds *diffService) GetPatchID(ctx context.Context, req *gitalypb.GetPatchIDRequest) (*gitalypb.GetPatchIDResponse, error) {
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	text, err := ds.rawDiffText(ctx, name, string(req.GetOldRevision()), string(req.GetNewRevision()))
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(text))
	return &gitalypb.GetPatchIDResponse{PatchId: hex.EncodeToString(sum[:])}, nil
}

func (ds *diffService) FindChangedPaths(req *gitalypb.FindChangedPathsRequest, stream gitalypb.DiffService_FindChangedPathsServer) error {
	ctx := stream.Context()
	name, err := ds.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	for _, r := range req.GetRequests() {
		switch q := r.GetType().(type) {
		case *gitalypb.FindChangedPathsRequest_Request_TreeRequest_:
			changes, err := ds.diffBetween(ctx, name, q.TreeRequest.GetLeftTreeRevision(), q.TreeRequest.GetRightTreeRevision())
			if err != nil {
				return err
			}
			if err := sendChangedPaths(stream, changes); err != nil {
				return err
			}
		case *gitalypb.FindChangedPathsRequest_Request_CommitRequest_:
			rev := q.CommitRequest.GetCommitRevision()
			head, err := ds.ResolveRevision(ctx, name, rev)
			if err != nil {
				return err
			}
			rec, err := ds.CommitRecord(ctx, name, head)
			if err != nil {
				return err
			}
			var parentTrees []model.OID
			for _, prev := range q.CommitRequest.GetParentCommitRevisions() {
				if po, err := ds.ResolveRevision(ctx, name, prev); err == nil {
					if pr, err := ds.CommitRecord(ctx, name, po); err == nil {
						parentTrees = append(parentTrees, pr.Tree)
					}
				}
			}
			if len(parentTrees) == 0 && len(rec.Parents) > 0 {
				if pr, err := ds.CommitRecord(ctx, name, rec.Parents[0]); err == nil {
					parentTrees = append(parentTrees, pr.Tree)
				}
			}
			var changes []kit.TreeChange
			if len(parentTrees) == 0 {
				empty := model.OID("")
				changes, err = ds.TreeDiff(ctx, name, empty, rec.Tree)
			} else {
				// union of changes against each parent (merge posture)
				for _, pt := range parentTrees {
					cs, err := ds.TreeDiff(ctx, name, pt, rec.Tree)
					if err != nil {
						return err
					}
					changes = append(changes, cs...)
				}
			}
			if err != nil {
				return err
			}
			if err := sendChangedPaths(stream, changes); err != nil {
				return err
			}
		}
	}
	return nil
}

func sendChangedPaths(stream gitalypb.DiffService_FindChangedPathsServer, changes []kit.TreeChange) error {
	seen := map[string]bool{}
	var out []*gitalypb.ChangedPaths
	for _, c := range changes {
		if seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		st := gitalypb.ChangedPaths_MODIFIED
		switch c.Status() {
		case 'A':
			st = gitalypb.ChangedPaths_ADDED
		case 'D':
			st = gitalypb.ChangedPaths_DELETED
		}
		out = append(out, &gitalypb.ChangedPaths{
			Path: []byte(c.Path), Status: st,
			OldMode: kit.ModeValue(c.OldMode), NewMode: kit.ModeValue(c.NewMode),
			OldBlobId: c.OldOID.String(), NewBlobId: c.NewOID.String(),
		})
	}
	if len(out) > 0 {
		return stream.Send(&gitalypb.FindChangedPathsResponse{Paths: out})
	}
	return nil
}

// gitFormatPatch renders one change in git-diff format (diff --git header,
// index line, then the unified body) so RawDiff output byte-compares with
// the git CLI.
func (ds *diffService) gitFormatPatch(ctx context.Context, repo string, c kit.TreeChange) []byte {
	oldContent := ds.blobContent(ctx, repo, c.OldOID)
	newContent := ds.blobContent(ctx, repo, c.NewOID)
	binary := kit.LooksBinary(oldContent) || kit.LooksBinary(newContent)
	var head strings.Builder
	fmt.Fprintf(&head, "diff --git a/%s b/%s\n", c.Path, c.Path)
	abbr := func(o model.OID, n int) string {
		s := o.String()
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	switch c.Status() {
	case 'A':
		fmt.Fprintf(&head, "new file mode %s\nindex 0000000..%s\n", modeString(c.NewMode), abbr(c.NewOID, 7))
	case 'D':
		fmt.Fprintf(&head, "deleted file mode %s\nindex %s..0000000\n", modeString(c.OldMode), abbr(c.OldOID, 7))
	default:
		if c.OldMode == c.NewMode {
			fmt.Fprintf(&head, "index %s..%s %s\n", abbr(c.OldOID, 7), abbr(c.NewOID, 7), modeString(c.NewMode))
		} else {
			fmt.Fprintf(&head, "old mode %s\nnew mode %s\nindex %s..%s\n",
				modeString(c.OldMode), modeString(c.NewMode), abbr(c.OldOID, 7), abbr(c.NewOID, 7))
		}
	}
	if binary {
		fmt.Fprintf(&head, "Binary files a/%s and b/%s differ\n", c.Path, c.Path)
		return []byte(head.String())
	}
	oldPath, newPath := c.Path, c.Path
	if c.Status() == 'A' {
		oldPath = "/dev/null"
	}
	if c.Status() == 'D' {
		newPath = "/dev/null"
	}
	body := kit.UnifiedPatch(oldContent, newContent, oldPath, newPath)
	return append([]byte(head.String()), body...)
}

// modeString renders a tree mode ("100644") zero-extended like git.
func modeString(mode string) string {
	for len(mode) < 6 {
		mode = "0" + mode
	}
	return mode
}

// BlobContent reads one blob's bytes (empty on miss) — the commit
// family's stats pass shares it.
func BlobContent(ctx context.Context, b *kit.Base, repo string, oid model.OID) []byte {
	ds := &diffService{Base: b}
	return ds.blobContent(ctx, repo, oid)
}
