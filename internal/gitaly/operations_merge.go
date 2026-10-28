package gitaly

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/merge"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// User-facing merge surface (docs/19): UserMergeBranch, UserMergeToRef,
// UserSquash, UserCherryPick and UserRevert ride one 3-way engine —
// internal/merge (tree-entry merge + content diff3, git merge-file oracle
// tested) over the synthetic-object layer, with commits assembled through
// the same AnchoredCommit encoding git pushes produce. Renames are not
// detected (delete+add = conflict), the honest subset git's own merge
// defaults use when rename detection is off.

// operationMergeStore adapts ObjectService to merge.Store (the same shape
// as gitwire's convergence adapter): reads through the repo's TreeStore,
// writes land as synthetic objects.
type operationMergeStore struct {
	model.TreeStore
	objects *storage.ObjectService
	repo    string
}

func (m *operationMergeStore) PutBlob(data []byte) model.OID {
	oid := (&model.Blob{Content: data}).OID(m.objects.Algo)
	m.objects.PutSynthetic(m.repo, oid, model.BlobType, data)
	return oid
}

// mergeBaseOID answers the first common ancestor of two commits in
// commit-time order (the single-base form the merge surface uses); an
// empty OID means unrelated histories.
func (s *Server) mergeBaseOID(ctx context.Context, repo string, a, b model.OID) (model.OID, error) {
	ancestors := map[model.OID]bool{}
	err := s.walkParents(ctx, repo, []model.OID{a}, &walkFilter{}, func(oid model.OID, _ *commitRecord) bool {
		ancestors[oid] = true
		return true
	})
	if err != nil {
		return "", err
	}
	var base model.OID
	err = s.walkParents(ctx, repo, []model.OID{b}, &walkFilter{}, func(oid model.OID, _ *commitRecord) bool {
		if ancestors[oid] {
			base = oid
			return false
		}
		return true
	})
	return base, err
}

// mergeSpec is one server-side merge: the 3-way tree merge plus the commit
// assembled over its result. baseCommit zero means "compute the merge
// base"; explicit empty tree merges are expressed as the empty-tree OID of
// the repo's algorithm... and absent bases (unrelated histories) pass "".
type mergeSpec struct {
	ours, theirs           model.OID // commits
	theirsEmpty            bool      // theirs side is the empty tree (root-commit revert)
	baseCommit             model.OID // "" = compute from ours/theirs
	noBase                 bool      // force the empty base (cherry-pick of a root commit)
	parents                []model.OID
	author, committer      model.Identity
	message                string
	oursLabel, theirsLabel string
}

// applyMerge runs the 3-way merge and writes the resulting commit as one
// synthetic batch, persisted before returning — a ref CAS afterwards never
// references unregistered objects (cluster visibility invariant).
func (os *operationService) applyMerge(ctx context.Context, repo string, spec mergeSpec) (model.OID, []string, error) {
	oursRec, err := os.s.commitRecord(ctx, repo, spec.ours)
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "merge: resolve ours %s: %v", spec.ours, err)
	}
	theirsTree := model.OID("")
	if !spec.theirsEmpty {
		theirsRec, err := os.s.commitRecord(ctx, repo, spec.theirs)
		if err != nil {
			return "", nil, status.Errorf(codes.Internal, "merge: resolve theirs %s: %v", spec.theirs, err)
		}
		theirsTree = theirsRec.Tree
	}
	baseTree := model.OID("")
	switch {
	case spec.noBase:
	case spec.baseCommit != "":
		baseRec, err := os.s.commitRecord(ctx, repo, spec.baseCommit)
		if err != nil {
			return "", nil, status.Errorf(codes.Internal, "merge: resolve base %s: %v", spec.baseCommit, err)
		}
		baseTree = baseRec.Tree
	default:
		base, err := os.s.mergeBaseOID(ctx, repo, spec.ours, spec.theirs)
		if err != nil {
			return "", nil, status.Errorf(codes.Internal, "merge: merge base: %v", err)
		}
		if !base.IsZero() {
			baseRec, err := os.s.commitRecord(ctx, repo, base)
			if err != nil {
				return "", nil, status.Errorf(codes.Internal, "merge: merge base %s: %v", base, err)
			}
			baseTree = baseRec.Tree
		}
	}

	os.s.Objects.BeginSynthBatch(repo)
	defer os.s.Objects.EndSynthBatch(repo)

	store := &operationMergeStore{TreeStore: os.s.Objects.TreeStoreFor(repo), objects: os.s.Objects, repo: repo}
	treeOID, conflicts, err := merge.Trees(store, os.s.Algo, baseTree, oursRec.Tree, theirsTree, "", merge.Options{
		OursLabel:   spec.oursLabel,
		TheirsLabel: spec.theirsLabel,
	})
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "merge: %v", err)
	}
	if len(conflicts) > 0 {
		return "", conflicts, nil
	}

	msg := spec.message
	if msg != "" && !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	commit := &model.AnchoredCommit{
		Anchor:    model.RootAnchor,
		TreeOID:   treeOID,
		Parents:   spec.parents,
		Author:    spec.author,
		Committer: spec.committer,
		Message:   msg,
	}
	commitOID := commit.OID(os.s.Algo)
	os.s.Objects.PutSynthetic(repo, commitOID, model.CommitType, commit.Encode())
	os.s.Objects.FlushSynthBatch(repo) // durability before any ref CAS
	return commitOID, nil, nil
}

// requestIdentity renders the acting user + timestamp as a git signature.
func requestIdentity(user *gitalypb.User, ts *timestamppb.Timestamp) (model.Identity, error) {
	if user == nil || len(user.GetName()) == 0 || len(user.GetEmail()) == 0 {
		return model.Identity{}, fmt.Errorf("empty user name or email")
	}
	when := time.Now()
	if ts != nil {
		when = ts.AsTime()
	}
	return model.Identity{Name: string(user.GetName()), Email: string(user.GetEmail()), When: when}, nil
}

// withDetail attaches protobuf error details to a status error.
func withDetail(code codes.Code, msg string, details ...protoadapt.MessageV1) error {
	st := status.New(code, msg)
	st, err := st.WithDetails(details...)
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}

// conflictFiles renders conflict paths as the wire shape every error detail shares.
func conflictFiles(paths []string) [][]byte {
	out := make([][]byte, 0, len(paths))
	for _, p := range paths {
		out = append(out, []byte(p))
	}
	return out
}

// UserMergeBranch is the two-phase MR merge: apply (compute the merge
// commit, answer its OID), then a client confirmation that CASes it onto
// the branch. An aborted second message leaves the objects orphaned — the
// same quarantine semantics upstream has.
func (os *operationService) UserMergeBranch(stream gitalypb.OperationService_UserMergeBranchServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	name, err := os.s.existingRepo(ctx, first.GetRepository())
	if err != nil {
		return err
	}
	branch := string(first.GetBranch())
	if !validBranchName(branch) {
		return status.Error(codes.InvalidArgument, "empty branch name")
	}
	if first.GetCommitId() == "" {
		return status.Error(codes.InvalidArgument, "empty commit ID")
	}
	if len(first.GetMessage()) == 0 {
		return status.Error(codes.InvalidArgument, "empty message")
	}
	ident, err := requestIdentity(first.GetUser(), first.GetTimestamp())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	// ours: the expected old OID when given, else the branch tip. Client
	// expectations arrive as advertised (woven) OIDs; the merge parents and
	// the CAS use the raw timeline head.
	advertised, raw, hasBranch := os.branchState(ctx, name, branch)
	if !hasBranch {
		return status.Errorf(codes.NotFound, "reference not found: refs/heads/%s", branch)
	}
	ours := raw
	if old := first.GetExpectedOldOid(); old != "" {
		if !isHexOID(old) {
			return status.Errorf(codes.InvalidArgument, "invalid expected old object ID: %q", old)
		}
		if model.OID(old) != raw && model.OID(old) != advertised {
			return status.Error(codes.FailedPrecondition, "unexpected old value")
		}
	}
	ours, err = os.peelToCommit(ctx, name, ours)
	if err != nil {
		return status.Error(codes.InvalidArgument, "merge: ours is not a commit")
	}
	theirs, err := os.peelToCommit(ctx, name, model.OID(first.GetCommitId()))
	if err != nil {
		return status.Errorf(codes.NotFound, "merge: commit %s not found", first.GetCommitId())
	}

	parents := []model.OID{ours}
	if !first.GetSquash() {
		parents = append(parents, theirs)
	}
	mergeOID, conflicts, err := os.applyMerge(ctx, name, mergeSpec{
		ours: ours, theirs: theirs,
		parents: parents,
		author:  ident, committer: ident,
		message:     string(first.GetMessage()),
		oursLabel:   "HEAD",
		theirsLabel: shortRev(first.GetCommitId()),
	})
	if err != nil {
		return err
	}
	if conflicts != nil {
		return withDetail(codes.FailedPrecondition, "merging commits: merge conflict",
			&gitalypb.UserMergeBranchError{
				Error: &gitalypb.UserMergeBranchError_MergeConflict{
					MergeConflict: &gitalypb.MergeConflictError{
						ConflictingFiles:     conflictFiles(conflicts),
						ConflictingCommitIds: []string{ours.String(), theirs.String()},
					},
				},
			})
	}
	if err := stream.Send(&gitalypb.UserMergeBranchResponse{CommitId: mergeOID.String()}); err != nil {
		return err
	}

	second, err := stream.Recv()
	if err != nil {
		return err
	}
	if !second.GetApply() {
		return status.Error(codes.FailedPrecondition, "merge aborted by client")
	}
	tl := branchTimeline(name, branch)
	if err := os.casRef(ctx, name, tl, ours, mergeOID); err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return withDetail(codes.FailedPrecondition, "unexpected old value",
				&gitalypb.UserMergeBranchError{
					Error: &gitalypb.UserMergeBranchError_ReferenceUpdate{
						ReferenceUpdate: &gitalypb.ReferenceUpdateError{
							ReferenceName: []byte("refs/heads/" + branch),
							OldOid:        ours.String(),
							NewOid:        mergeOID.String(),
						},
					},
				})
		}
		return err
	}
	return stream.Send(&gitalypb.UserMergeBranchResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{CommitId: mergeOID.String()},
	})
}

// UserMergeToRef merges SourceSha into the branch/first-parent ref and
// writes the merge commit to a refs/merge-requests/... target — the merge
// train / merge-result shape. The target ref CASes against its current
// value (or expected_old_oid).
func (os *operationService) UserMergeToRef(ctx context.Context, req *gitalypb.UserMergeToRefRequest) (*gitalypb.UserMergeToRefResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	source := string(req.GetBranch())
	if fp := req.GetFirstParentRef(); len(fp) > 0 {
		source = string(fp)
	}
	if source == "" {
		return nil, status.Error(codes.InvalidArgument, "empty first parent ref and branch name")
	}
	targetRef := string(req.GetTargetRef())
	if targetRef == "" {
		return nil, status.Error(codes.InvalidArgument, "empty target ref")
	}
	if !strings.HasPrefix(targetRef, "refs/merge-requests") {
		return nil, status.Error(codes.InvalidArgument, "invalid target ref")
	}
	if req.GetSourceSha() == "" {
		return nil, status.Error(codes.InvalidArgument, "empty source SHA")
	}
	ident, err := requestIdentity(req.GetUser(), req.GetTimestamp())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	ours, err := os.s.resolveRevision(ctx, name, source)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid merge source")
	}
	ours, err = os.peelToCommit(ctx, name, ours)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid merge source")
	}
	theirs, err := os.s.resolveRevision(ctx, name, req.GetSourceSha())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid merge source")
	}
	theirs, err = os.peelToCommit(ctx, name, theirs)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid merge source")
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

	mergeOID, conflicts, err := os.applyMerge(ctx, name, mergeSpec{
		ours: ours, theirs: theirs,
		parents: []model.OID{ours, theirs},
		author:  ident, committer: ident,
		message:     string(req.GetMessage()),
		oursLabel:   "HEAD",
		theirsLabel: shortRev(req.GetSourceSha()),
	})
	if err != nil {
		return nil, err
	}
	if conflicts != nil {
		return nil, withDetail(codes.FailedPrecondition, "merging commits: merge conflict",
			&gitalypb.UserMergeToRefError{
				Error: &gitalypb.UserMergeToRefError_MergeConflict{
					MergeConflict: &gitalypb.MergeConflictError{
						ConflictingFiles:     conflictFiles(conflicts),
						ConflictingCommitIds: []string{ours.String(), theirs.String()},
					},
				},
			})
	}
	if err := os.casRef(ctx, name, tl, oldTarget, mergeOID); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Could not update %s. Please refresh and try again", targetRef)
	}
	return &gitalypb.UserMergeToRefResponse{CommitId: mergeOID.String()}, nil
}

// UserSquash collapses start..end into one commit whose single parent is
// start. No reference is updated — the squashed commit OID is the answer.
func (os *operationService) UserSquash(ctx context.Context, req *gitalypb.UserSquashRequest) (*gitalypb.UserSquashResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	committer, err := requestIdentity(req.GetUser(), req.GetTimestamp())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetStartSha() == "" || req.GetEndSha() == "" {
		return nil, status.Error(codes.InvalidArgument, "empty StartSha or EndSha")
	}
	if len(req.GetCommitMessage()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty CommitMessage")
	}
	if req.GetAuthor() == nil || len(req.GetAuthor().GetName()) == 0 || len(req.GetAuthor().GetEmail()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty author name or email")
	}

	resolveSquash := func(hex string) (model.OID, error) {
		oid, err := os.peelToCommit(ctx, name, model.OID(hex))
		if err != nil {
			return "", withDetail(codes.InvalidArgument, "resolving revision",
				&gitalypb.UserSquashError{
					Error: &gitalypb.UserSquashError_ResolveRevision{
						ResolveRevision: &gitalypb.ResolveRevisionError{Revision: []byte(hex)},
					},
				})
		}
		return oid, nil
	}
	start, err := resolveSquash(req.GetStartSha())
	if err != nil {
		return nil, err
	}
	end, err := resolveSquash(req.GetEndSha())
	if err != nil {
		return nil, err
	}

	author := model.Identity{Name: string(req.GetAuthor().GetName()), Email: string(req.GetAuthor().GetEmail()), When: committer.When}
	if tz := req.GetAuthor().GetTimezone(); len(tz) > 0 {
		if loc, perr := time.Parse("-0700", string(tz)); perr == nil {
			author.When = author.When.In(loc.Location())
		}
	}

	squashOID, conflicts, err := os.applyMerge(ctx, name, mergeSpec{
		ours: start, theirs: end,
		parents:   []model.OID{start},
		author:    author,
		committer: committer,
		message:   string(req.GetCommitMessage()),
		oursLabel: "HEAD", theirsLabel: shortRev(req.GetEndSha()),
	})
	if err != nil {
		return nil, err
	}
	if conflicts != nil {
		return nil, withDetail(codes.FailedPrecondition, "squashing commits: merge conflict",
			&gitalypb.UserSquashError{
				Error: &gitalypb.UserSquashError_RebaseConflict{
					RebaseConflict: &gitalypb.MergeConflictError{
						ConflictingFiles:     conflictFiles(conflicts),
						ConflictingCommitIds: []string{start.String(), end.String()},
					},
				},
			})
	}
	return &gitalypb.UserSquashResponse{SquashSha: squashOID.String()}, nil
}

// cherryPickRevert is the shared body of UserCherryPick and UserRevert:
// both apply a single commit's change as a 3-way onto the start revision,
// difference only in which side plays base and theirs.
func (os *operationService) cherryPickRevert(ctx context.Context, req cherryRevertRequest) (*gitalypb.OperationBranchUpdate, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	if req.GetCommit() == nil || req.GetCommit().GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "empty Commit")
	}
	branch := string(req.GetBranchName())
	if !validBranchName(branch) {
		return nil, status.Error(codes.InvalidArgument, "empty BranchName")
	}
	if len(req.GetMessage()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty Message")
	}
	if req.GetStartRepository() != nil {
		return nil, status.Error(codes.InvalidArgument,
			"start_repository: cross-repository operations are not supported on this node")
	}
	committer, err := requestIdentity(req.GetUser(), req.GetTimestamp())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Start revision: the start branch (default the target branch) supplies
	// the tree the change applies onto; the CAS base is the target branch.
	startBranch := string(req.GetStartBranchName())
	if startBranch == "" {
		startBranch = branch
	}
	start, err := os.s.resolveRevision(ctx, name, "refs/heads/"+startBranch)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "resolve start ref: %v", err)
	}
	start, err = os.peelToCommit(ctx, name, start)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "resolve start ref: not a commit")
	}
	startRec, err := os.s.commitRecord(ctx, name, start)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read start commit: %v", err)
	}

	repoHadBranches, err := os.repoHasBranches(ctx, name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "has branches: %v", err)
	}

	branchCreated := false
	var oldrev model.OID // client-view expectation (advertised tip)
	advertised, raw, hasBranch := os.branchState(ctx, name, branch)
	if expected := req.GetExpectedOldOid(); expected != "" {
		if !isHexOID(expected) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid expected old object ID: %q", expected)
		}
		if model.OID(expected) != advertised && model.OID(expected) != raw {
			return nil, status.Error(codes.FailedPrecondition, "unexpected old value")
		}
		oldrev = model.OID(expected)
	} else if !hasBranch {
		branchCreated = true
		oldrev = model.OID("")
	} else {
		oldrev = advertised
		if oldrev.IsZero() {
			oldrev = raw
		}
	}

	changeOID, err := os.peelToCommit(ctx, name, model.OID(req.GetCommit().GetId()))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%s: commit lookup: commit not found: %q", req.opName(), req.GetCommit().GetId())
	}
	changeRec, err := os.s.commitRecord(ctx, name, changeOID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read commit: %v", err)
	}

	// The 3-way shape: cherry-pick merges the picked commit against its own
	// parent as base; revert flips sides — the parent's tree as theirs with
	// the reverted commit as base applies the inverse patch. A root commit
	// reverts against the empty tree.
	var baseCommit model.OID
	theirs := changeOID
	noBase := false
	theirsEmpty := false
	if req.isRevert() {
		baseCommit = changeOID
		if len(changeRec.Parents) > 0 {
			theirs = changeRec.Parents[0]
		} else {
			theirsEmpty = true
		}
	} else {
		if len(changeRec.Parents) > 0 {
			baseCommit = changeRec.Parents[0]
		} else {
			noBase = true
		}
	}

	// Identities: cherry-pick keeps the picked commit's author (overridable
	// via explicit fields); revert authors as the acting user.
	author := committer
	if !req.isRevert() {
		typ, raw, aerr := os.s.Objects.Raw(ctx, name, changeOID)
		if aerr != nil || typ != model.CommitType {
			return nil, status.Errorf(codes.Internal, "read picked commit: %v", aerr)
		}
		decoded, derr := model.DecodeCommit(raw)
		if derr != nil {
			return nil, status.Errorf(codes.Internal, "decode picked commit: %v", derr)
		}
		author = decoded.Author
		if n, e := string(req.GetCommitAuthorName()), string(req.GetCommitAuthorEmail()); n != "" && e != "" {
			author = model.Identity{Name: n, Email: e, When: committer.When}
		}
	}

	newRev, conflicts, err := os.applyMerge(ctx, name, mergeSpec{
		ours: start, theirs: theirs, baseCommit: baseCommit, noBase: noBase, theirsEmpty: theirsEmpty,
		parents: []model.OID{start},
		author:  author, committer: committer,
		message:   string(req.GetMessage()),
		oursLabel: "HEAD", theirsLabel: shortRev(req.GetCommit().GetId()),
	})
	if err != nil {
		return nil, err
	}
	if conflicts != nil {
		return nil, req.conflictDetail(conflicts)
	}
	newRec, err := os.s.commitRecord(ctx, name, newRev)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read result: %v", err)
	}
	if newRec.Tree == startRec.Tree {
		return nil, req.emptyDetail()
	}

	if req.GetDryRun() {
		newRev = start
	} else {
		if !branchCreated {
			anc, aerr := os.s.isAncestor(ctx, name, oldrev, newRev)
			if aerr != nil {
				return nil, status.Errorf(codes.Internal, "checking for ancestry: %v", aerr)
			}
			if !anc && raw != oldrev {
				anc, aerr = os.s.isAncestor(ctx, name, raw, newRev)
				if aerr != nil {
					return nil, status.Errorf(codes.Internal, "checking for ancestry: %v", aerr)
				}
			}
			if !anc {
				return nil, req.divergedDetail(oldrev, newRev)
			}
		}
		if err := os.casRef(ctx, name, branchTimeline(name, branch), raw, newRev); err != nil {
			return nil, err
		}
	}
	return &gitalypb.OperationBranchUpdate{
		CommitId:      newRev.String(),
		BranchCreated: branchCreated,
		RepoCreated:   !repoHadBranches,
	}, nil
}

// cherryRevertRequest abstracts the two single-commit flows' shared accessors.
type cherryRevertRequest interface {
	GetRepository() *gitalypb.Repository
	GetUser() *gitalypb.User
	GetCommit() *gitalypb.GitCommit
	GetBranchName() []byte
	GetMessage() []byte
	GetStartBranchName() []byte
	GetStartRepository() *gitalypb.Repository
	GetDryRun() bool
	GetTimestamp() *timestamppb.Timestamp
	GetExpectedOldOid() string
	GetCommitAuthorName() []byte
	GetCommitAuthorEmail() []byte
	isRevert() bool
	opName() string
	conflictDetail(paths []string) error
	emptyDetail() error
	divergedDetail(oldRev, newRev model.OID) error
}

type cherryPickReq struct {
	*gitalypb.UserCherryPickRequest
}

func (cherryPickReq) isRevert() bool { return false }
func (cherryPickReq) opName() string { return "cherry-pick" }
func (r cherryPickReq) conflictDetail(paths []string) error {
	return withDetail(codes.FailedPrecondition, "cherry pick: conflict",
		&gitalypb.UserCherryPickError{
			Error: &gitalypb.UserCherryPickError_CherryPickConflict{
				CherryPickConflict: &gitalypb.MergeConflictError{ConflictingFiles: conflictFiles(paths)},
			},
		})
}
func (cherryPickReq) emptyDetail() error {
	return withDetail(codes.FailedPrecondition, "cherry-pick: could not apply because the result was empty",
		&gitalypb.UserCherryPickError{
			Error: &gitalypb.UserCherryPickError_ChangesAlreadyApplied{
				ChangesAlreadyApplied: &gitalypb.ChangesAlreadyAppliedError{},
			},
		})
}
func (cherryPickReq) divergedDetail(oldRev, newRev model.OID) error {
	return withDetail(codes.FailedPrecondition, "cherry-pick: branch diverged",
		&gitalypb.UserCherryPickError{
			Error: &gitalypb.UserCherryPickError_TargetBranchDiverged{
				TargetBranchDiverged: &gitalypb.NotAncestorError{
					ParentRevision: []byte(oldRev.String()),
					ChildRevision:  []byte(newRev.String()),
				},
			},
		})
}

type revertReq struct{ *gitalypb.UserRevertRequest }

func (revertReq) isRevert() bool               { return true }
func (revertReq) opName() string               { return "revert" }
func (revertReq) GetCommitAuthorName() []byte  { return nil }
func (revertReq) GetCommitAuthorEmail() []byte { return nil }
func (r revertReq) conflictDetail(paths []string) error {
	return withDetail(codes.FailedPrecondition, "revert: there are conflicting files",
		&gitalypb.UserRevertError{
			Error: &gitalypb.UserRevertError_MergeConflict{
				MergeConflict: &gitalypb.MergeConflictError{ConflictingFiles: conflictFiles(paths)},
			},
		})
}
func (revertReq) emptyDetail() error {
	return withDetail(codes.FailedPrecondition, "revert: could not apply because the result was empty",
		&gitalypb.UserRevertError{
			Error: &gitalypb.UserRevertError_ChangesAlreadyApplied{
				ChangesAlreadyApplied: &gitalypb.ChangesAlreadyAppliedError{},
			},
		})
}
func (revertReq) divergedDetail(oldRev, newRev model.OID) error {
	return withDetail(codes.FailedPrecondition, "revert: branch diverged",
		&gitalypb.UserRevertError{
			Error: &gitalypb.UserRevertError_NotAncestor{
				NotAncestor: &gitalypb.NotAncestorError{
					ParentRevision: []byte(oldRev.String()),
					ChildRevision:  []byte(newRev.String()),
				},
			},
		})
}

func (os *operationService) UserCherryPick(ctx context.Context, req *gitalypb.UserCherryPickRequest) (*gitalypb.UserCherryPickResponse, error) {
	update, err := os.cherryPickRevert(ctx, cherryPickReq{req})
	if err != nil {
		return nil, err
	}
	return &gitalypb.UserCherryPickResponse{BranchUpdate: update}, nil
}

func (os *operationService) UserRevert(ctx context.Context, req *gitalypb.UserRevertRequest) (*gitalypb.UserRevertResponse, error) {
	update, err := os.cherryPickRevert(ctx, revertReq{req})
	if err != nil {
		return nil, err
	}
	return &gitalypb.UserRevertResponse{BranchUpdate: update}, nil
}

// shortRev abbreviates an OID for conflict labels.
func shortRev(hex string) string {
	if len(hex) > 7 {
		return hex[:7]
	}
	return hex
}
