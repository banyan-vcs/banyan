// mergerequests.go — the merge-request lifecycle and the server-side branch merge it
// rides (docs/design/merge-requests.md). The merge recipe is the gitaly
// layer's applyMerge, service-flavored: raw timeline heads as parents
// (client expectations arrive advertised; the CAS and the parents use the
// raw head), merge.Trees over the shared syncMergeStore adapter, a
// two-parent root-anchor AnchoredCommit (author = the merge request's author,
// committer = the merger), FlushSynthBatch as the durability gate, then the
// target-timeline CAS. Concurrency is the reserve-land-finalize order: the
// optimistic TransitionMergeRequest(open→merged) reservation is exclusive, a failed
// merge rolls the merge request back to open, and the landing CAS is the last referee
// (a target that moved under us rolls back too — the retry re-derives the
// same content-addressed merge).
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/merge"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
)

// mergeRequestWalkCap bounds the ancestor walks (merge base + reachability): an merge request
// between two tips of a huge repo walks at most this many commits a side.
const mergeRequestWalkCap = 50_000

// ErrBranchMissing names a branch that has no root-anchor timeline.
var ErrBranchMissing = errors.New("branch not found")

// mergeRequestIdentity renders one acting user as a git signature (GitLab's
// noreply shape: the local part is the username, the domain is ours).
func mergeRequestIdentity(user string, when time.Time) model.Identity {
	return model.Identity{Name: user, Email: user + "@banyan.users", When: when}
}

// branchHead answers a branch's raw root-anchor timeline head — the merge
// parent and CAS base, mirroring the gitaly merge surface.
func (s *Service) branchHead(ctx context.Context, repo, branch string) (model.OID, error) {
	if !validMergeRequestBranch(branch) {
		return "", invalid("branch name %q invalid", branch)
	}
	st, err := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: branch})
	if err != nil {
		// An unknown timeline reads as a missing branch (the store carries no
		// separate sentinel; the context keeps the underlying error).
		return "", fmt.Errorf("%w: %s: %v", ErrBranchMissing, branch, err)
	}
	if st.Head.IsZero() {
		return "", fmt.Errorf("%w: %s", ErrBranchMissing, branch)
	}
	return st.Head, nil
}

// validMergeRequestBranch is git's check-ref-format subset (the gitaly layer keeps
// its own copy; the service one covers the JSON merge request surface).
func validMergeRequestBranch(name string) bool {
	if name == "" || name == "HEAD" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return false
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, bad := range []string{"..", "//", "@{", `\`, "~", "^", ":", "?", "[", "*"} {
		if strings.Contains(name, bad) {
			return false
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return false
		}
	}
	return true
}

// mergeBaseOf answers the first common ancestor of two commits in a
// topological walk — the single-base approximation the merge engine's
// contract already is (merge.Trees takes one base; git's recursive virtual
// base is out of scope, documented in docs/design/merge-requests.md).
func (s *Service) mergeBaseOf(ctx context.Context, repo string, a, b model.OID) (model.OID, error) {
	ancestors := map[model.OID]bool{}
	if err := s.walkAncestors(repo, a, func(oid model.OID) bool {
		ancestors[oid] = true
		return len(ancestors) < mergeRequestWalkCap
	}); err != nil {
		return "", err
	}
	var base model.OID
	if err := s.walkAncestors(repo, b, func(oid model.OID) bool {
		if ancestors[oid] {
			base = oid
			return false
		}
		return true
	}); err != nil {
		return "", err
	}
	return base, nil
}

// walkAncestors visits a commit's ancestry breadth-first (parents after
// children); keep=false stops the walk. Missing commits end their path —
// partial history serves the merge-base approximation fine.
func (s *Service) walkAncestors(repo string, from model.OID, keep func(model.OID) bool) error {
	seen := map[model.OID]bool{}
	queue := []model.OID{from}
	for len(queue) > 0 && len(seen) < mergeRequestWalkCap {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if !keep(oid) {
			return nil
		}
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			continue
		}
		queue = append(queue, ci.Parents...)
	}
	return nil
}

// MergeOutcome is one landed (or previewed) merge.
type MergeOutcome struct {
	Commit    model.OID // the two-parent merge commit (zero in a preview)
	Base      model.OID // the merge base used (zero = unrelated histories)
	Conflicts []string  // repo-relative paths, non-empty = not mergeable
}

// mergeRequestMerge runs the shared 3-way body: resolve tips, compute the base,
// merge the trees, and — when land is set — assemble and persist the
// two-parent commit. The trees of a conflicted merge are content-addressed
// orphans; nothing references them, nothing leaks.
func (s *Service) mergeRequestMerge(ctx context.Context, repo, sourceBranch, targetBranch string, author, committer model.Identity, message string, land bool) (*MergeOutcome, error) {
	if _, err := s.Meta.GetRepo(ctx, repo); err != nil {
		return nil, err
	}
	theirs, err := s.branchHead(ctx, repo, sourceBranch)
	if err != nil {
		return nil, err
	}
	ours, err := s.branchHead(ctx, repo, targetBranch)
	if err != nil {
		return nil, err
	}
	base, err := s.mergeBaseOf(ctx, repo, ours, theirs)
	if err != nil {
		return nil, err
	}
	var baseTree model.OID
	if !base.IsZero() {
		bi, err := s.Objects.CommitInfo(repo, base)
		if err != nil {
			return nil, invalid("merge base %s unreadable: %v", base, err)
		}
		baseTree = bi.Tree
	}
	oursInfo, err := s.Objects.CommitInfo(repo, ours)
	if err != nil {
		return nil, invalid("target tip %s unreadable: %v", ours, err)
	}
	theirsInfo, err := s.Objects.CommitInfo(repo, theirs)
	if err != nil {
		return nil, invalid("source tip %s unreadable: %v", theirs, err)
	}

	s.Objects.BeginSynthBatch(repo)
	defer s.Objects.EndSynthBatch(repo)

	store := &syncMergeStore{TreeStore: s.Objects.TreeStoreFor(repo), objects: s.Objects, repo: repo}
	treeOID, conflicts, err := merge.Trees(store, s.Algo, baseTree, oursInfo.Tree, theirsInfo.Tree, "", merge.Options{
		OursLabel:   targetBranch,
		TheirsLabel: sourceBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("merge: %v", err)
	}
	out := &MergeOutcome{Base: base, Conflicts: conflicts}
	if len(conflicts) > 0 || !land {
		return out, nil
	}

	msg := message
	if msg != "" && !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	commit := &model.AnchoredCommit{
		Anchor:    model.RootAnchor,
		TreeOID:   treeOID,
		Parents:   []model.OID{ours, theirs},
		Author:    author,
		Committer: committer,
		Message:   msg,
	}
	commitOID := commit.OID(s.Algo)
	s.Objects.PutSynthetic(repo, commitOID, model.CommitType, commit.Encode())
	if err := s.Objects.FlushSynthBatch(repo); err != nil {
		// Durability before any ref CAS: a head pointing at objects only
		// this process knows would strand every other reader.
		return nil, fmt.Errorf("persisting merge objects: %v", err)
	}
	out.Commit = commitOID
	return out, nil
}

// PreviewMerge answers mergeability live: the merge base and the conflicted
// paths, without landing anything.
func (s *Service) PreviewMerge(ctx context.Context, repo, sourceBranch, targetBranch string) (*MergeOutcome, error) {
	return s.mergeRequestMerge(ctx, repo, sourceBranch, targetBranch, model.Identity{}, model.Identity{}, "", false)
}

// MergeBranches lands the merge: source into target as a two-parent commit
// on the target's raw timeline. The CAS base is the raw head read here —
// a target that moves concurrently fails with meta.ErrNonFastForward and
// the caller refetches and retries.
func (s *Service) MergeBranches(ctx context.Context, repo, sourceBranch, targetBranch, authorUser, mergerUser, message string) (*MergeOutcome, error) {
	if message == "" {
		message = fmt.Sprintf("Merge branch '%s' into %s", sourceBranch, targetBranch)
	}
	now := time.Now()
	out, err := s.mergeRequestMerge(ctx, repo, sourceBranch, targetBranch, mergeRequestIdentity(authorUser, now), mergeRequestIdentity(mergerUser, now), message, true)
	if err != nil {
		return out, err
	}
	if len(out.Conflicts) > 0 {
		// Asked to merge, could not: the conflicts are the failure.
		return out, &ErrMergeRequestNotMergeable{Reason: "conflicts", Conflicts: out.Conflicts}
	}

	// The landing CAS: expected = the target's raw head re-read now (the
	// tips were resolved inside mergeRequestMerge; a concurrent writer moves it and
	// this fails closed).
	ours, err := s.branchHead(ctx, repo, targetBranch)
	if err != nil {
		return out, err
	}
	tl := model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: targetBranch}
	if err := s.Meta.CASHead(ctx, tl, ours, out.Commit); err != nil {
		return out, err
	}
	// A merge commit carries the old head as its first parent — a forward
	// move; no woven reset (the gitaly casRef discipline).
	return out, nil
}

// --- the merge request lifecycle ---

// OpenMergeRequest validates both branches and records the request. The IID is
// allocated per repo and never reused.
func (s *Service) OpenMergeRequest(ctx context.Context, repo, sourceBranch, targetBranch, title, description, author string) (meta.MergeRequestInfo, error) {
	if title == "" {
		return meta.MergeRequestInfo{}, invalid("title required")
	}
	if author == "" {
		return meta.MergeRequestInfo{}, invalid("author required")
	}
	srcHead, err := s.branchHead(ctx, repo, sourceBranch)
	if err != nil {
		return meta.MergeRequestInfo{}, err
	}
	if _, err := s.branchHead(ctx, repo, targetBranch); err != nil {
		return meta.MergeRequestInfo{}, err
	}
	st, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return meta.MergeRequestInfo{}, invalid("this deployment owns no merge-request store")
	}
	now := time.Now().Unix()
	return st.CreateMergeRequest(ctx, meta.MergeRequestInfo{
		Repo: repo, Title: title, Description: description,
		Source: sourceBranch, Target: targetBranch,
		State: meta.MergeRequestStateOpen, Author: author, SourceHead: string(srcHead),
		CreatedAt: now, UpdatedAt: now,
	})
}

// ListMergeRequests answers the repo's merge requests (state "" = all), newest first.
func (s *Service) ListMergeRequests(ctx context.Context, repo, state string) ([]meta.MergeRequestInfo, error) {
	st, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return nil, invalid("this deployment owns no merge-request store")
	}
	return st.ListMergeRequests(ctx, repo, state)
}

// MergeRequestTarget answers one merge request's target branch (the merge gate's rule lookup).
func (s *Service) MergeRequestTarget(ctx context.Context, repo string, iid int64) (string, error) {
	st, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return "", invalid("this deployment owns no merge-request store")
	}
	mergeRequest, err := st.MergeRequestByIID(ctx, repo, iid)
	if err != nil {
		return "", err
	}
	return mergeRequest.Target, nil
}

// MergeRequestView is the detail wire shape: the stored record plus everything the
// UI decides on, computed live.
type MergeRequestView struct {
	meta.MergeRequestInfo
	SourceTip         model.OID `json:"source_tip"`
	Mergeable         bool      `json:"mergeable"`
	Conflicts         []string  `json:"conflicts,omitempty"`
	MergeBase         model.OID `json:"merge_base"`
	Approvals         []string  `json:"approvals"`
	RequiredApprovals int       `json:"required_approvals"`
	RuleLevel         string    `json:"rule_level,omitempty"` // "" | protected | merge-request-only
	Blocking          []string  `json:"blocking,omitempty"`   // reviewers whose latest verdict is request-changes
}

// MergeRequestDetail answers one merge request with live mergeability and the approvals
// standing (missing branches — a deleted source after merge — answer
// unmergeable rather than erroring: the record is the shareable truth).
func (s *Service) MergeRequestDetail(ctx context.Context, repo string, iid int64) (MergeRequestView, error) {
	st, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return MergeRequestView{}, invalid("this deployment owns no merge-request store")
	}
	mergeRequest, err := st.MergeRequestByIID(ctx, repo, iid)
	if err != nil {
		return MergeRequestView{}, err
	}
	view := MergeRequestView{MergeRequestInfo: mergeRequest, Approvals: []string{}}
	// Approvals are part of the record — merged and closed requests keep them.
	if view.Approvals, err = st.ListApprovals(ctx, repo, iid); err != nil {
		return MergeRequestView{}, err
	}
	if rs, ok := s.Meta.(meta.BranchRuleStore); ok {
		if rule, has, rerr := rs.BranchRuleOf(ctx, repo, mergeRequest.Target); rerr == nil && has {
			view.RuleLevel = rule.Level
			view.RequiredApprovals = rule.Approvals
		}
	}
	if view.State != meta.MergeRequestStateOpen {
		return view, nil // merged/closed: the stored record is the answer
	}
	// The request-changes lock rides the detail (the merge box's red strip).
	if cs, ok := s.Meta.(meta.MergeRequestCommentStore); ok {
		if blocking, berr := s.mergeRequestBlocking(ctx, cs, repo, iid, mergeRequest.Author); berr == nil {
			view.Blocking = blocking
		}
	}
	if view.SourceTip, err = s.branchHead(ctx, repo, mergeRequest.Source); err == nil {
		if pv, perr := s.PreviewMerge(ctx, repo, mergeRequest.Source, mergeRequest.Target); perr == nil {
			view.Mergeable = len(pv.Conflicts) == 0
			view.Conflicts = pv.Conflicts
			view.MergeBase = pv.Base
		}
	}
	return view, nil
}

// ApproveMergeRequest records one approval; the author's own approval is refused —
// the required count is other people by construction.
func (s *Service) ApproveMergeRequest(ctx context.Context, repo string, iid int64, user string, at int64) error {
	st, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return invalid("this deployment owns no merge-request store")
	}
	mergeRequest, err := st.MergeRequestByIID(ctx, repo, iid)
	if err != nil {
		return err
	}
	if mergeRequest.State != meta.MergeRequestStateOpen {
		return invalid("merge request !%d is %s", iid, mergeRequest.State)
	}
	if mergeRequest.Author == user {
		return invalid("the merge request author cannot approve it")
	}
	return st.AddApproval(ctx, repo, iid, user, at)
}

// UnapproveMergeRequest drops one approval (idempotent).
func (s *Service) UnapproveMergeRequest(ctx context.Context, repo string, iid int64, user string) error {
	if st, ok := s.Meta.(meta.MergeRequestStore); ok {
		return st.RemoveApproval(ctx, repo, iid, user)
	}
	return invalid("this deployment owns no merge-request store")
}

// CloseMergeRequest / ReopenMergeRequest move the state without touching refs.
func (s *Service) CloseMergeRequest(ctx context.Context, repo string, iid int64, at int64) (meta.MergeRequestInfo, error) {
	return s.transitionMergeRequest(ctx, repo, iid, meta.MergeRequestStateOpen, meta.MergeRequestStateClosed, at)
}

func (s *Service) ReopenMergeRequest(ctx context.Context, repo string, iid int64, at int64) (meta.MergeRequestInfo, error) {
	return s.transitionMergeRequest(ctx, repo, iid, meta.MergeRequestStateClosed, meta.MergeRequestStateOpen, at)
}

func (s *Service) transitionMergeRequest(ctx context.Context, repo string, iid int64, from, to string, at int64) (meta.MergeRequestInfo, error) {
	st, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return meta.MergeRequestInfo{}, invalid("this deployment owns no merge-request store")
	}
	return st.TransitionMergeRequest(ctx, repo, iid, from, func(m *meta.MergeRequestInfo) {
		m.State = to
		m.UpdatedAt = at
	})
}

// ErrMergeRequestNotMergeable carries why a merge bounced: conflicts, missing
// approvals, or a moved target (the CAS race — refetch and retry).
type ErrMergeRequestNotMergeable struct {
	Reason    string
	Conflicts []string
}

func (e *ErrMergeRequestNotMergeable) Error() string {
	if len(e.Conflicts) > 0 {
		return fmt.Sprintf("merge request not mergeable: %s: %s", e.Reason, strings.Join(e.Conflicts, ", "))
	}
	return "merge request not mergeable: " + e.Reason
}

// MergeMergeRequest is the full gate: reserve the merge request (open→merged, the exclusive
// hop), check approvals against the target's rule, land the merge, and
// finalize; any failure rolls the reservation back to open so the state
// never lies about a merge that did not land. meta.ErrNonFastForward from
// the landing CAS is surfaced as-is — a moved target is retryable.
func (s *Service) MergeMergeRequest(ctx context.Context, repo string, iid int64, merger, message string, at int64) (meta.MergeRequestInfo, error) {
	st, ok := s.Meta.(meta.MergeRequestStore)
	if !ok {
		return meta.MergeRequestInfo{}, invalid("this deployment owns no merge-request store")
	}
	mergeRequest, err := st.MergeRequestByIID(ctx, repo, iid)
	if err != nil {
		return meta.MergeRequestInfo{}, err
	}
	if mergeRequest.State != meta.MergeRequestStateOpen {
		return meta.MergeRequestInfo{}, invalid("merge request !%d is %s", iid, mergeRequest.State)
	}
	// The request-changes lock: reviewers whose LATEST verdict blocks stand
	// between the request and the merge until they review again (source
	// advances never dismiss a verdict — the reviewer's key only).
	if cs, ok := s.Meta.(meta.MergeRequestCommentStore); ok {
		if blocking, berr := s.mergeRequestBlocking(ctx, cs, repo, iid, mergeRequest.Author); berr != nil {
			return meta.MergeRequestInfo{}, berr
		} else if reason := mergeRequestBlockingReason(blocking); reason != "" {
			return meta.MergeRequestInfo{}, &ErrMergeRequestNotMergeable{Reason: reason}
		}
	}
	// Approvals against the target's rule: merge-request-only demands its count; the
	// other levels demand none here (their rank gates live at the API).
	if rs, ok := s.Meta.(meta.BranchRuleStore); ok {
		if rule, has, rerr := rs.BranchRuleOf(ctx, repo, mergeRequest.Target); rerr == nil && has && rule.Level == meta.RuleMergeRequestOnly {
			approvals, err := st.ListApprovals(ctx, repo, iid)
			if err != nil {
				return meta.MergeRequestInfo{}, err
			}
			if len(approvals) < rule.Approvals {
				return meta.MergeRequestInfo{}, &ErrMergeRequestNotMergeable{Reason: fmt.Sprintf("%d approval(s) required, %d recorded", rule.Approvals, len(approvals))}
			}
		}
	}

	// Reserve: the exclusive open→merged hop. Losing here means a
	// concurrent merge or close got there first.
	reserved, err := st.TransitionMergeRequest(ctx, repo, iid, meta.MergeRequestStateOpen, func(m *meta.MergeRequestInfo) {
		m.State = meta.MergeRequestStateMerged
		m.UpdatedAt = at
	})
	if err != nil {
		return meta.MergeRequestInfo{}, err
	}
	release := func(why error) (meta.MergeRequestInfo, error) {
		if _, rerr := s.transitionMergeRequest(ctx, repo, iid, meta.MergeRequestStateMerged, meta.MergeRequestStateOpen, at); rerr != nil {
			return meta.MergeRequestInfo{}, fmt.Errorf("%w (rollback also failed: %v)", why, rerr)
		}
		return meta.MergeRequestInfo{}, why
	}

	out, err := s.MergeBranches(ctx, repo, mergeRequest.Source, mergeRequest.Target, mergeRequest.Author, merger, message)
	if err != nil {
		if errors.Is(err, meta.ErrNonFastForward) {
			return release(&ErrMergeRequestNotMergeable{Reason: "the target branch moved — refetch and retry"})
		}
		return release(err)
	}
	// The recorded merge commit is the ADVERTISED tip, not the raw timeline
	// head: the woven view re-linearizes a multi-parent commit into a fold
	// (docs/design/merge-requests.md §边界) — the fold is what every clone
	// and every link resolves; the raw two-parent commit stays in the
	// timeline and the object store.
	recorded := out.Commit
	if adv, terr := s.Views.RootTip(ctx, repo, mergeRequest.Target); terr == nil && !adv.IsZero() {
		recorded = adv
	}
	final, err := st.TransitionMergeRequest(ctx, repo, iid, meta.MergeRequestStateMerged, func(m *meta.MergeRequestInfo) {
		m.MergeCommit = string(recorded)
		m.MergedBy = merger
		m.UpdatedAt = at
	})
	if err != nil {
		// The ref moved but the ledger did not — surface both facts; the
		// merge itself stands (out.Commit is on the branch).
		return reserved, fmt.Errorf("merge landed as %s but finalizing the record failed: %w", recorded, err)
	}
	return final, nil
}
