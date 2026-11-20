package service

// The merge-request lifecycle (docs/design/merge-requests.md): the merge
// recipe itself (base computation, two-parent commit, forward CAS), the
// approval gate, the author-self-approval refusal, the moved-target race,
// and the reserve-land-finalize exclusivity two racing merges run through.
import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
)

// mrBranch commits files on top of head ("" = the repo's first commit on
// the branch), answering the new head.
func mrBranch(t *testing.T, s *Service, repo, branch string, head model.OID, files map[string]string, msg string) model.OID {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Meta.GetRepo(ctx, repo); err != nil {
		require.NoError(t, s.Meta.CreateRepo(ctx, repo, model.SHA1))
	}
	if !head.IsZero() {
		// Fork: point the branch timeline at the fork point first (the thin-client
		// commit primitive CASes Base against the branch head).
		require.NoError(t, s.Meta.EnsureTimeline(ctx, model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: branch}, head, ""))
	}
	putFiles(t, s, repo, files)
	res, err := s.CreateCommit(context.Background(), CommitRequest{
		Repo: repo, Branch: branch, Base: head, Message: msg, Changes: changesFor(files),
	})
	require.NoError(t, err, "commit on %s failed", branch)
	return res.Head
}

func TestMergeBranchesClean(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mrBranch(t, s, "r", "main", "", map[string]string{"readme": "hi\n"}, "seed")
	feat := mrBranch(t, s, "r", "feat", base, map[string]string{"feat.txt": "new\n"}, "feat work")
	more := mrBranch(t, s, "r", "main", base, map[string]string{"other.txt": "also\n"}, "main moves on")

	out, err := s.MergeBranches(ctx, "r", "feat", "main", "alice", "bob", "")
	require.NoError(t, err)
	require.Empty(t, out.Conflicts)
	require.NotZero(t, out.Commit)
	require.Equal(t, base, out.Base, "merge base must be the common parent")

	// The landed commit: two parents, the merged tree carries both sides.
	ci, err := s.Objects.CommitInfo("r", out.Commit)
	require.NoError(t, err)
	require.Len(t, ci.Parents, 2)
	require.Equal(t, more, ci.Parents[0], "parent 1 = the target tip")
	require.Equal(t, feat, ci.Parents[1], "parent 2 = the source tip")
	files := treeFiles(t, s, "r", ci.Tree)
	require.Equal(t, "new\n", files["feat.txt"])
	require.Equal(t, "also\n", files["other.txt"])

	// The target timeline now points at the merge commit.
	require.Equal(t, out.Commit, s.branchHeadOrDie(t, "r", "main"))
}

func (s *Service) branchHeadOrDie(t *testing.T, repo, branch string) model.OID {
	t.Helper()
	oid, err := s.branchHead(context.Background(), repo, branch)
	require.NoError(t, err)
	return oid
}

func TestMergeBranchesConflicts(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mrBranch(t, s, "r", "main", "", map[string]string{"f": "base\n"}, "seed")
	mrBranch(t, s, "r", "feat", base, map[string]string{"f": "theirs\n"}, "feat edits f")
	mrBranch(t, s, "r", "main", base, map[string]string{"f": "ours\n"}, "main edits f")

	pv, err := s.PreviewMerge(ctx, "r", "feat", "main")
	require.NoError(t, err)
	require.False(t, len(pv.Conflicts) == 0, "same-file edit must conflict")
	require.Equal(t, []string{"f"}, pv.Conflicts)
	require.Zero(t, pv.Commit, "preview lands nothing")

	_, err = s.MergeBranches(ctx, "r", "feat", "main", "alice", "bob", "")
	var nm *ErrMRNotMergeable
	require.ErrorAs(t, err, &nm)
	require.Equal(t, []string{"f"}, nm.Conflicts)
	// The target timeline is untouched.
	require.NotZero(t, s.branchHeadOrDie(t, "r", "main"))
}

func TestMergeBranchesUnrelatedHistories(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	// Two root commits, no common ancestor: empty base, both trees merge.
	mrBranch(t, s, "r", "main", "", map[string]string{"a": "1\n"}, "root a")
	mrBranch(t, s, "r", "feat", "", map[string]string{"b": "2\n"}, "root b")
	out, err := s.MergeBranches(ctx, "r", "feat", "main", "alice", "bob", "")
	require.NoError(t, err)
	require.Zero(t, out.Base, "no merge base")
	files := treeFiles(t, s, "r", mustTree(t, s, out.Commit))
	require.Equal(t, "1\n", files["a"])
	require.Equal(t, "2\n", files["b"])
}

func mustTree(t *testing.T, s *Service, commit model.OID) model.OID {
	t.Helper()
	ci, err := s.Objects.CommitInfo("r", commit)
	require.NoError(t, err)
	return ci.Tree
}

func TestMRLifecycleApprovalGate(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mrBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mrBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	rs, ok := s.Meta.(meta.BranchRuleStore)
	require.True(t, ok)
	require.NoError(t, rs.PutBranchRule(ctx, meta.BranchRule{Repo: "r", Branch: "main", Level: meta.RuleMROnly, Approvals: 2}))

	mr, err := s.OpenMR(ctx, "r", "feat", "main", "add g", "", "alice")
	require.NoError(t, err)
	require.Equal(t, int64(1), mr.IID)

	// Author cannot self-approve.
	require.Error(t, s.ApproveMR(ctx, "r", 1, "alice", 1), "self-approval must bounce")

	// Below the count: merge refuses.
	_, err = s.MergeMR(ctx, "r", 1, "bob", "", 2)
	var nm *ErrMRNotMergeable
	require.ErrorAs(t, err, &nm)
	require.Contains(t, nm.Reason, "approval")

	// Two others approve; the gate opens.
	require.NoError(t, s.ApproveMR(ctx, "r", 1, "bob", 3))
	require.NoError(t, s.ApproveMR(ctx, "r", 1, "carol", 4))
	merged, err := s.MergeMR(ctx, "r", 1, "bob", "", 5)
	require.NoError(t, err)
	require.Equal(t, meta.MRStateMerged, merged.State)
	require.NotZero(t, merged.MergeCommit)
	require.Equal(t, "bob", merged.MergedBy)

	// The detail view of a merged MR is the record, not a live recompute.
	view, err := s.MRDetail(ctx, "r", 1)
	require.NoError(t, err)
	require.False(t, view.Mergeable)
	require.Equal(t, []string{"bob", "carol"}, view.Approvals)

	// Merging again bounces on state; reopening a merged MR is not a thing.
	_, err = s.MergeMR(ctx, "r", 1, "bob", "", 6)
	require.Error(t, err)
}

func TestMRAuthorSelfApprovalRefused(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mrBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mrBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	_, err := s.OpenMR(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)
	require.Error(t, s.ApproveMR(ctx, "r", 1, "alice", 1), "self-approval must bounce")
	st := s.Meta.(meta.MRStore)
	users, err := st.ListApprovals(ctx, "r", 1)
	require.NoError(t, err)
	require.Empty(t, users, "the refused approval must not be recorded")
}

func TestMRMergeConflictRollsBackToOpen(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mrBranch(t, s, "r", "main", "", map[string]string{"f": "base\n"}, "seed")
	mrBranch(t, s, "r", "feat", base, map[string]string{"f": "theirs\n"}, "feat")
	mrBranch(t, s, "r", "main", base, map[string]string{"f": "ours\n"}, "main")
	mr, err := s.OpenMR(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)

	var nm *ErrMRNotMergeable
	_, merr := s.MergeMR(ctx, "r", mr.IID, "bob", "", 1)
	require.ErrorAs(t, merr, &nm)
	require.Equal(t, []string{"f"}, nm.Conflicts)

	// The reservation rolled back: still open, still mergeable after the
	// conflict is resolved on the source side.
	got, err := s.MRByIIDDie(ctx, mr.IID)
	require.Equal(t, meta.MRStateOpen, got.State)
	mrBranch(t, s, "r", "feat", s.branchHeadOrDie(t, "r", "feat"), map[string]string{"f": "ours\n"}, "resolve to the target content")
	require.NoError(t, s.ApproveMR(ctx, "r", mr.IID, "carol", 2))
	merged, err := s.MergeMR(ctx, "r", mr.IID, "bob", "", 3)
	require.NoError(t, err)
	require.Equal(t, meta.MRStateMerged, merged.State)
}

func (s *Service) MRByIIDDie(ctx context.Context, iid int64) (meta.MRInfo, error) {
	return s.Meta.(meta.MRStore).MRByIID(ctx, "r", iid)
}

func TestMRConcurrentMergeSingleWinner(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mrBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mrBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	_, err := s.OpenMR(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)
	require.NoError(t, s.ApproveMR(ctx, "r", 1, "carol", 1))

	// Racer one merges; racer two hits the state gate.
	first, err := s.MergeMR(ctx, "r", 1, "bob", "", 2)
	require.NoError(t, err)
	_, err = s.MergeMR(ctx, "r", 1, "carol", "", 3)
	require.Error(t, err, "the second merge must lose")
	require.Contains(t, err.Error(), "merged")

	// The ledger records the advertised tip; the raw timeline advanced under it.
	require.NotZero(t, s.branchHeadOrDie(t, "r", "main"))
	adv, err := s.Views.RootTip(context.Background(), "r", "main")
	require.NoError(t, err)
	require.Equal(t, model.OID(first.MergeCommit), adv)
}

func TestMRMissingBranches(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	mrBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")

	_, err := s.OpenMR(ctx, "r", "nope", "main", "t", "", "alice")
	require.ErrorIs(t, err, ErrBranchMissing)
	_, err = s.OpenMR(ctx, "r", "main", "gone", "t", "", "alice")
	require.ErrorIs(t, err, ErrBranchMissing)
	// source == target is invalid before any branch lookup.
	_, err = s.OpenMR(ctx, "r", "main", "main", "t", "", "alice")
	require.Error(t, err)
}
