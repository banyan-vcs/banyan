// mrreview.go — the discussion lifecycle over the comment ledger
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

// AddMRComment stores one comment or reply. An anchored comment (line > 0)
// gets its AnchorOID stamped here from the source branch's live tip — the
// anchor is what the diff was computed against, and only the server knows
// that truthfully. Comments ride any state (merged and closed requests keep
// the discussion); anchors need a live source branch.
func (s *Service) AddMRComment(ctx context.Context, repo string, iid int64, author, body, path, side string, line int, parentID int64) (meta.MRComment, error) {
	st, ok := s.Meta.(meta.MRCommentStore)
	if !ok {
		return meta.MRComment{}, invalid("this deployment owns no merge-request comment store")
	}
	mrs, ok := s.Meta.(meta.MRStore)
	if !ok {
		return meta.MRComment{}, invalid("this deployment owns no merge-request store")
	}
	mr, err := mrs.MRByIID(ctx, repo, iid)
	if err != nil {
		return meta.MRComment{}, err
	}
	if author == "" {
		return meta.MRComment{}, invalid("author required")
	}
	c := meta.MRComment{
		Repo: repo, IID: iid, ParentID: parentID, Author: author,
		Kind: meta.CommentKindComment, Body: body,
		Path: path, Side: side, Line: line,
		CreatedAt: time.Now().Unix(),
	}
	if line > 0 || path != "" || side != "" {
		if mr.State != meta.MRStateOpen {
			return meta.MRComment{}, invalid("line anchors belong to open requests — the stored diff is the record")
		}
		tip, err := s.branchHead(ctx, repo, mr.Source)
		if err != nil {
			return meta.MRComment{}, err
		}
		c.AnchorOID = string(tip)
	}
	return st.CreateMRComment(ctx, c)
}

// ListMRComments answers the discussion, ID-ascending (the timeline order).
func (s *Service) ListMRComments(ctx context.Context, repo string, iid int64) ([]meta.MRComment, error) {
	st, ok := s.Meta.(meta.MRCommentStore)
	if !ok {
		return nil, invalid("this deployment owns no merge-request comment store")
	}
	return st.ListMRComments(ctx, repo, iid)
}

// ResolveMRComment flips a thread head's resolved flag (replies refuse at
// the store; the caller's rank gate lives at the JSON face).
func (s *Service) ResolveMRComment(ctx context.Context, repo string, iid, id int64, by string, resolved bool) error {
	st, ok := s.Meta.(meta.MRCommentStore)
	if !ok {
		return invalid("this deployment owns no merge-request comment store")
	}
	return st.SetMRCommentResolved(ctx, repo, iid, id, resolved, by)
}

// SubmitMRReview files a review: verdict approve | request-changes |
// comment, optional body, MR must be open. Approve also records an approval
// (the same rank the plain approve endpoint demands — not the author);
// request-changes refuses the author too (a self-block is nonsense); the
// comment verdict is open to the author as well.
func (s *Service) SubmitMRReview(ctx context.Context, repo string, iid int64, author, verdict, body string) (meta.MRComment, error) {
	cs, ok := s.Meta.(meta.MRCommentStore)
	if !ok {
		return meta.MRComment{}, invalid("this deployment owns no merge-request comment store")
	}
	mrs, ok := s.Meta.(meta.MRStore)
	if !ok {
		return meta.MRComment{}, invalid("this deployment owns no merge-request store")
	}
	if author == "" {
		return meta.MRComment{}, invalid("author required")
	}
	mr, err := mrs.MRByIID(ctx, repo, iid)
	if err != nil {
		return meta.MRComment{}, err
	}
	if mr.State != meta.MRStateOpen {
		return meta.MRComment{}, invalid("merge request !%d is %s", iid, mr.State)
	}
	switch verdict {
	case meta.VerdictApprove, meta.VerdictRequestChanges:
		if mr.Author == author {
			return meta.MRComment{}, invalid("the merge request author cannot review their own verdict")
		}
	case meta.VerdictComment:
	default:
		return meta.MRComment{}, invalid("verdict must be approve|request-changes|comment")
	}
	c, err := cs.CreateMRComment(ctx, meta.MRComment{
		Repo: repo, IID: iid, Author: author, Kind: meta.CommentKindReview,
		Verdict: verdict, Body: body, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return meta.MRComment{}, err
	}
	if verdict == meta.VerdictApprove {
		// One gesture, two effects: the approval count and the narrative.
		if err := mrs.AddApproval(ctx, repo, iid, author, c.CreatedAt); err != nil {
			return meta.MRComment{}, err
		}
	}
	return c, nil
}

// mrBlocking answers the reviewers whose LATEST review is request-changes —
// the merge's second lock. The author's rows never enter (the submit path
// refuses them, the belt reads the same condition).
func (s *Service) mrBlocking(ctx context.Context, cs meta.MRCommentStore, repo string, iid int64, author string) ([]string, error) {
	all, err := cs.ListMRComments(ctx, repo, iid)
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

// MRDiscussion answers the timeline's full data set: the ledger plus the
// approval events (a timestamped approval is a timeline row, not just a
// count). One call, one query key on the consuming page.
func (s *Service) MRDiscussion(ctx context.Context, repo string, iid int64) ([]meta.MRComment, []meta.MRApproval, error) {
	cs, ok := s.Meta.(meta.MRCommentStore)
	if !ok {
		return nil, nil, invalid("this deployment owns no merge-request comment store")
	}
	mrs, ok := s.Meta.(meta.MRStore)
	if !ok {
		return nil, nil, invalid("this deployment owns no merge-request store")
	}
	comments, err := cs.ListMRComments(ctx, repo, iid)
	if err != nil {
		return nil, nil, err
	}
	approvals, err := mrs.ListApprovalDetails(ctx, repo, iid)
	if err != nil {
		return nil, nil, err
	}
	return comments, approvals, nil
}

// mrBlockingReason renders the lock's failure text ("" = clear).
func mrBlockingReason(blocking []string) string {
	if len(blocking) == 0 {
		return ""
	}
	return "request-changes outstanding from " + strings.Join(blocking, ", ")
}
