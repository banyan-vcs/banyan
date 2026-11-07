package view

import (
	"container/heap"
	"context"
	"log"
	"sort"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// weaveScopedAppend builds a subtree view (docs/02 §13): the
// identity-preserving combined chain (projection of the root-local history
// into X plus X's own locals, per the descendant rule) stays the PREFIX
// verbatim — existing clones keep a fast-forward path — and three families of
// entries append after it in (time, rel, oid) order:
//
//	kind 2  ancestor-scope locals — each commit's tree projected into scope
//	        via SubtreeAt (skipped when the scoped content is unchanged);
//	kind 0  the projection-side delta — root-local X commits outside the
//	        prefix ancestry (full X-scoped trees);
//	kind 1  strictly-below descendant locals, grafted at their relative path.
//
// New commits only ever append (prefix-stable under sane clocks). The cache
// key covers every input: root-local tip, the scope's own local tip, and
// every contributing timeline's head.
func (s *Service) weaveScopedAppend(ctx context.Context, repo, branch string, scope model.AnchorPath, rootLocal model.OID) (model.OID, error) {
	var localTip model.OID
	if st, lerr := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: scope, Name: branch}); lerr == nil {
		localTip = st.Head
	}
	baseHead, err := s.subtreeTip(repo, scope, branch, rootLocal, localTip)
	if err != nil || baseHead.IsZero() {
		return baseHead, err
	}

	all, err := s.Meta.ListAllTimelines(ctx, repo)
	if err != nil {
		return "", err
	}
	type subSrc struct {
		anchor model.AnchorPath
		head   model.OID
	}
	var subs []subSrc      // strictly below scope: graft
	var ancestors []subSrc // strictly above scope: project into scope
	var keySubs []string
	for _, tl := range all {
		if tl.Name != branch || tl.Anchor == model.RootAnchor || isWovenFamily(tl.Anchor) {
			continue
		}
		// Own scope rides the prefix (subtreeTip) and the key (scope@tip);
		// unrelated scopes do not intersect this view.
		if tl.Anchor == scope {
			continue
		}
		st, terr := s.Meta.Timeline(ctx, tl)
		if terr != nil || st.Head.IsZero() {
			continue
		}
		if isStrictlyBelow(scope, tl.Anchor) {
			ancestors = append(ancestors, subSrc{tl.Anchor, st.Head})
		} else if isStrictlyBelow(tl.Anchor, scope) {
			subs = append(subs, subSrc{tl.Anchor, st.Head})
		} else {
			continue // sibling scope: no overlap
		}
		keySubs = append(keySubs, string(tl.Anchor)+"@"+string(st.Head))
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].anchor < subs[j].anchor })
	sort.Slice(ancestors, func(i, j int) bool { return ancestors[i].anchor < ancestors[j].anchor })
	if !localTip.IsZero() {
		keySubs = append(keySubs, string(scope)+"@"+string(localTip))
	}
	sort.Strings(keySubs)
	key := rootKey{repo: repo, branch: branch, root: rootLocal, subs: strings.Join(keySubs, ","), scope: scope}
	// Record how far this view folded along every contributing source.
	advance := func() {
		s.advanceMarker(ctx, repo, branch, scope, model.RootAnchor, rootLocal)
		for _, as := range ancestors {
			s.advanceMarker(ctx, repo, branch, scope, as.anchor, as.head)
		}
		for _, ss := range subs {
			s.advanceMarker(ctx, repo, branch, scope, ss.anchor, ss.head)
		}
	}
	s.mu.Lock()
	if oid, ok := s.rootCache[key]; ok {
		s.mu.Unlock()
		return oid, nil
	}
	s.mu.Unlock()

	entries := map[model.OID]*weaveEntry{}
	// Provenance-embedded sources (docs/02 §13, same rule as the root weave):
	// every visited chain records which source commits it already folds
	// (banyan-woven-from — scoped graft outputs ride their own timeline's
	// locals after adoption, and root-side folds ride the projections). The
	// prefix walk below seeds the set; entry collection (ancestors →
	// projection delta → strictly-below grafts, anchor-ordered) keeps it
	// complete before each family is visited.
	embedded := map[model.OID]bool{}
	children := map[model.OID][]model.OID{}
	reachable := func(from model.OID, stop map[model.OID]bool, visit func(model.OID)) {
		if from.IsZero() {
			return
		}
		q := []model.OID{from}
		seen := map[model.OID]bool{}
		for len(q) > 0 {
			oid := q[0]
			q = q[1:]
			if seen[oid] || stop[oid] {
				continue
			}
			seen[oid] = true
			visit(oid)
			ci, err := s.Objects.CommitInfo(repo, oid)
			if err != nil {
				continue
			}
			q = append(q, ci.Parents...)
		}
	}
	// forkCut materializes the projection zone of a timeline's fork point and
	// returns the stop set for its local-zone walk.
	forkCut := func(anchor model.AnchorPath, base model.OID) map[model.OID]bool {
		stop := map[model.OID]bool{}
		if base.IsZero() {
			return stop
		}
		if res, serr := model.SplitHistory(s.Objects.Lookup(repo), s.Algo, base, anchor); serr == nil {
			for _, pc := range res.Commits {
				s.Objects.PutSynthetic(repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
			}
			reachable(res.Head, nil, func(oid model.OID) { stop[oid] = true })
		}
		return stop
	}
	// Rewrite-space append anchor (docs/02 sec.13): the persisted scoped
	// woven tip anchors the fold OUTPUT (prefix-stable, arrival order). A
	// stale anchor — the prefix outgrew it (own-scope push) — forces a full
	// append rebuild, and per-source markers are ignored for that rebuild
	// (they describe the dead section) then re-advanced to the read heads.
	prevWoven := model.OID("")
	if st, werr := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: wovenAnchorFor(scope), Name: branch}); werr == nil {
		prevWoven = st.Head
	}
	markersHonored := prevWoven.IsZero() || s.descends(repo, prevWoven, baseHead)
	if !markersHonored {
		prevWoven = model.OID("") // stale anchor: full refold
	}
	// Ancestor-scope locals: project each commit's tree into scope (kind 2).
	if len(ancestors) > 0 {
		lookup := s.Objects.Lookup(repo)
		for _, as := range ancestors {
			base := model.OID("")
			if st, terr := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: as.anchor, Name: branch}); terr == nil {
				base = st.Base
			}
			stop := forkCut(as.anchor, base)
			if markersHonored {
				if _, mstop := s.markerStop(ctx, repo, branch, scope, as.anchor, as.head); mstop != nil {
					mergeStop(stop, mstop)
				}
			}
			rel := model.AnchorPath(strings.TrimPrefix(string(scope), string(as.anchor)+"/"))
			reachable(as.head, stop, func(oid model.OID) {
				if _, dup := entries[oid]; dup {
					return
				}
				ci, err := s.Objects.CommitInfo(repo, oid)
				if err != nil {
					return
				}
				// Same two-clause skip as the root weave (see view.go); the
				// candidate's own claim is collected only after the check.
				skip := embedded[oid] || (ci.WovenFrom != "" && embedded[ci.WovenFrom])
				if ci.WovenFrom != "" {
					embedded[ci.WovenFrom] = true
				}
				if skip {
					return // its change is already folded into a visited chain
				}
				xtree := model.SubtreeAt(lookup, ci.Tree, rel)
				if xtree.IsZero() {
					return // scope path absent in this commit
				}
				// No-op criterion: the commit did not touch X relative to its
				// own first parent. Comparing against the current view tree
				// instead misjudges forks — a local-side X advance makes every
				// ancestor xtree look "different" and would roll X back.
				if len(ci.Parents) > 0 {
					if pci, perr := s.Objects.CommitInfo(repo, ci.Parents[0]); perr == nil &&
						model.SubtreeAt(lookup, pci.Tree, rel) == xtree {
						return
					}
				}
				entries[oid] = &weaveEntry{oid: oid, kind: 2, rel: rel, xtree: xtree, when: ci.When, ci: ci}
			})
		}
	}
	// Projection-side delta: root-local X commits outside the prefix ancestry
	// (kind 0, full X-scoped trees). Excluded by the root-local source marker
	// (projection OIDs are deterministic per source commit, so the marker's
	// projection ancestry is a stable skip set).
	if !rootLocal.IsZero() {
		if res, serr := model.SplitHistory(s.Objects.Lookup(repo), s.Algo, rootLocal, scope); serr == nil {
			for _, pc := range res.Commits {
				s.Objects.PutSynthetic(repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
			}
			skip := map[model.OID]bool{}
			// The prefix walk also seeds the provenance set: adopted scoped
			// outputs (locals) and their folds record what is already woven.
			reachable(baseHead, nil, func(oid model.OID) {
				skip[oid] = true
				if ci, cerr := s.Objects.CommitInfo(repo, oid); cerr == nil && ci.WovenFrom != "" {
					embedded[ci.WovenFrom] = true
				}
			})
			if markersHonored {
				if mHead, mstop := s.markerStop(ctx, repo, branch, scope, model.RootAnchor, rootLocal); mHead != "" {
					mergeStop(skip, mstop)
					if resm, merr := model.SplitHistory(s.Objects.Lookup(repo), s.Algo, mHead, scope); merr == nil {
						for _, pc := range resm.Commits {
							s.Objects.PutSynthetic(repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
						}
						reachable(resm.Head, nil, func(oid model.OID) { skip[oid] = true })
					}
				}
			}
			for _, pc := range res.Commits {
				oid := pc.OID(s.Algo)
				// Two-clause provenance skip (see view.go), checked before the
				// projection's own claim is collected.
				pskip := embedded[oid] || (pc.WovenFrom != "" && embedded[pc.WovenFrom])
				if pc.WovenFrom != "" {
					embedded[pc.WovenFrom] = true
				}
				if skip[oid] || pskip {
					continue
				}
				if ci, cerr := s.Objects.CommitInfo(repo, oid); cerr == nil {
					entries[oid] = &weaveEntry{oid: oid, kind: 0, when: ci.When, ci: ci}
				}
			}
		}
	}
	// Strictly-below descendant locals (kind 1, graft at relative path).
	for _, ss := range subs {
		base := model.OID("")
		if st, terr := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: ss.anchor, Name: branch}); terr == nil {
			base = st.Base
		}
		stop := forkCut(ss.anchor, base)
		if markersHonored {
			if _, mstop := s.markerStop(ctx, repo, branch, scope, ss.anchor, ss.head); mstop != nil {
				mergeStop(stop, mstop)
			}
		}
		rel := model.AnchorPath(strings.TrimPrefix(string(ss.anchor), string(scope)+"/"))
		reachable(ss.head, stop, func(oid model.OID) {
			if _, dup := entries[oid]; dup {
				return
			}
			ci, err := s.Objects.CommitInfo(repo, oid)
			if err != nil {
				return
			}
			// Two-clause provenance skip (see view.go): the graft itself is
			// claimed, or it is a fold of a source already embedded into the
			// prefix / projections / an earlier graft.
			skip := embedded[oid] || (ci.WovenFrom != "" && embedded[ci.WovenFrom])
			if ci.WovenFrom != "" {
				embedded[ci.WovenFrom] = true
			}
			if skip {
				return
			}
			entries[oid] = &weaveEntry{oid: oid, kind: 1, rel: rel, when: ci.When, ci: ci}
		})
	}

	for _, e := range entries {
		for _, p := range e.ci.Parents {
			if _, ok := entries[p]; ok && p != e.oid {
				children[p] = append(children[p], e.oid)
				e.deps++
			}
		}
	}
	h := &entryHeap{}
	for _, e := range entries {
		if e.deps == 0 {
			heap.Push(h, e)
		}
	}
	if h.Len() == 0 {
		advance()
		s.mu.Lock()
		s.rootCache[key] = baseHead
		s.mu.Unlock()
		return baseHead, nil
	}

	// Fold: kind 2/0 set the scoped tree directly; kind 1 grafts into the
	// evolving scope tree. Entries whose scoped tree equals the current
	// content are no-ops for this view and are skipped (children still unlock).
	store := s.Objects.TreeStoreFor(repo)
	bci, err := s.Objects.CommitInfo(repo, baseHead)
	if err != nil {
		return "", err
	}
	cur := bci.Tree
	prev := baseHead
	last := baseHead
	if !prevWoven.IsZero() {
		if wci, werr := s.Objects.CommitInfo(repo, prevWoven); werr == nil {
			cur = wci.Tree
		}
		prev, last = prevWoven, prevWoven
	}
	unlock := func(oid model.OID) {
		for _, ch := range children[oid] {
			ce := entries[ch]
			ce.deps--
			if ce.deps == 0 {
				heap.Push(h, ce)
			}
		}
	}
	batchTrees := map[model.OID]bool{}
	for h.Len() > 0 {
		e := heap.Pop(h).(*weaveEntry)
		_, raw, rerr := s.Objects.Raw(context.Background(), repo, e.oid)
		if rerr != nil {
			unlock(e.oid)
			continue
		}
		c, cerr := model.DecodeCommit(raw)
		if cerr != nil {
			unlock(e.oid)
			continue
		}
		var tree model.OID
		switch e.kind {
		case 2:
			tree = e.xtree
		case 0:
			tree = e.ci.Tree
		default:
			var terr error
			tree, terr = model.ReplaceSubtree(store, s.Algo, cur, e.rel, e.ci.Tree)
			if terr != nil {
				return "", terr
			}
		}
		// No-op for this scope, content already in the prefix, or already
		// produced earlier in this batch (cross-view rewrites of one change
		// yield identical trees; timestamp ties can order the older copy
		// after the newer).
		if tree == cur || s.TreeKnown(repo, baseHead, tree) || batchTrees[tree] {
			unlock(e.oid)
			continue
		}
		batchTrees[tree] = true
		nc := &model.AnchoredCommit{
			TreeOID:   tree,
			Parents:   []model.OID{prev},
			Author:    c.Author,
			Committer: c.Committer,
			Message:   c.Message,
			// The append IS the provenance: record the fold source so later
			// folds (rebuilds, other scopes, deeper timelines) recognize it.
			WovenFrom: e.oid,
		}
		oid := nc.OID(s.Algo)
		s.Objects.PutSynthetic(repo, oid, model.CommitType, nc.Encode())
		prev, last, cur = oid, oid, tree
		unlock(e.oid)
	}
	if !last.IsZero() && last != prevWoven {
		// Durability before visibility (same as the root weave): flush the
		// batched synthetics before the woven-tip CAS commits them. A failed
		// flush defers the anchor — the read keeps its in-memory answer and
		// the next materialization re-derives and retries (see view.go).
		if err := s.Objects.FlushSynthBatch(repo); err != nil {
			log.Printf("scoped weave %s%s: synth flush deferred: %v", repo, scope, err)
		} else {
			wovTL := model.Timeline{Repo: repo, Anchor: wovenAnchorFor(scope), Name: branch}
			_ = s.Meta.EnsureTimeline(ctx, wovTL, prevWoven, model.OID(""))
			_ = s.Meta.CASHead(ctx, wovTL, prevWoven, last) // benign race: CAS winner stands
		}
	}
	advance()
	s.mu.Lock()
	s.rootCache[key] = last
	s.mu.Unlock()
	return last, nil
}
