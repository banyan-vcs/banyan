package service

// The review mechanics over the comment ledger (docs/design/merge-requests.md
// §review): anchored comments stamp the live source tip server-side, the
// verdict approve is approval-plus-narrative, request-changes blocks the
// merge until the same reviewer reviews again, and the author's rows never
// enter the lock.
import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// mergeRequestCommentOn is AddMergeRequestComment with the happy-path defaults spelled out.
func mergeRequestCommentOn(t *testing.T, s *Service, repo string, iid int64, author, body string, line int) meta.MergeRequestComment {
	t.Helper()
	c, err := s.AddMergeRequestComment(context.Background(), repo, iid, author, body, "", "", line, 0)
	require.NoError(t, err)
	return c
}

func TestMergeRequestReviewLifecycle(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"readme": "hi\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"feat.go": "package feat\n"}, "feat work")
	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "title", "desc", "alice")
	require.NoError(t, err)

	// --- anchored comment: the server stamps the live source tip ---
	c1, err := s.AddMergeRequestComment(ctx, "r", mergeRequest.IID, "bob", "the package clause", "feat.go", "new", 1, 0)
	require.NoError(t, err)
	require.NotZero(t, c1.ID)
	assert.Equal(t, "feat.go", c1.Path)
	require.NotEmpty(t, c1.AnchorOID, "anchor must be stamped server-side")
	tip, err := s.branchHead(ctx, "r", "feat")
	require.NoError(t, err)
	assert.Equal(t, string(tip), c1.AnchorOID, "anchor = the live source tip")

	// A client-claimed anchor is impossible by construction: AddMergeRequestComment
	// takes no anchor parameter at all.

	// --- replies and resolution ---
	reply, err := s.AddMergeRequestComment(ctx, "r", mergeRequest.IID, "alice", "renamed in the follow-up", "", "", 0, c1.ID)
	require.NoError(t, err)
	assert.Equal(t, c1.ID, reply.ParentID)
	require.NoError(t, s.ResolveMergeRequestComment(ctx, "r", mergeRequest.IID, c1.ID, "alice", true))
	list, err := s.ListMergeRequestComments(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.True(t, list[0].Resolved)
	assert.Equal(t, "alice", list[0].ResolvedBy)
	require.Error(t, s.ResolveMergeRequestComment(ctx, "r", mergeRequest.IID, reply.ID, "alice", true), "replies never resolve")

	// --- verdicts: approve counts, author refused on real verdicts ---
	_, err = s.SubmitMergeRequestReview(ctx, "r", mergeRequest.IID, "alice", meta.VerdictApprove, "looks great")
	require.Error(t, err, "author cannot approve via review")
	_, err = s.SubmitMergeRequestReview(ctx, "r", mergeRequest.IID, "alice", meta.VerdictRequestChanges, "")
	require.Error(t, err, "author cannot request changes")
	rv, err := s.SubmitMergeRequestReview(ctx, "r", mergeRequest.IID, "alice", meta.VerdictComment, "note to self")
	require.NoError(t, err, "the comment verdict is the author's to use")
	assert.Equal(t, meta.VerdictComment, rv.Verdict)

	rv, err = s.SubmitMergeRequestReview(ctx, "r", mergeRequest.IID, "bob", meta.VerdictApprove, "lgtm")
	require.NoError(t, err)
	view, err := s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	assert.Empty(t, view.Blocking)
	assert.Contains(t, view.Approvals, "bob", "an approve review records the approval")

	// --- request-changes blocks the merge ---
	_, err = s.SubmitMergeRequestReview(ctx, "r", mergeRequest.IID, "carol", meta.VerdictRequestChanges, "rename the package")
	require.NoError(t, err)
	view, err = s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	assert.Equal(t, []string{"carol"}, view.Blocking)
	_, err = s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request-changes outstanding from carol")

	// The author pushing does NOT dismiss the verdict (only re-review does).
	mergeRequestBranch(t, s, "r", "feat", tip, map[string]string{"feat.go": "package features\n"}, "rename")
	view, err = s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	assert.Equal(t, []string{"carol"}, view.Blocking, "a source advance never dismisses a verdict")

	// --- re-review clears: carol approves, the merge lands ---
	_, err = s.SubmitMergeRequestReview(ctx, "r", mergeRequest.IID, "carol", meta.VerdictApprove, "renamed, lgtm")
	require.NoError(t, err)
	view, err = s.MergeRequestDetail(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	assert.Empty(t, view.Blocking)
	merged, err := s.MergeMergeRequest(ctx, "r", mergeRequest.IID, "bob", "", 2)
	require.NoError(t, err)
	assert.Equal(t, meta.MergeRequestStateMerged, merged.State)

	// The discussion survives the merge — the timeline is the record.
	list, err = s.ListMergeRequestComments(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	assert.NotEmpty(t, list)
	_, err = s.SubmitMergeRequestReview(ctx, "r", mergeRequest.IID, "bob", meta.VerdictApprove, "after the fact")
	require.Error(t, err, "reviews need an open request")
	c2, err := s.AddMergeRequestComment(ctx, "r", mergeRequest.IID, "bob", "post-merge remark", "", "", 0, 0)
	require.NoError(t, err, "plain comments ride any state")
	require.Greater(t, c2.ID, rv.ID)
	_, err = s.AddMergeRequestComment(ctx, "r", mergeRequest.IID, "bob", "anchored too late", "x.go", "new", 3, 0)
	require.Error(t, err, "line anchors belong to open requests")
}

func TestMergeRequestDiscussionShape(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	base := mergeRequestBranch(t, s, "r", "main", "", map[string]string{"a": "1\n"}, "seed")
	mergeRequestBranch(t, s, "r", "feat", base, map[string]string{"b": "2\n"}, "work")
	mergeRequest, err := s.OpenMergeRequest(ctx, "r", "feat", "main", "title", "", "alice")
	require.NoError(t, err)
	require.NoError(t, s.ApproveMergeRequest(ctx, "r", mergeRequest.IID, "bob", 77))
	mergeRequestCommentOn(t, s, "r", mergeRequest.IID, "carol", "remark", 0)

	comments, approvals, err := s.MergeRequestDiscussion(ctx, "r", mergeRequest.IID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Len(t, approvals, 1)
	assert.Equal(t, "bob", approvals[0].User)
	assert.Equal(t, int64(77), approvals[0].At, "the timeline needs the timestamps")
}
