// mr.go — the merge-request lifecycle and the server-side branch merge it
// rides (docs/design/merge-requests.md). The merge recipe is the gitaly
// layer's applyMerge, service-flavored: raw timeline heads as parents
// (client expectations arrive advertised; the CAS and the parents use the
// raw head), merge.Trees over the shared syncMergeStore adapter, a
// two-parent root-anchor AnchoredCommit (author = the MR's author,
// committer = the merger), FlushSynthBatch as the durability gate, then the
// target-timeline CAS. Concurrency is the reserve-land-finalize order: the
// optimistic TransitionMR(open→merged) reservation is exclusive, a failed
// merge rolls the MR back to open, and the landing CAS is the last referee
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

// mrWalkCap bounds the ancestor walks (merge base + reachability): an MR
// between two tips of a huge repo walks at most this many commits a side.
const mrWalkCap = 50_000

// ErrBranchMissing names a branch that has no root-anchor timeline.
var ErrBranchMissing = errors.New("branch not found")

// mrIdentity renders one acting user as a git signature (GitLab's
// noreply shape: the local part is the username, the domain is ours).
func mrIdentity(user string, when time.Time) model.Identity {
	return model.Identity{Name: user, Email: user + "@banyan.users", When: when}
}

// branchHead answers a branch's raw root-anchor timeline head — the merge
// parent and CAS base, mirroring the gitaly merge surface.
func (s *Service) branchHead(ctx context.Context, repo, branch string) (model.OID, error) {
	if !validMRBranch(branch) {
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

// validMRBranch is git's check-ref-format subset (the gitaly layer keeps
// its own copy; the service one covers the JSON MR surface).
func validMRBranch(name string) bool {
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
		return len(ancestors) < mrWalkCap
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
	for len(queue) > 0 && len(seen) < mrWalkCap {
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

// mrMerge runs the shared 3-way body: resolve tips, compute the base,
// merge the trees, and — when land is set — assemble and persist the
// two-parent commit. The trees of a conflicted merge are content-addressed
// orphans; nothing references them, nothing leaks.
func (s *Service) mrMerge(ctx context.Context, repo, sourceBranch, targetBranch string, author, committer model.Identity, message string, land bool) (*MergeOutcome, error) {
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
	return s.mrMerge(ctx, repo, sourceBranch, targetBranch, model.Identity{}, model.Identity{}, "", false)
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
	out, err := s.mrMerge(ctx, repo, sourceBranch, targetBranch, mrIdentity(authorUser, now), mrIdentity(mergerUser, now), message, true)
	if err != nil {
		return out, err
	}
	if len(out.Conflicts) > 0 {
		// Asked to merge, could not: the conflicts are the failure.
		return out, &ErrMRNotMergeable{Reason: "conflicts", Conflicts: out.Conflicts}
	}

	// The landing CAS: expected = the target's raw head re-read now (the
	// tips were resolved inside mrMerge; a concurrent writer moves it and
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

// --- the MR lifecycle ---

// OpenMR validates both branches and records the request. The IID is
// allocated per repo and never reused.
func (s *Service) OpenMR(ctx context.Context, repo, sourceBranch, targetBranch, title, description, author string) (meta.MRInfo, error) {
	if title == "" {
		return meta.MRInfo{}, invalid("title required")
	}
	if author == "" {
		return meta.MRInfo{}, invalid("author required")
	}
	srcHead, err := s.branchHead(ctx, repo, sourceBranch)
	if err != nil {
		return meta.MRInfo{}, err
	}
	if _, err := s.branchHead(ctx, repo, targetBranch); err != nil {
		return meta.MRInfo{}, err
	}
	st, ok := s.Meta.(meta.MRStore)
	if !ok {
		return meta.MRInfo{}, invalid("this deployment owns no merge-request store")
	}
	now := time.Now().Unix()
	return st.CreateMR(ctx, meta.MRInfo{
		Repo: repo, Title: title, Description: description,
		Source: sourceBranch, Target: targetBranch,
		State: meta.MRStateOpen, Author: author, SourceHead: string(srcHead),
		CreatedAt: now, UpdatedAt: now,
	})
}

// ListMRs answers the repo's merge requests (state "" = all), newest first.
func (s *Service) ListMRs(ctx context.Context, repo, state string) ([]meta.MRInfo, error) {
	st, ok := s.Meta.(meta.MRStore)
	if !ok {
		return nil, invalid("this deployment owns no merge-request store")
	}
	return st.ListMRs(ctx, repo, state)
}

// MRTarget answers one MR's target branch (the merge gate's rule lookup).
func (s *Service) MRTarget(ctx context.Context, repo string, iid int64) (string, error) {
	st, ok := s.Meta.(meta.MRStore)
	if !ok {
		return "", invalid("this deployment owns no merge-request store")
	}
	mr, err := st.MRByIID(ctx, repo, iid)
	if err != nil {
		return "", err
	}
	return mr.Target, nil
}

// MRView is the detail wire shape: the stored record plus everything the
// UI decides on, computed live.
type MRView struct {
	meta.MRInfo
	SourceTip         model.OID `json:"source_tip"`
	Mergeable         bool      `json:"mergeable"`
	Conflicts         []string  `json:"conflicts,omitempty"`
	MergeBase         model.OID `json:"merge_base"`
	Approvals         []string  `json:"approvals"`
	RequiredApprovals int       `json:"required_approvals"`
	RuleLevel         string    `json:"rule_level,omitempty"`  // "" | protected | mr-only
	Blocking          []string  `json:"blocking,omitempty"`    // reviewers whose latest verdict is request-changes
}

// MRDetail answers one MR with live mergeability and the approvals
// standing (missing branches — a deleted source after merge — answer
// unmergeable rather than erroring: the record is the shareable truth).
func (s *Service) MRDetail(ctx context.Context, repo string, iid int64) (MRView, error) {
	st, ok := s.Meta.(meta.MRStore)
	if !ok {
		return MRView{}, invalid("this deployment owns no merge-request store")
	}
	mr, err := st.MRByIID(ctx, repo, iid)
	if err != nil {
		return MRView{}, err
	}
	view := MRView{MRInfo: mr, Approvals: []string{}}
	// Approvals are part of the record — merged and closed requests keep them.
	if view.Approvals, err = st.ListApprovals(ctx, repo, iid); err != nil {
		return MRView{}, err
	}
	if rs, ok := s.Meta.(meta.BranchRuleStore); ok {
		if rule, has, rerr := rs.BranchRuleOf(ctx, repo, mr.Target); rerr == nil && has {
			view.RuleLevel = rule.Level
			view.RequiredApprovals = rule.Approvals
		}
	}
	if view.State != meta.MRStateOpen {
		return view, nil // merged/closed: the stored record is the answer
	}
	// The request-changes lock rides the detail (the merge box's red strip).
	if cs, ok := s.Meta.(meta.MRCommentStore); ok {
		if blocking, berr := s.mrBlocking(ctx, cs, repo, iid, mr.Author); berr == nil {
			view.Blocking = blocking
		}
	}
	if view.SourceTip, err = s.branchHead(ctx, repo, mr.Source); err == nil {
		if pv, perr := s.PreviewMerge(ctx, repo, mr.Source, mr.Target); perr == nil {
			view.Mergeable = len(pv.Conflicts) == 0
			view.Conflicts = pv.Conflicts
			view.MergeBase = pv.Base
		}
	}
	return view, nil
}

// ApproveMR records one approval; the author's own approval is refused —
// the required count is other people by construction.
func (s *Service) ApproveMR(ctx context.Context, repo string, iid int64, user string, at int64) error {
	st, ok := s.Meta.(meta.MRStore)
	if !ok {
		return invalid("this deployment owns no merge-request store")
	}
	mr, err := st.MRByIID(ctx, repo, iid)
	if err != nil {
		return err
	}
	if mr.State != meta.MRStateOpen {
		return invalid("merge request !%d is %s", iid, mr.State)
	}
	if mr.Author == user {
		return invalid("the merge request author cannot approve it")
	}
	return st.AddApproval(ctx, repo, iid, user, at)
}

// UnapproveMR drops one approval (idempotent).
func (s *Service) UnapproveMR(ctx context.Context, repo string, iid int64, user string) error {
	if st, ok := s.Meta.(meta.MRStore); ok {
		return st.RemoveApproval(ctx, repo, iid, user)
	}
	return invalid("this deployment owns no merge-request store")
}

// CloseMR / ReopenMR move the state without touching refs.
func (s *Service) CloseMR(ctx context.Context, repo string, iid int64, at int64) (meta.MRInfo, error) {
	return s.transitionMR(ctx, repo, iid, meta.MRStateOpen, meta.MRStateClosed, at)
}

func (s *Service) ReopenMR(ctx context.Context, repo string, iid int64, at int64) (meta.MRInfo, error) {
	return s.transitionMR(ctx, repo, iid, meta.MRStateClosed, meta.MRStateOpen, at)
}

func (s *Service) transitionMR(ctx context.Context, repo string, iid int64, from, to string, at int64) (meta.MRInfo, error) {
	st, ok := s.Meta.(meta.MRStore)
	if !ok {
		return meta.MRInfo{}, invalid("this deployment owns no merge-request store")
	}
	return st.TransitionMR(ctx, repo, iid, from, func(m *meta.MRInfo) {
		m.State = to
		m.UpdatedAt = at
	})
}

// ErrMRNotMergeable carries why a merge bounced: conflicts, missing
// approvals, or a moved target (the CAS race — refetch and retry).
type ErrMRNotMergeable struct {
	Reason    string
	Conflicts []string
}

func (e *ErrMRNotMergeable) Error() string {
	if len(e.Conflicts) > 0 {
		return fmt.Sprintf("merge request not mergeable: %s: %s", e.Reason, strings.Join(e.Conflicts, ", "))
	}
	return "merge request not mergeable: " + e.Reason
}

// MergeMR is the full gate: reserve the MR (open→merged, the exclusive
// hop), check approvals against the target's rule, land the merge, and
// finalize; any failure rolls the reservation back to open so the state
// never lies about a merge that did not land. meta.ErrNonFastForward from
// the landing CAS is surfaced as-is — a moved target is retryable.
func (s *Service) MergeMR(ctx context.Context, repo string, iid int64, merger, message string, at int64) (meta.MRInfo, error) {
	st, ok := s.Meta.(meta.MRStore)
	if !ok {
		return meta.MRInfo{}, invalid("this deployment owns no merge-request store")
	}
	mr, err := st.MRByIID(ctx, repo, iid)
	if err != nil {
		return meta.MRInfo{}, err
	}
	if mr.State != meta.MRStateOpen {
		return meta.MRInfo{}, invalid("merge request !%d is %s", iid, mr.State)
	}
	// The request-changes lock: reviewers whose LATEST verdict blocks stand
	// between the request and the merge until they review again (source
	// advances never dismiss a verdict — the reviewer's key only).
	if cs, ok := s.Meta.(meta.MRCommentStore); ok {
		if blocking, berr := s.mrBlocking(ctx, cs, repo, iid, mr.Author); berr != nil {
			return meta.MRInfo{}, berr
		} else if reason := mrBlockingReason(blocking); reason != "" {
			return meta.MRInfo{}, &ErrMRNotMergeable{Reason: reason}
		}
	}
	// Approvals against the target's rule: mr-only demands its count; the
	// other levels demand none here (their rank gates live at the API).
	if rs, ok := s.Meta.(meta.BranchRuleStore); ok {
		if rule, has, rerr := rs.BranchRuleOf(ctx, repo, mr.Target); rerr == nil && has && rule.Level == meta.RuleMROnly {
			approvals, err := st.ListApprovals(ctx, repo, iid)
			if err != nil {
				return meta.MRInfo{}, err
			}
			if len(approvals) < rule.Approvals {
				return meta.MRInfo{}, &ErrMRNotMergeable{Reason: fmt.Sprintf("%d approval(s) required, %d recorded", rule.Approvals, len(approvals))}
			}
		}
	}

	// Reserve: the exclusive open→merged hop. Losing here means a
	// concurrent merge or close got there first.
	reserved, err := st.TransitionMR(ctx, repo, iid, meta.MRStateOpen, func(m *meta.MRInfo) {
		m.State = meta.MRStateMerged
		m.UpdatedAt = at
	})
	if err != nil {
		return meta.MRInfo{}, err
	}
	release := func(why error) (meta.MRInfo, error) {
		if _, rerr := s.transitionMR(ctx, repo, iid, meta.MRStateMerged, meta.MRStateOpen, at); rerr != nil {
			return meta.MRInfo{}, fmt.Errorf("%w (rollback also failed: %v)", why, rerr)
		}
		return meta.MRInfo{}, why
	}

	out, err := s.MergeBranches(ctx, repo, mr.Source, mr.Target, mr.Author, merger, message)
	if err != nil {
		if errors.Is(err, meta.ErrNonFastForward) {
			return release(&ErrMRNotMergeable{Reason: "the target branch moved — refetch and retry"})
		}
		return release(err)
	}
	// The recorded merge commit is the ADVERTISED tip, not the raw timeline
	// head: the woven view re-linearizes a multi-parent commit into a fold
	// (docs/design/merge-requests.md §边界) — the fold is what every clone
	// and every link resolves; the raw two-parent commit stays in the
	// timeline and the object store.
	recorded := out.Commit
	if adv, terr := s.Views.RootTip(ctx, repo, mr.Target); terr == nil && !adv.IsZero() {
		recorded = adv
	}
	final, err := st.TransitionMR(ctx, repo, iid, meta.MRStateMerged, func(m *meta.MRInfo) {
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
