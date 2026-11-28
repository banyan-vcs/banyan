package service

// The merge-request lifecycle (docs/design/merge-requests.md): the merge
// recipe itself (base computation, two-parent commit, forward CAS), the
// approval gate, the author-self-approval refusal, the moved-target race,
// and the reserve-land-finalize exclusivity two racing merges run through.
import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
)

// mergeRequestBranch commits files on top of head ("" = the repo's first commit on
// the branch), answering the new head.
func mergeRequestBranch(t *testing.T, s *Service, repo, branch string, head model.OID, files map[string]string, msg string) model.OID {
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
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"readme": "hi\n"}, "seed")
	feat := mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"feat.txt": "new\n"}, "feat work")
	more := mergeRequestBranch(t, s, "r", "main", base, map[string]string{"other.txt": "also\n"}, "main moves on")

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
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "base\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"f": "theirs\n"}, "feat edits f")
	mergeRequestBranch(t, s, "r", "main", base, map[string]string{"f": "ours\n"}, "main edits f")

	pv, err := s.PreviewMerge(ctx, "r", "feat", "main")
	require.NoError(t, err)
	require.False(t, len(pv.Conflicts) == 0, "same-file edit must conflict")
	require.Equal(t, []string{"f"}, pv.Conflicts)
	require.Zero(t, pv.Commit, "preview lands nothing")

	_, err = s.MergeBranches(ctx, "r", "feat", "main", "alice", "bob", "")
	var nm *ErrMergeRequestNotMergeable
	require.ErrorAs(t, err, &nm)
	require.Equal(t, []string{"f"}, nm.Conflicts)
	// The target timeline is untouched.
	require.NotZero(t, s.branchHeadOrDie(t, "r", "main"))
}

func TestMergeBranchesUnrelatedHistories(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	// Two root commits, no common ancestor: empty base, both trees merge.
	mergeRequestBranch(t, s, "r", "main", "", map[string]string{"a": "1\n"}, "root a")
	mergeRequestBranch(t, s, "r", "feat", "", map[string]string{"b": "2\n"}, "root b")
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

func TestMergeRequestLifecycleApprovalGate(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	rs, ok := s.Meta.(meta.BranchRuleStore)
	require.True(t, ok)
	require.NoError(t, rs.PutBranchRule(ctx, meta.BranchRule{Repo: "r", Branch: "main", Level: meta.RuleMergeRequestOnly, Approvals: 2}))

	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "add g", "", "alice")
	require.NoError(t, err)
	require.Equal(t, int64(1), mergeRequest.IID)

	// Author cannot self-approve.
	require.Error(t, s.ApproveMergeRequest(ctx, "r", 1, "alice", 1), "self-approval must bounce")

	// Below the count: merge refuses.
	_, err = s.MergeMergeRequest(ctx, "r", 1, "bob", "", 2)
	var nm *ErrMergeRequestNotMergeable
	require.ErrorAs(t, err, &nm)
	require.Contains(t, nm.Reason, "approval")

	// Two others approve; the gate opens.
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", 1, "bob", 3))
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", 1, "carol", 4))
	merged, err := s.MergeMergeRequest(ctx, "r", 1, "bob", "", 5)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateMerged, merged.State)
	require.NotZero(t, merged.MergeCommit)
	require.Equal(t, "bob", merged.MergedBy)

	// The detail view of a merged merge request is the record, not a live recompute.
	view, err := s.MergeRequestDetail(ctx, "r", 1)
	require.NoError(t, err)
	require.False(t, view.Mergeable)
	require.Equal(t, []string{"bob", "carol"}, view.Approvals)

	// Merging again bounces on state; reopening a merged merge request is not a thing.
	_, err = s.MergeMergeRequest(ctx, "r", 1, "bob", "", 6)
	require.Error(t, err)
}

func TestMergeRequestAuthorSelfApprovalRefused(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	_, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)
	require.Error(t, s.ApproveMergeRequest(ctx, "r", 1, "alice", 1), "self-approval must bounce")
	st := s.Meta.(meta.MergeRequestStore)
	users, err := st.ListApprovals(ctx, "r", 1)
	require.NoError(t, err)
	require.Empty(t, users, "the refused approval must not be recorded")
}

func TestMergeRequestMergeConflictRollsBackToOpen(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "base\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"f": "theirs\n"}, "feat")
	mergeRequestBranch(t, s, "r", "main", base, map[string]string{"f": "ours\n"}, "main")
	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)

	var nm *ErrMergeRequestNotMergeable
	_, merr := s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 1)
	require.ErrorAs(t, merr, &nm)
	require.Equal(t, []string{"f"}, nm.Conflicts)

	// The reservation rolled back: still open, still mergeable after the
	// conflict is resolved on the source side.
	got, err := s.MergeRequestByIIDDie(ctx, mergeRequest.IID)
	require.Equal(t, meta.MergeRequestStateOpen, got.State)
	mergeRequestBranch(t, s, "r", "feat", s.branchHeadOrDie(t, "r", "feat"), map[string]string{"f": "ours\n"}, "resolve to the target content")
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", mergeRequest.IID, "carol", 2))
	merged, err := s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 3)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateMerged, merged.State)
}

func (s *Service) MergeRequestByIIDDie(ctx context.Context, iid int64) (meta.MergeRequestInfo, error) {
	return s.Meta.(meta.MergeRequestStore).MergeRequestByIID(ctx, "r", iid)
}

func TestMergeRequestConcurrentMergeSingleWinner(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	_, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", 1, "carol", 1))

	// Racer one merges; racer two hits the state gate.
	first, err := s.MergeMergeRequest(ctx, "r", 1, "bob", "", 2)
	require.NoError(t, err)
	_, err = s.MergeMergeRequest(ctx, "r", 1, "carol", "", 3)
	require.Error(t, err, "the second merge must lose")
	require.Contains(t, err.Error(), "merged")

	// The ledger records the advertised tip; the raw timeline advanced under it.
	require.NotZero(t, s.branchHeadOrDie(t, "r", "main"))
	adv, err := s.Views.RootTip(context.Background(), "r", "main")
	require.NoError(t, err)
	require.Equal(t, model.OID(first.MergeCommit), adv)
}

func TestMergeRequestMissingBranches(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")

	_, err := s.OpenMergeRequest(ctx, "r", "nope", "main", "t", "", "alice")
	require.ErrorIs(t, err, ErrBranchMissing)
	_, err = s.OpenMergeRequest(ctx, "r", "main", "gone", "t", "", "alice")
	require.ErrorIs(t, err, ErrBranchMissing)
	// source == target is invalid before any branch lookup.
	_, err = s.OpenMergeRequest(ctx, "r", "main", "main", "t", "", "alice")
	require.Error(t, err)

	// The raw merge surface answers the same sentinel the lifecycle rides.
	_, err = s.MergeBranches(ctx, "r", "feat", "main", "a", "b", "")
	require.ErrorIs(t, err, ErrBranchMissing)
	_, err = s.MergeBranches(ctx, "r", "main", "feat", "a", "b", "")
	require.ErrorIs(t, err, ErrBranchMissing)
}

// TestMergeRequestCloseReopenLifecycle: close and reopen move the state without
// touching refs; every actor on a non-open merge request — merge, approval, the state
// hops themselves — bounces on the optimistic gate. Merged is terminal.
func TestMergeRequestCloseReopenLifecycle(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")

	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)

	// Close: state only — the refs do not move.
	closed, err := s.CloseMergeRequest(ctx, "r", mergeRequest.IID, 10)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateClosed, closed.State)
	require.NotZero(t, s.branchHeadOrDie(t, "r", "feat"))

	// A closed merge request accepts neither merge nor approval.
	_, err = s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 11)
	require.Error(t, err)
	require.Contains(t, err.Error(), "closed")
	require.Error(t, s.ApproveMergeRequest(ctx, "r", mergeRequest.IID, "bob", 12), "closed merge requests take no approvals")

	// Reopen restores open; the merge then lands through the ordinary gate.
	reopened, err := s.ReopenMergeRequest(ctx, "r", mergeRequest.IID, 13)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateOpen, reopened.State)
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", mergeRequest.IID, "carol", 14))
	merged, err := s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 15)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateMerged, merged.State)

	// Merged is terminal: neither close nor reopen applies.
	_, err = s.CloseMergeRequest(ctx, "r", mergeRequest.IID, 16)
	require.ErrorIs(t, err, meta.ErrMergeRequestState)
	_, err = s.ReopenMergeRequest(ctx, "r", mergeRequest.IID, 17)
	require.ErrorIs(t, err, meta.ErrMergeRequestState)
}

// TestMergeRequestApprovalWithdrawal: a withdrawn approval drops the count below the
// rule's bar — the gate is live, not a stamp collected once.
func TestMergeRequestApprovalWithdrawal(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	rs, ok := s.Meta.(meta.BranchRuleStore)
	require.True(t, ok)
	require.NoError(t, rs.PutBranchRule(ctx, meta.BranchRule{Repo: "r", Branch: "main", Level: meta.RuleMergeRequestOnly, Approvals: 1}))

	_, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", 1, "bob", 1))

	// Withdrawn: the count falls below 1 and the merge bounces again.
	require.NoError(t, s.UnapproveMergeRequest(ctx, "r", 1, "bob"))
	var nm *ErrMergeRequestNotMergeable
	_, err = s.MergeMergeRequest(ctx, "r", 1, "carol", "", 2)
	require.ErrorAs(t, err, &nm)
	require.Contains(t, nm.Reason, "approval")

	// Re-approved, it lands.
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", 1, "bob", 3))
	merged, err := s.MergeMergeRequest(ctx, "r", 1, "carol", "", 4)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateMerged, merged.State)
}

// TestMergeRequestMergeTakesCurrentTip: approvals stand across source advances (the
// honest record — they are not reset), and the merge takes the tip at merge
// time, not the head recorded at open.
func TestMergeRequestMergeTakesCurrentTip(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	feat1 := mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat step 1")

	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)
	require.Equal(t, feat1, model.OID(mergeRequest.SourceHead), "the record keeps the head at open time")

	// The source moves on after open; the detail answers the live tip.
	feat2 := mergeRequestBranch(t, s, "r", "feat", feat1, map[string]string{"h": "3\n"}, "feat step 2")
	view, err := s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	require.Equal(t, feat2, view.SourceTip)
	require.True(t, view.Mergeable)

	merged, err := s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 5)
	require.NoError(t, err)
	require.NotZero(t, merged.MergeCommit)
	// The raw two-parent commit (the advertised fold is the record): its
	// second parent is the current tip, the tree carries both steps.
	raw := s.branchHeadOrDie(t, "r", "main")
	ci, err := s.Objects.CommitInfo("r", raw)
	require.NoError(t, err)
	require.Len(t, ci.Parents, 2)
	require.Equal(t, feat2, ci.Parents[1], "the merge took the tip at merge time")
	files := treeFiles(t, s, "r", ci.Tree)
	require.Equal(t, "2\n", files["g"])
	require.Equal(t, "3\n", files["h"])
}

// TestMergeRequestDetailRuleEcho: the detail surfaces the target's rule (level + the
// required count) so the UI renders the bar without a second round-trip.
func TestMergeRequestDetailRuleEcho(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)

	view, err := s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	require.Empty(t, view.RuleLevel)
	require.Zero(t, view.RequiredApprovals)

	rs, ok := s.Meta.(meta.BranchRuleStore)
	require.True(t, ok)
	require.NoError(t, rs.PutBranchRule(ctx, meta.BranchRule{Repo: "r", Branch: "main", Level: meta.RuleProtected}))
	view, err = s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	require.Equal(t, meta.RuleProtected, view.RuleLevel)
	require.Zero(t, view.RequiredApprovals, "protection carries no approval bar")

	require.NoError(t, rs.PutBranchRule(ctx, meta.BranchRule{Repo: "r", Branch: "main", Level: meta.RuleMergeRequestOnly, Approvals: 2}))
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", mergeRequest.IID, "bob", 1))
	view, err = s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	require.Equal(t, meta.RuleMergeRequestOnly, view.RuleLevel)
	require.Equal(t, 2, view.RequiredApprovals)
	require.Equal(t, []string{"bob"}, view.Approvals)
}

// TestMergeBranchesSourceBehindTarget: a source with nothing unique still
// lands an explicit two-parent merge commit (git's --no-ff shape) — the
// tree is the target's, both heads are the parents.
func TestMergeBranchesSourceBehindTarget(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	require.NoError(t, s.Meta.EnsureTimeline(ctx, model.Timeline{Repo: "r", Anchor: model.RootAnchor, Name: "feat"}, base, ""))
	head2 := mergeRequestBranch(t, s, "r", "main", base, map[string]string{"g": "2\n"}, "main moves on")

	out, err := s.MergeBranches(ctx, "r", "feat", "main", "alice", "bob", "")
	require.NoError(t, err)
	require.Empty(t, out.Conflicts)
	require.Equal(t, base, out.Base)

	ci, err := s.Objects.CommitInfo("r", out.Commit)
	require.NoError(t, err)
	require.Len(t, ci.Parents, 2)
	require.Equal(t, head2, ci.Parents[0])
	require.Equal(t, base, ci.Parents[1])
	files := treeFiles(t, s, "r", ci.Tree)
	require.Equal(t, "2\n", files["g"])
}

// racingMeta lands a concurrent writer on the watched timeline between the
// merger's head read and its landing CAS — the exact race the CAS fails
// closed on (a sequential test cannot produce it; the wrapper injects it).
// The capability interfaces embed so the service's type assertions
// (MergeRequestStore, BranchRuleStore) keep finding a store through the wrapper.
type racingMeta struct {
	meta.MetaStore
	meta.MergeRequestStore
	meta.BranchRuleStore
	watch   model.Timeline
	intrude model.OID
	fired   bool
}

func (m *racingMeta) CASHead(ctx context.Context, tl model.Timeline, expected, next model.OID) error {
	if tl == m.watch && !m.fired {
		m.fired = true
		if err := m.MetaStore.CASHead(ctx, tl, expected, m.intrude); err != nil {
			return err
		}
	}
	return m.MetaStore.CASHead(ctx, tl, expected, next)
}

// TestMergeRequestMergeTargetMovedRollsBackAndRetries: a target that moves between
// the merge's head read and its CAS bounces as "moved — retry", the
// reservation rolls back to open, nothing lands, and the retry re-derives
// the same content over the moved head.
func TestMergeRequestMergeTargetMovedRollsBackAndRetries(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	feat := mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	// A commit that exists but is NOT on main — the racer lands it while the
	// merge is between its head read and its CAS (a disjoint file, so the
	// retry stays clean).
	racer := mergeRequestBranch(t, s, "r", "racer", base, map[string]string{"h": "3\n"}, "concurrent writer")

	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)

	real := s.Meta
	s.Meta = &racingMeta{MetaStore: real, MergeRequestStore: real.(meta.MergeRequestStore), BranchRuleStore: real.(meta.BranchRuleStore),
		watch: model.Timeline{Repo: "r", Anchor: model.RootAnchor, Name: "main"}, intrude: racer}

	var nm *ErrMergeRequestNotMergeable
	_, err = s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 1)
	require.ErrorAs(t, err, &nm)
	require.Contains(t, nm.Reason, "moved")

	// The reservation rolled back — still open, the racer owns the head.
	got, err := s.MergeRequestByIIDDie(ctx, mergeRequest.IID)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateOpen, got.State)
	require.Empty(t, got.MergeCommit)
	require.Equal(t, racer, s.branchHeadOrDie(t, "r", "main"))

	// The retry re-derives over the moved head: racer is parent 1 now.
	s.Meta = real
	merged, err := s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 2)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateMerged, merged.State)
	raw := s.branchHeadOrDie(t, "r", "main")
	ci, err := s.Objects.CommitInfo("r", raw)
	require.NoError(t, err)
	require.Len(t, ci.Parents, 2)
	require.Equal(t, racer, ci.Parents[0], "the retry merged onto the moved head")
	require.Equal(t, feat, ci.Parents[1])
	files := treeFiles(t, s, "r", ci.Tree)
	require.Equal(t, "3\n", files["h"], "the racer's content survives the retry")
}

// failingFinalize drops exactly one finalize transition on the floor — the
// store hiccup that opens the documented crash window (state=merged, the
// ledger never filled in).
type failingFinalize struct {
	meta.MetaStore
	meta.MergeRequestStore
	meta.BranchRuleStore
}

func (m *failingFinalize) TransitionMergeRequest(ctx context.Context, repo string, iid int64, from string, patch func(*meta.MergeRequestInfo)) (meta.MergeRequestInfo, error) {
	if from == meta.MergeRequestStateMerged {
		return meta.MergeRequestInfo{}, fmt.Errorf("store hiccup")
	}
	return m.MergeRequestStore.TransitionMergeRequest(ctx, repo, iid, from, patch)
}

// TestMergeRequestFinalizeFailureWindow pins the honest window (docs/design/
// merge-requests.md §边界): the ref moved and the merge stands, the error
// says both facts, and the ledger holds merged with an empty merge_commit
// — never a silent success, never a phantom landing.
func TestMergeRequestFinalizeFailureWindow(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)

	real := s.Meta
	s.Meta = &failingFinalize{MetaStore: real, MergeRequestStore: real.(meta.MergeRequestStore), BranchRuleStore: real.(meta.BranchRuleStore)}
	_, err = s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 1)
	s.Meta = real
	require.Error(t, err)
	require.Contains(t, err.Error(), "finalizing")

	// The merge itself stands on the ref; the ledger tells the truth about
	// what it does not know.
	raw := s.branchHeadOrDie(t, "r", "main")
	ci, err := s.Objects.CommitInfo("r", raw)
	require.NoError(t, err)
	require.Len(t, ci.Parents, 2, "the landing happened")
	got, err := s.MergeRequestByIIDDie(ctx, mergeRequest.IID)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateMerged, got.State)
	require.Empty(t, got.MergeCommit, "the window: merged with no recorded commit")
	require.Empty(t, got.MergedBy)
}

// TestMergeRequestDeletedSourceBranch: a source branch deleted after open (a push of
// :feat) — the detail answers honestly unmergeable rather than erroring
// (the record is the shareable truth), and the merge bounces with the
// branch sentinel, rolled back to open.
func TestMergeRequestDeletedSourceBranch(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"f": "1\n"}, "seed")
	featHead := mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"g": "2\n"}, "feat")
	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "t", "", "alice")
	require.NoError(t, err)

	// Deletion is the zero-head CAS (the receive-pack form).
	require.NoError(t, s.Meta.CASHead(ctx, model.Timeline{Repo: "r", Anchor: model.RootAnchor, Name: "feat"}, featHead, ""))

	view, err := s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err, "the detail must not error on a deleted source")
	require.Equal(t, meta.MergeRequestStateOpen, view.State)
	require.Zero(t, view.SourceTip)
	require.False(t, view.Mergeable)

	_, err = s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 1)
	require.ErrorIs(t, err, ErrBranchMissing)
	got, err := s.MergeRequestByIIDDie(ctx, mergeRequest.IID)
	require.NoError(t, err)
	require.Equal(t, meta.MergeRequestStateOpen, got.State, "the failed merge rolls back")
}

// TestMergeRequestConcurrentMergeVsClose: the optimistic gate gives the merge and
// the close exactly one winner, and the ledger agrees with the refs — a
// merged winner moved the head and filled the record, a closed winner
// touched nothing.
func TestMergeRequestConcurrentMergeVsClose(t *testing.T) {
	for round := 0; round < 3; round++ {
		s := newService(t)
		ctx := context.Background()
		repo := fmt.Sprintf("r%d", round)
		base := mergeRequestBranch(t, s, repo, "main", "", map[string]string{"f": "1\n"}, "seed")
		mergeRequestBranch(t, s, repo, "feat", base, map[string]string{"g": "2\n"}, "feat")
		mergeRequest, err := s.OpenMergeRequest(ctx, repo, "feat", "main", "t", "", "alice")
		require.NoError(t, err)
		before := s.branchHeadOrDie(t, repo, "main")

		var wg sync.WaitGroup
		var mergeErr, closeErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, mergeErr = s.MergeMergeRequest(ctx, repo, mergeRequest.IID, "bob", "", 1) }()
		go func() { defer wg.Done(); _, closeErr = s.CloseMergeRequest(ctx, repo, mergeRequest.IID, 2) }()
		wg.Wait()

		require.True(t, (mergeErr == nil) != (closeErr == nil),
			"exactly one winner: merge=%v close=%v", mergeErr, closeErr)

		st := s.Meta.(meta.MergeRequestStore)
		got, err := st.MergeRequestByIID(ctx, repo, mergeRequest.IID)
		require.NoError(t, err)
		if mergeErr == nil {
			require.Equal(t, meta.MergeRequestStateMerged, got.State)
			require.NotEmpty(t, got.MergeCommit)
			require.NotEqual(t, before, s.branchHeadOrDie(t, repo, "main"), "the merge moved the ref")
		} else {
			require.Equal(t, meta.MergeRequestStateClosed, got.State)
			require.Equal(t, before, s.branchHeadOrDie(t, repo, "main"), "a closed winner touches no ref")
		}
	}
}
