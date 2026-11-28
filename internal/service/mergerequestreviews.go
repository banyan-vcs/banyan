// mergerequestreviews.go — the discussion lifecycle over the comment ledger
// (docs/design/merge-requests.md §review): comments and replies (anchored
// ones stamp the anchor server-side — the live source tip the caller was
// looking at, never a client-claimed OID), review submissions (the verdict
// approve also records an approval, one gesture two effects), and the
// request-changes lock: a reviewer's LATEST review blocks the merge until
// that same reviewer reviews again without it. Source advances never
// dismiss a verdict — approvals and verdicts both survive pushes; the
// reviewer's re-review is the only key.
package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// AddMergeRequestComment stores one comment or reply. An anchored comment (line > 0)
// gets its AnchorOID stamped here from the source branch's live tip — the
// anchor is what the diff was computed against, and only the server knows
// that truthfully. Comments ride any state (merged and closed requests keep
// the discussion); anchors need a live source branch.
func (s *Service) AddMergeRequestComment(ctx context.Context, repo string, iid int64, author, body, path, side string, line int, parentID int64) (meta.MergeRequestComment, error) {
	st, ok := s.Meta.(meta.MergeRequestCommentStore)
	if !ok {
		return meta.MergeRequestComment{}, invalid("this deployment owns no merge-request comment store")
	}
	mergeRequests, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return meta.MergeRequestComment{}, invalid("this deployment owns no merge-request store")
	}
	mergeRequest, err := mergeRequests.MergeRequestByIID(ctx, repo, iid)
	if err != nil {
		return meta.MergeRequestComment{}, err
	}
	if author == "" {
		return meta.MergeRequestComment{}, invalid("author required")
	}
	c := meta.MergeRequestComment{
		Repo: repo, IID: iid, ParentID: parentID, Author: author,
		Kind: meta.CommentKindComment, Body: body,
		Path: path, Side: side, Line: line,
		CreatedAt: time.Now().Unix(),
	}
	if line > 0 || path != "" || side != "" {
		if mergeRequest.State != meta.MergeRequestStateOpen {
			return meta.MergeRequestComment{}, invalid("line anchors belong to open requests — the stored diff is the record")
		}
		tip, err := s.branchHead(ctx, repo, mergeRequest.Source)
		if err != nil {
			return meta.MergeRequestComment{}, err
		}
		c.AnchorOID = string(tip)
	}
	return st.CreateMergeRequestComment(ctx, c)
}

// ListMergeRequestComments answers the discussion, ID-ascending (the timeline order).
func (s *Service) ListMergeRequestComments(ctx context.Context, repo string, iid int64) ([]meta.MergeRequestComment, error) {
	st, ok := s.Meta.(meta.MergeRequestCommentStore)
	if !ok {
		return nil, invalid("this deployment owns no merge-request comment store")
	}
	return st.ListMergeRequestComments(ctx, repo, iid)
}

// ResolveMergeRequestComment flips a thread head's resolved flag (replies refuse at
// the store; the caller's rank gate lives at the JSON face).
func (s *Service) ResolveMergeRequestComment(ctx context.Context, repo string, iid, id int64, by string, resolved bool) error {
	st, ok := s.Meta.(meta.MergeRequestCommentStore)
	if !ok {
		return invalid("this deployment owns no merge-request comment store")
	}
	return st.SetMergeRequestCommentResolved(ctx, repo, iid, id, resolved, by)
}

// SubmitMergeRequestReview files a review: verdict approve | request-changes |
// comment, optional body, merge request must be open. Approve also records an approval
// (the same rank the plain approve endpoint demands — not the author);
// request-changes refuses the author too (a self-block is nonsense); the
// comment verdict is open to the author as well.
func (s *Service) SubmitMergeRequestReview(ctx context.Context, repo string, iid int64, author, verdict, body string) (meta.MergeRequestComment, error) {
	cs, ok := s.Meta.(meta.MergeRequestCommentStore)
	if !ok {
		return meta.MergeRequestComment{}, invalid("this deployment owns no merge-request comment store")
	}
	mergeRequests, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return meta.MergeRequestComment{}, invalid("this deployment owns no merge-request store")
	}
	if author == "" {
		return meta.MergeRequestComment{}, invalid("author required")
	}
	mergeRequest, err := mergeRequests.MergeRequestByIID(ctx, repo, iid)
	if err != nil {
		return meta.MergeRequestComment{}, err
	}
	if mergeRequest.State != meta.MergeRequestStateOpen {
		return meta.MergeRequestComment{}, invalid("merge request !%d is %s", iid, mergeRequest.State)
	}
	switch verdict {
	case meta.VerdictApprove, meta.VerdictRequestChanges:
		if mergeRequest.Author == author {
			return meta.MergeRequestComment{}, invalid("the merge request author cannot review their own verdict")
		}
	case meta.VerdictComment:
	default:
		return meta.MergeRequestComment{}, invalid("verdict must be approve|request-changes|comment")
	}
	c, err := cs.CreateMergeRequestComment(ctx, meta.MergeRequestComment{
		Repo: repo, IID: iid, Author: author, Kind: meta.CommentKindReview,
		Verdict: verdict, Body: body, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return meta.MergeRequestComment{}, err
	}
	if verdict == meta.VerdictApprove {
		// One gesture, two effects: the approval count and the narrative.
		if err := mergeRequests.AddApproval(ctx, repo, iid, author, c.CreatedAt); err != nil {
			return meta.MergeRequestComment{}, err
		}
	}
	return c, nil
}

// mergeRequestBlocking answers the reviewers whose LATEST review is request-changes —
// the merge's second lock. The author's rows never enter (the submit path
// refuses them, the belt reads the same condition).
func (s *Service) mergeRequestBlocking(ctx context.Context, cs meta.MergeRequestCommentStore, repo string, iid int64, author string) ([]string, error) {
	all, err := cs.ListMergeRequestComments(ctx, repo, iid)
	if err != nil {
		return nil, err
	}
	latest := map[string]string{} // reviewer → verdict of their newest review
	for _, c := range all {
		if c.Kind == meta.CommentKindReview && c.Author != author {
			latest[c.Author] = c.Verdict // ascending IDs: the last write wins
		}
	}
	blocking := []string{}
	for who, verdict := range latest {
		if verdict == meta.VerdictRequestChanges {
			blocking = append(blocking, who)
		}
	}
	sort.Strings(blocking)
	return blocking, nil
}

// MergeRequestDiscussion answers the timeline's full data set: the ledger plus the
// approval events (a timestamped approval is a timeline row, not just a
// count). One call, one query key on the consuming page.
func (s *Service) MergeRequestDiscussion(ctx context.Context, repo string, iid int64) ([]meta.MergeRequestComment, []meta.MergeRequestApproval, error) {
	cs, ok := s.Meta.(meta.MergeRequestCommentStore)
	if !ok {
		return nil, nil, invalid("this deployment owns no merge-request comment store")
	}
	mergeRequests, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return nil, nil, invalid("this deployment owns no merge-request store")
	}
	comments, err := cs.ListMergeRequestComments(ctx, repo, iid)
	if err != nil {
		return nil, nil, err
	}
	approvals, err := mergeRequests.ListApprovalDetails(ctx, repo, iid)
	if err != nil {
		return nil, nil, err
	}
	return comments, approvals, nil
}

// mergeRequestBlockingReason renders the lock's failure text ("" = clear).
func mergeRequestBlockingReason(blocking []string) string {
	if len(blocking) == 0 {
		return ""
	}
	return "request-changes outstanding from " + strings.Join(blocking, ", ")
}
