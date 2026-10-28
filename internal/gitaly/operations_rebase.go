package gitaly

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/merge"
	"github.com/banyan-vcs/banyan/internal/model"
)

// UserRebaseToRef rebases SourceSha onto FirstParentRef and writes the
// result to a refs/merge-requests/* target — the merge-train rebase shape.
func (os *operationService) UserRebaseToRef(ctx context.Context, req *gitalypb.UserRebaseToRefRequest) (*gitalypb.UserRebaseToRefResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	if len(req.GetFirstParentRef()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty FirstParentRef")
	}
	if req.GetSourceSha() == "" {
		return nil, status.Error(codes.InvalidArgument, "empty SourceSha")
	}
	targetRef := string(req.GetTargetRef())
	if targetRef == "" {
		return nil, status.Error(codes.InvalidArgument, "empty TargetRef")
	}
	if !strings.HasPrefix(targetRef, "refs/merge-requests") {
		return nil, status.Error(codes.InvalidArgument, "invalid TargetRef")
	}
	committer, err := requestIdentity(req.GetUser(), req.GetTimestamp())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	upstream, err := os.s.resolveRevision(ctx, name, string(req.GetFirstParentRef()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid FirstParentRef")
	}
	upstream, err = os.peelToCommit(ctx, name, upstream)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid FirstParentRef")
	}
	source, err := os.s.resolveRevision(ctx, name, req.GetSourceSha())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid SourceSha")
	}
	source, err = os.peelToCommit(ctx, name, source)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid SourceSha")
	}

	tl := fullRefTimeline(name, targetRef)
	oldTarget, ok, _ := os.timelineHead(ctx, name, tl)
	if !ok {
		oldTarget = model.OID("")
	}
	if expected := req.GetExpectedOldOid(); expected != "" {
		if !isHexOID(expected) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid expected old object ID: %q", expected)
		}
		oldTarget = model.OID(expected)
	}

	rebased, conflicts, err := os.rebaseOnto(ctx, name, upstream, source, committer)
	if err != nil {
		return nil, err
	}
	if conflicts != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"failed to rebase %s on %s while preparing %s due to conflict", source, upstream, targetRef)
	}
	if err := os.casRef(ctx, name, tl, oldTarget, rebased); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"could not update %s. Please refresh and try again", targetRef)
	}
	return &gitalypb.UserRebaseToRefResponse{CommitId: rebased.String()}, nil
}

// UserRebaseConfirmable is the two-phase MR rebase: the header names the
// branch being rebased (BranchSha) and the branch it lands on
// (RemoteBranch — same-repo only on this node); apply answers the rebased
// tip, the client confirmation moves the branch.
func (os *operationService) UserRebaseConfirmable(stream gitalypb.OperationService_UserRebaseConfirmableServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "empty UserRebaseConfirmableRequest.Header")
	}
	name, err := os.s.existingRepo(ctx, header.GetRepository())
	if err != nil {
		return err
	}
	branch := string(header.GetBranch())
	if !validBranchName(branch) {
		return status.Error(codes.InvalidArgument, "empty Branch")
	}
	if header.GetBranchSha() == "" {
		return status.Error(codes.InvalidArgument, "empty BranchSha")
	}
	if header.GetRemoteRepository() == nil {
		return status.Error(codes.InvalidArgument, "empty RemoteRepository")
	}
	if len(header.GetRemoteBranch()) == 0 {
		return status.Error(codes.InvalidArgument, "empty RemoteBranch")
	}
	committer, err := requestIdentity(header.GetUser(), header.GetTimestamp())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if !isHexOID(header.GetBranchSha()) {
		return status.Error(codes.NotFound, "invalid BranchSha")
	}
	oldrev := model.OID(header.GetBranchSha())

	// RemoteBranch is the base being rebased onto; cross-repo remotes are
	// rejected like every other cross-repo flow on this face.
	if remote := header.GetRemoteRepository(); remote.GetStorageName() != os.s.Opts.StorageName ||
		remote.GetRelativePath() != header.GetRepository().GetRelativePath() {
		return status.Error(codes.InvalidArgument,
			"remote_repository: cross-repository operations are not supported on this node")
	}
	start, err := os.s.resolveRevision(ctx, name, "refs/heads/"+string(header.GetRemoteBranch()))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "resolve remote branch: %v", err)
	}
	start, err = os.peelToCommit(ctx, name, start)
	if err != nil {
		return status.Error(codes.InvalidArgument, "remote branch does not point at a commit")
	}

	newrev, conflicts, err := os.rebaseOnto(ctx, name, start, oldrev, committer)
	if err != nil {
		return err
	}
	if conflicts != nil {
		return withDetail(codes.FailedPrecondition, "rebasing commits: merge conflict",
			&gitalypb.UserRebaseConfirmableError{
				Error: &gitalypb.UserRebaseConfirmableError_RebaseConflict{
					RebaseConflict: &gitalypb.MergeConflictError{
						ConflictingFiles:     conflictFiles(conflicts),
						ConflictingCommitIds: []string{start.String(), oldrev.String()},
					},
				},
			})
	}
	if err := stream.Send(&gitalypb.UserRebaseConfirmableResponse{
		UserRebaseConfirmableResponsePayload: &gitalypb.UserRebaseConfirmableResponse_RebaseSha{
			RebaseSha: newrev.String(),
		},
	}); err != nil {
		return err
	}

	second, err := stream.Recv()
	if err != nil {
		return err
	}
	if !second.GetApply() {
		return status.Error(codes.FailedPrecondition, "rebase aborted by client")
	}
	advertised, raw, hasBranch := os.branchState(ctx, name, branch)
	_ = advertised
	if !hasBranch {
		return status.Errorf(codes.NotFound, "reference not found: refs/heads/%s", branch)
	}
	if err := os.casRef(ctx, name, branchTimeline(name, branch), raw, newrev); err != nil {
		return status.Error(codes.FailedPrecondition, "unexpected old value")
	}
	return stream.Send(&gitalypb.UserRebaseConfirmableResponse{
		UserRebaseConfirmableResponsePayload: &gitalypb.UserRebaseConfirmableResponse_RebaseApplied{
			RebaseApplied: true,
		},
	})
}

// rebaseOnto replays branch's own commits (upstream excluded, merges
// dropped, patch-equivalent picks dropped — git rebase's todo semantics)
// onto upstream. Authors and messages are preserved; the committer is the
// acting identity. Became-empty commits are dropped, empty-from-the-start
// commits are kept (git's --empty=drop behavior for the former).
func (os *operationService) rebaseOnto(ctx context.Context, repo string, upstream, branch model.OID, committer model.Identity) (model.OID, []string, error) {
	upstreamClosure := map[model.OID]*commitRecord{}
	err := os.s.walkHistory(ctx, repo, []model.OID{upstream}, func(oid model.OID, rec *commitRecord) bool {
		upstreamClosure[oid] = rec
		return true
	})
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "rebase: walk upstream: %v", err)
	}
	branchClosure := map[model.OID]*commitRecord{}
	err = os.s.walkHistory(ctx, repo, []model.OID{branch}, func(oid model.OID, rec *commitRecord) bool {
		branchClosure[oid] = rec
		return true
	})
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "rebase: walk branch: %v", err)
	}

	// Patch-equivalence set (--cherry-pick): upstream-only, non-merge
	// commits drop their identical branch-side twins from the todo list.
	upstreamPatches := map[string]bool{}
	for oid, rec := range upstreamClosure {
		if _, inBranch := branchClosure[oid]; inBranch || len(rec.Parents) > 1 {
			continue
		}
		sig, err := os.patchSignature(ctx, repo, oid, rec)
		if err == nil {
			upstreamPatches[sig] = true
		}
	}

	// Todo list: branch-only commits, oldest first.
	var todo []model.OID
	todoSet := map[model.OID]*commitRecord{}
	for oid, rec := range branchClosure {
		if _, inUpstream := upstreamClosure[oid]; inUpstream {
			continue
		}
		todoSet[oid] = rec
	}
	// walkParents yields commit-time descending; sort oldest-first for the
	// replay, same-second chains keeping their walk order reversed.
	order := map[model.OID]int{}
	seq := 0
	err = os.s.walkHistory(ctx, repo, []model.OID{branch}, func(oid model.OID, _ *commitRecord) bool {
		seq++
		order[oid] = seq
		return true
	})
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "rebase: order walk: %v", err)
	}
	for oid := range todoSet {
		todo = append(todo, oid)
	}
	sort.Slice(todo, func(i, j int) bool {
		a, b := todoSet[todo[i]], todoSet[todo[j]]
		if a.When != b.When {
			return a.When < b.When
		}
		return order[todo[i]] > order[todo[j]] // later in the walk = older
	})

	os.s.Objects.BeginSynthBatch(repo)
	defer os.s.Objects.EndSynthBatch(repo)
	store := &operationMergeStore{TreeStore: os.s.Objects.TreeStoreFor(repo), objects: os.s.Objects, repo: repo}

	oursRec, err := os.s.commitRecord(ctx, repo, upstream)
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "rebase: read upstream: %v", err)
	}
	ours := upstream
	oursTree := oursRec.Tree
	for _, oid := range todo {
		rec := todoSet[oid]
		if len(rec.Parents) > 1 {
			continue // --no-merges
		}
		if sig, serr := os.patchSignature(ctx, repo, oid, rec); serr == nil && upstreamPatches[sig] {
			continue // clean cherry-pick of an upstream commit
		}
		baseTree := model.OID("")
		if len(rec.Parents) == 1 {
			if prec, perr := os.s.commitRecord(ctx, repo, rec.Parents[0]); perr == nil {
				baseTree = prec.Tree
			}
		}
		newTree, conflicts, merr := merge.Trees(store, os.s.Algo, baseTree, oursTree, rec.Tree, "", merge.Options{
			OursLabel: "HEAD", TheirsLabel: shortRev(oid.String()),
		})
		if merr != nil {
			return "", nil, status.Errorf(codes.Internal, "rebase: merge: %v", merr)
		}
		if conflicts != nil {
			return "", conflicts, nil
		}
		if newTree == oursTree {
			// Became empty: dropped when the source commit was non-empty.
			if len(rec.Parents) == 0 {
				if empty := model.EmptyTreeOID(os.s.Algo); rec.Tree != empty {
					continue
				}
			} else if prec, perr := os.s.commitRecord(ctx, repo, rec.Parents[0]); perr == nil && rec.Tree != prec.Tree {
				continue
			}
		}

		typ, raw, rerr := os.s.Objects.Raw(ctx, repo, oid)
		if rerr != nil || typ != model.CommitType {
			return "", nil, status.Errorf(codes.Internal, "rebase: read commit %s", oid)
		}
		decoded, derr := model.DecodeCommit(raw)
		if derr != nil {
			return "", nil, status.Errorf(codes.Internal, "rebase: decode %s: %v", oid, derr)
		}
		nc := &model.AnchoredCommit{
			Anchor:    model.RootAnchor,
			TreeOID:   newTree,
			Parents:   []model.OID{ours},
			Author:    decoded.Author,
			Committer: committer,
			Message:   decoded.Message,
		}
		ours = nc.OID(os.s.Algo)
		os.s.Objects.PutSynthetic(repo, ours, model.CommitType, nc.Encode())
		oursTree = newTree
	}
	os.s.Objects.FlushSynthBatch(repo)
	return ours, nil, nil
}

// patchSignature is the --cherry-pick patch-equivalence key: a hash over
// the commit's tree change set (path, both blob OIDs, modes) — identical
// edits hash identically, any content divergence differs.
func (os *operationService) patchSignature(ctx context.Context, repo string, oid model.OID, rec *commitRecord) (string, error) {
	baseTree := model.OID("")
	if len(rec.Parents) > 0 {
		if prec, err := os.s.commitRecord(ctx, repo, rec.Parents[0]); err == nil {
			baseTree = prec.Tree
		}
	}
	changes, err := os.s.treeDiff(ctx, repo, baseTree, rec.Tree)
	if err != nil {
		return "", err
	}
	h := sha1.New()
	for _, c := range changes {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00", c.path, c.oldOID, c.newOID, c.oldMode, c.newMode)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hasPrefix is gone: use strings.HasPrefix at the call sites.
