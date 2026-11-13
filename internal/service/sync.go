// sync.go — the explicit subtree→root alignment (docs/design/data-model.md §12: true
// divergence — both the subtree timeline and the root projection advanced —
// keeps the local head on read and "waits for explicit sync"). This IS that
// sync: replay the subtree's local-only commits onto the root's current
// projection tip with the same per-commit three-way replay the convergence
// train uses (docs/design/workflow.md §2.2), then CAS the timeline head. Thin clients hold
// no history to rebase locally, so the server does it; the rebased chain is
// exactly the "cross-view rewrite of the projection plus newer content"
// shape the receive-pack guard already accepts (docs/design/data-model.md §13 third form).
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/banyan-vcs/banyan/internal/merge"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// ErrSyncConflict is a true content divergence: replaying the local segment
// onto the projection conflicted at Commit on Paths. v1 has no half-way
// resume — resolve by hand (a reconciling root commit that makes the local
// diff apply cleanly, or rewriting the local side) and sync again.
type ErrSyncConflict struct {
	Commit model.OID
	Paths  []string
}

func (e *ErrSyncConflict) Error() string {
	return fmt.Sprintf("sync conflict at %s: %v", shortOID(e.Commit), e.Paths)
}

// ErrSyncUnsupported marks chain shapes v1 does not replay (merge commits in
// the local segment, or a segment with no common point on the projection).
type ErrSyncUnsupported struct{ Reason string }

func (e *ErrSyncUnsupported) Error() string { return e.Reason }

// SyncRequest addresses one subtree timeline; Branch defaults to the repo's.
type SyncRequest struct {
	Repo   string `json:"repo"`
	Sub    string `json:"sub"`
	Branch string `json:"branch,omitempty"`
}

// SyncResult reports what happened: uptodate covers ahead / purely-behind /
// no local timeline (all leave the timeline untouched); rebased counts the
// replayed local commits.
type SyncResult struct {
	Status   string    `json:"status"` // uptodate | rebased
	Rebased  int       `json:"rebased"`
	Head     model.OID `json:"head"`
	RootView model.OID `json:"rootView,omitempty"`
}

// SyncSubtree aligns one subtree timeline onto the root view. Bounded OCC:
// a lost head CAS (a concurrent push landed first) refetches everything and
// replays onto the fresh projection, ≤3 attempts.
func (s *Service) SyncSubtree(ctx context.Context, req SyncRequest) (*SyncResult, error) {
	sub, err := parseSub(req.Sub)
	if err != nil {
		return nil, err
	}
	if sub.IsRoot() {
		return nil, invalid("sync: sub required — root timelines land verbatim and never need alignment (docs/design/data-model.md §13)")
	}
	if _, err = s.Meta.GetRepo(ctx, req.Repo); err != nil {
		return nil, err
	}
	branch := req.Branch
	if branch == "" {
		if branch, err = s.defaultBranchOf(ctx, req.Repo); err != nil {
			return nil, err
		}
	}
	localTL := model.Timeline{Repo: req.Repo, Anchor: sub, Name: branch}
	for attempt := 0; attempt < 3; attempt++ {
		res, retry, err := s.syncOnce(ctx, req.Repo, sub, branch, localTL)
		if err != nil {
			return nil, err
		}
		if !retry {
			return res, nil
		}
	}
	return nil, fmt.Errorf("sync %s%s: busy (head keeps moving)", req.Repo, sub)
}

func (s *Service) syncOnce(ctx context.Context, repo string, sub model.AnchorPath, branch string, localTL model.Timeline) (*SyncResult, bool, error) {
	st, err := s.Meta.Timeline(ctx, localTL)
	if errors.Is(err, meta.ErrTimelineUnknown) {
		// No local timeline — nothing of ours to align (the view already
		// tracks the projection; a plain pull fast-forwards).
		return &SyncResult{Status: "uptodate"}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rootState, err := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: branch})
	if errors.Is(err, meta.ErrTimelineUnknown) {
		return &SyncResult{Status: "uptodate", Head: st.Head}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rootLocal := rootState.Head

	projTip, err := s.Views.ProjectionTip(repo, sub, rootLocal)
	if err != nil {
		return nil, false, err
	}
	if projTip.IsZero() || st.Head.IsZero() {
		return &SyncResult{Status: "uptodate", Head: st.Head}, false, nil
	}
	// Descendant rule (docs/design/data-model.md §12): local ahead (contains the projection)
	// or purely behind (projection contains the local) — both are already
	// consistent; only true divergence needs the replay.
	if s.Views.Descends(repo, st.Head, projTip) || s.Views.Descends(repo, projTip, st.Head) {
		return &SyncResult{Status: "uptodate", Head: st.Head}, false, nil
	}

	chain, fork, err := s.localSegment(repo, st.Head, projTip)
	if err != nil {
		return nil, false, err
	}
	if len(chain) == 0 {
		return &SyncResult{Status: "uptodate", Head: st.Head}, false, nil
	}

	// One replay = one synth batch: synthesized trees/commits persist as a
	// single pack (docs/operations/performance.md P0-2).
	s.Objects.BeginSynthBatch(repo)
	defer s.Objects.EndSynthBatch(repo)

	projInfo, err := s.Objects.CommitInfo(repo, projTip)
	if err != nil {
		return nil, false, err
	}
	store := &syncMergeStore{TreeStore: s.Objects.TreeStoreFor(repo), objects: s.Objects, repo: repo}
	lookup := s.Objects.Lookup(repo)
	parent := projTip
	merged := projInfo.Tree // projection commits carry X-scoped trees already
	prev := fork            // original parent of the oldest local commit
	for _, ci := range chain {
		info, cerr := lookup.Commit(ci)
		if cerr != nil {
			return nil, false, cerr
		}
		prevTree := model.OID("")
		if !prev.IsZero() {
			pi, perr := lookup.Commit(prev)
			if perr != nil {
				return nil, false, perr
			}
			prevTree = pi.TreeOID
		}
		m, conflicts, merr := merge.Trees(store, s.Algo, prevTree, merged, info.TreeOID, string(sub), merge.Options{
			OursLabel:   "banyan/" + branch,
			TheirsLabel: "synced " + shortOID(ci),
		})
		if merr != nil {
			return nil, false, merr
		}
		if len(conflicts) > 0 {
			return nil, false, &ErrSyncConflict{Commit: ci, Paths: conflicts}
		}
		// treeKnown no-op: the replay result already exists in the chain's
		// history (a prior partial sync, a pre-rebased push, or the change
		// already reached root through a peer's pull) — reuse the original
		// commit, no duplicate graft (the convergence rule).
		if s.Views.TreeKnown(repo, parent, m) {
			parent = ci
			merged = m
			prev = ci
			continue
		}
		syn := *info
		syn.TreeOID = m
		syn.Parents = []model.OID{parent}
		syn.Committer = model.Identity{Name: "banyan", Email: "sync@banyan", When: time.Now()}
		oid := syn.OID(s.Algo)
		s.Objects.PutSynthetic(repo, oid, model.CommitType, syn.Encode())
		parent = oid
		merged = m
		prev = ci
	}
	// Everything already flowed (e.g. the change reached root through a
	// peer's pull of the woven view): the timeline is consistent as-is.
	if parent == st.Head {
		return &SyncResult{Status: "uptodate", Head: st.Head}, false, nil
	}

	// Fork point first (safe under either CAS outcome: Base only bounds the
	// weave's local zone, and every commit between old and new zone bounds
	// is root content), then the head CAS.
	if berr := s.Meta.SetTimelineBase(ctx, localTL, rootLocal); berr != nil && !errors.Is(berr, meta.ErrTimelineUnknown) {
		return nil, false, berr
	}
	if cerr := s.Meta.CASHead(ctx, localTL, st.Head, parent); cerr != nil {
		if errors.Is(cerr, meta.ErrNonFastForward) {
			return nil, true, nil // concurrent landing — replay onto the fresh state
		}
		return nil, false, cerr
	}
	// The rebased chain REPLACES the old local head — from the timeline's
	// perspective a rewind. The old head's fold may already be baked into
	// the persisted /.woven output (an append-only anchor), which would
	// duplicate the rebased subjects on the next fold. Reset the marker
	// family so the next materialization re-derives from the source
	// timelines (byte-stable, T78) — the same reset the force-push landing
	// path performs (receivepack.go).
	if rerr := s.Views.ResetWoven(ctx, repo, branch); rerr != nil {
		return nil, false, rerr
	}
	rootView, rerr := s.Views.RootTip(ctx, repo, branch)
	if rerr != nil {
		rootView = ""
	}
	return &SyncResult{Status: "rebased", Rebased: len(chain), Head: parent, RootView: rootView}, false, nil
}

// localSegment collects the local-only commits of head (oldest first) down
// to the merge point: the first ancestor that the projection also contains.
// fork is that commit (the original parent of chain[0]).
func (s *Service) localSegment(repo string, head, projTip model.OID) (chain []model.OID, fork model.OID, err error) {
	cur := head
	for !cur.IsZero() && cur != projTip {
		if s.Views.Descends(repo, projTip, cur) {
			return reverseOIDs(chain), cur, nil
		}
		if len(chain) > 10000 {
			return nil, "", invalid("sync: local chain too long (>10000 commits)")
		}
		info, cerr := s.Objects.CommitInfo(repo, cur)
		if cerr != nil {
			return nil, "", cerr
		}
		if len(info.Parents) > 1 {
			return nil, "", &ErrSyncUnsupported{Reason: fmt.Sprintf("sync: merge commit %s in the local chain — v1 replays linear chains only", shortOID(cur))}
		}
		chain = append(chain, cur)
		if len(info.Parents) == 0 {
			break
		}
		cur = info.Parents[0]
	}
	if cur == projTip {
		return reverseOIDs(chain), cur, nil
	}
	return nil, "", &ErrSyncUnsupported{Reason: "sync: local chain has no common point with the root projection"}
}

func reverseOIDs(in []model.OID) []model.OID {
	out := make([]model.OID, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

func shortOID(oid model.OID) string {
	if len(oid) > 8 {
		return string(oid[:8])
	}
	return string(oid)
}

// syncMergeStore adapts the object plane to merge.Store (gitwire's
// convergence adapter shape): tree reads/writes through the repo's TreeStore,
// blob writes land in the synthetic-object layer.
type syncMergeStore struct {
	model.TreeStore
	objects *storage.ObjectService
	repo    string
}

func (m *syncMergeStore) PutBlob(data []byte) model.OID {
	oid := (&model.Blob{Content: data}).OID(m.objects.Algo)
	m.objects.PutSynthetic(m.repo, oid, model.BlobType, data)
	return oid
}
