// Package view derives git-visible views from timeline state (docs/01 §13,
// design v5 assembly-on-read). The ONLY stored mutable state is per-timeline
// heads (root-local plus one per directory); every git view is a pure, cached
// function of those heads:
//
//	SubtreeHead(X): projection of the root-local chain into X, combined with
//	                X's local timeline by the descendant rule (carried over
//	                from v4 unchanged).
//	RootTip:        linear weave — all source commits (the root-local chain
//	                plus every subtree timeline's local commits) folded in
//	                deterministic (time, anchor, oid) topological order into a
//	                merge-free root history; subtree entries re-tree into root
//	                scope via ReplaceSubtree.
//
// The write path therefore touches exactly one timeline per push: no
// cross-directory lock coupling, directory-granular CAS, and a root history
// with zero merge scaffolding.
package view

import (
	"container/heap"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

type Service struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo

	mu        sync.Mutex
	subCache  map[subKey]model.OID
	rootCache map[rootKey]model.OID
}

func New(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Service {
	return &Service{
		Meta:      meta,
		Objects:   objects,
		Algo:      algo,
		subCache:  map[subKey]model.OID{},
		rootCache: map[rootKey]model.OID{},
	}
}

// wovenAnchor is the internal timeline family persisting each branch's
// woven root-view tip: the append anchor that makes the weave arrival-ordered
// and prefix-stable across recomputation and restarts (docs/01 §13).
const wovenAnchor = model.AnchorPath("/.woven")

// wovenAnchorFor is the persisted append anchor of one scoped view's weave.
func wovenAnchorFor(scope model.AnchorPath) model.AnchorPath {
	return model.AnchorPath(string(wovenAnchor) + string(scope))
}

// srcMarker is the per-source woven marker of one view: how far along a
// contributing source chain this view has folded, recorded in SOURCE space
// (original client OIDs). Exclusion by the rewrite-space /.woven ancestry
// cannot see graft sources — their originals never enter it, only their
// rewrites do — so every materialization would re-fold them (duplicate
// subjects, churning tips). Markers close that gap (docs/01 §13).
func srcMarker(view, src model.AnchorPath) model.AnchorPath {
	return model.AnchorPath(string(wovenAnchor) + string(view) + "@" + string(src))
}

// isWovenFamily reports whether an anchor belongs to the internal /.woven family.
func isWovenFamily(a model.AnchorPath) bool {
	return strings.HasPrefix(string(a), string(wovenAnchor))
}

type subKey struct {
	repo   string
	sub    model.AnchorPath
	branch string
	root   model.OID
	local  model.OID
}

type rootKey struct {
	repo   string
	branch string
	root   model.OID
	subs   string // sorted "anchor@head,..." — the full materialization input
	scope  model.AnchorPath
}

// SubtreeHead returns the subtree view head and the root-local head.
// Plain scopes (no descendant timeline activity) use the identity-preserving
// projection + descendant rule (docs/01 §12). When any timeline exists at an
// anchor strictly BELOW this scope, the view must weave those descendant
// changes in (docs/01 §13) — the same linear weave as the root view, scoped
// to X: root-local projections and X's own locals fold as full X-scoped
// trees, descendant locals graft at their relative paths.
func (s *Service) SubtreeHead(ctx context.Context, repo string, sub model.AnchorPath, branch string) (head, rootLocal model.OID, err error) {
	state, err := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: branch})
	if errors.Is(err, storage.ErrTimelineUnknown) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	rootLocal = state.Head

	// The append-weave subsumes the plain path: its prefix IS the plain
	// combined chain (identity OIDs) and appended entries (projection-side
	// delta + descendant locals) only ever follow it. With no entries it
	// returns the prefix verbatim, so plain repos keep byte-stable views.
	head, werr := s.weaveScopedAppend(ctx, repo, branch, sub, rootLocal)
	if werr != nil {
		return "", "", werr
	}
	return head, rootLocal, nil
}

// isStrictlyBelow reports whether a is an anchor strictly below scope.
func isStrictlyBelow(a model.AnchorPath, scope model.AnchorPath) bool {
	if scope.IsRoot() {
		return a != model.RootAnchor
	}
	return strings.HasPrefix(string(a), string(scope)+"/")
}

// subtreeTip computes and caches the combined subtree head: projection chain
// plus local timeline merged by the descendant rule. A pure function of
// (rootLocal, localTip) — deterministic, recomputable after restart.
func (s *Service) subtreeTip(repo string, sub model.AnchorPath, branch string, rootLocal, localTip model.OID) (model.OID, error) {
	key := subKey{repo: repo, sub: sub, branch: branch, root: rootLocal, local: localTip}
	s.mu.Lock()
	if oid, ok := s.subCache[key]; ok {
		s.mu.Unlock()
		return oid, nil
	}
	s.mu.Unlock()

	var projTip model.OID
	if !rootLocal.IsZero() {
		res, err := model.SplitHistory(s.Objects.Lookup(repo), s.Algo, rootLocal, sub)
		if err != nil {
			return "", err
		}
		for _, pc := range res.Commits {
			s.Objects.PutSynthetic(repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
		}
		projTip = res.Head
	}

	var head model.OID
	switch {
	case localTip.IsZero():
		head = projTip
	case projTip.IsZero():
		head = localTip
	case s.descends(repo, projTip, localTip):
		head = projTip
	case s.descends(repo, localTip, projTip):
		head = localTip
	default: // true divergence: the local side keeps the head (stable prefix);
		// the projection-side delta is surfaced as appended weave entries
		// (weaveScopedAppend), so clients still see root-side X changes.
		head = localTip
	}

	s.mu.Lock()
	s.subCache[key] = head
	s.mu.Unlock()
	return head, nil
}

// descends reports whether target is an ancestor of from. Generation numbers
// (git commit-graph semantics) prune the walk: a parent with gen < gen(target)
// cannot lead to target, and gen(from) ≤ gen(target) rules out ancestry
// outright. Pruning uses EXACT generations only — tainted (lower-bound)
// values are ignored so an understated gen can never prune a true ancestor.
// The walk warms the generation memo over the visited ancestry, so repeated
// materializations reuse it (docs/09 §2).
func (s *Service) descends(repo string, from, target model.OID) bool {
	if from.IsZero() || target.IsZero() || from == target {
		return from == target
	}
	gT, okT := s.Objects.Generation(repo, target)
	if okT {
		if gF, okF := s.Objects.Generation(repo, from); okF && gF <= gT {
			return false
		}
	}
	visited := map[model.OID]bool{}
	queue := []model.OID{from}
	for len(queue) > 0 && len(visited) < 100000 {
		oid := queue[0]
		queue = queue[1:]
		if visited[oid] {
			continue
		}
		visited[oid] = true
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			continue
		}
		for _, p := range ci.Parents {
			if p == target {
				return true
			}
			if okT {
				if gP, okP := s.Objects.Generation(repo, p); okP && gP < gT {
					continue // target cannot sit under p
				}
			}
			queue = append(queue, p)
		}
	}
	return false
}

// --- root view: linear weave (docs/01 §13) ---

type weaveEntry struct {
	kind  int
	rel   model.AnchorPath
	xtree model.OID // ancestor-scope entries: the commit's tree projected into scope
	oid   model.OID
	when  int64
	ci    *storage.CommitInfo
	deps  int // parents inside the weave set not yet emitted
}

// entryHeap orders ready entries by (committer time, anchor, oid): the
// deterministic interleave key of the weave.
type entryHeap []*weaveEntry

func (h entryHeap) Len() int { return len(h) }
func (h entryHeap) Less(i, j int) bool {
	if h[i].when != h[j].when {
		return h[i].when < h[j].when
	}
	if h[i].rel != h[j].rel {
		return h[i].rel < h[j].rel
	}
	return h[i].oid < h[j].oid
}
func (h entryHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *entryHeap) Push(x any)   { *h = append(*h, x.(*weaveEntry)) }
func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

// RootTip materializes the root view head: weave over scope "" (docs/01 §13).
func (s *Service) RootTip(ctx context.Context, repo, branch string) (model.OID, error) {
	return s.weave(ctx, repo, branch, model.RootAnchor)
}

// weave materializes the view for one scope ("" = root view, X = subtree
// view with descendant activity): a linear, merge-free history folding
//
//   - the root-local chain — recorded full trees for the root view, or its
//     SplitHistory projections into X for a subtree scope (full X-scoped trees);
//   - the scope's own local timeline (full X-scoped trees);
//   - every strictly-below descendant timeline's local commits, grafted into
//     the evolving scope tree at their relative path.
//
// Pure function of the timeline tips (deterministic, cached per tips set);
// synthesized objects persist write-through. Root-local commits whose chain
// was not interleaved are reused verbatim (identity fast path — OID-stable
// root history for repos without subtree activity).
func (s *Service) weave(ctx context.Context, repo, branch string, scope model.AnchorPath) (model.OID, error) {
	state, err := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: branch})
	if errors.Is(err, storage.ErrTimelineUnknown) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	rootLocal := state.Head

	all, err := s.Meta.ListAllTimelines(ctx, repo)
	if err != nil {
		return "", err
	}
	type subSrc struct {
		anchor model.AnchorPath
		head   model.OID
	}
	var subs []subSrc
	var keySubs []string
	for _, tl := range all {
		if tl.Name != branch || tl.Anchor == model.RootAnchor || isWovenFamily(tl.Anchor) {
			continue
		}
		if scope != model.RootAnchor && tl.Anchor != scope && !isStrictlyBelow(tl.Anchor, scope) {
			continue
		}
		st, terr := s.Meta.Timeline(ctx, tl)
		if terr != nil || st.Head.IsZero() {
			continue
		}
		subs = append(subs, subSrc{tl.Anchor, st.Head})
		keySubs = append(keySubs, string(tl.Anchor)+"@"+string(st.Head))
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].anchor < subs[j].anchor })
	sort.Strings(keySubs)
	key := rootKey{repo: repo, branch: branch, root: rootLocal, subs: strings.Join(keySubs, ","), scope: scope}
	s.mu.Lock()
	if oid, ok := s.rootCache[key]; ok {
		s.mu.Unlock()
		return oid, nil
	}
	s.mu.Unlock()

	// 1. Collect weave entries. kind: full-scope trees fold directly;
	// graft entries stitch their scoped tree in at a relative path.
	const (
		kindFull  = 0
		kindGraft = 1
	)
	entries := map[model.OID]*weaveEntry{}
	addEntry := func(oid model.OID, kind int, rel model.AnchorPath) {
		if _, dup := entries[oid]; dup {
			return
		}
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			return
		}
		entries[oid] = &weaveEntry{oid: oid, kind: kind, rel: rel, when: ci.When, ci: ci}
	}
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
	if scope.IsRoot() {
		_, stop := s.markerStop(ctx, repo, branch, scope, model.RootAnchor, rootLocal)
		reachable(rootLocal, stop, func(oid model.OID) { addEntry(oid, kindFull, "") })
	} else if !rootLocal.IsZero() {
		res, serr := model.SplitHistory(s.Objects.Lookup(repo), s.Algo, rootLocal, scope)
		if serr != nil {
			return "", serr
		}
		for _, pc := range res.Commits {
			s.Objects.PutSynthetic(repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
		}
		for _, pc := range res.Commits {
			addEntry(pc.OID(s.Algo), kindFull, "")
		}
	}
	for _, ss := range subs {
		// Local zone = reachable(head) minus the projection zone at the
		// fork: the timeline was EnsureTimeline'd at the projection tip of
		// the Base root commit. Materialize those projections so the walk
		// can traverse them.
		base := model.OID("")
		if st, terr := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: ss.anchor, Name: branch}); terr == nil {
			base = st.Base
		}
		var p0 model.OID
		if !base.IsZero() {
			if res, serr := model.SplitHistory(s.Objects.Lookup(repo), s.Algo, base, ss.anchor); serr == nil {
				for _, pc := range res.Commits {
					s.Objects.PutSynthetic(repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
				}
				p0 = res.Head
			}
		}
		stop := map[model.OID]bool{}
		reachable(p0, nil, func(oid model.OID) { stop[oid] = true })
		if _, mstop := s.markerStop(ctx, repo, branch, scope, ss.anchor, ss.head); mstop != nil {
			mergeStop(stop, mstop)
		}
		kind, rel := kindFull, model.AnchorPath("")
		if ss.anchor != scope {
			kind = kindGraft
			if scope.IsRoot() {
				rel = ss.anchor
			} else {
				rel = model.AnchorPath(strings.TrimPrefix(string(ss.anchor), string(scope)+"/"))
			}
		}
		reachable(ss.head, stop, func(oid model.OID) { addEntry(oid, kind, rel) })
	}

	// Rewrite-space append anchor (docs/01 §13): the persisted woven tip.
	// Already-folded sources are excluded by their per-source markers above
	// (source space); prevWoven only anchors the fold OUTPUT so appends stay
	// prefix-stable (arrival order, immune to commit-timestamp ties).
	prevWoven := model.OID("")
	if st, werr := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: wovenAnchorFor(scope), Name: branch}); werr == nil {
		prevWoven = st.Head
	}

	// 2. Topological bookkeeping within the weave set.
	children := map[model.OID][]model.OID{}
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

	// 3. Fold.
	store := s.Objects.TreeStoreFor(repo)
	empty := model.EmptyTreeOID(s.Algo)
	cur := empty
	prev := model.OID("")
	if !prevWoven.IsZero() {
		if ci, cerr := s.Objects.CommitInfo(repo, prevWoven); cerr == nil {
			cur = ci.Tree
		}
		prev = prevWoven
	}
	wovenAs := map[model.OID]model.OID{}
	last := prevWoven // zero new entries: the view stays at the woven tip
	batchTrees := map[model.OID]bool{}
	unlock := func(oid model.OID) {
		for _, ch := range children[oid] {
			ce := entries[ch]
			ce.deps--
			if ce.deps == 0 {
				heap.Push(h, ce)
			}
		}
	}
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
		case kindFull:
			tree = e.ci.Tree
			// A prior weave output re-entering through a client chain
			// (clients commit on top of view tips, so their pushed ancestry
			// carries rewrites): already woven — refolding it would rebuild
			// the OID (new parent) and duplicate the subject.
			if !prevWoven.IsZero() && s.descends(repo, prevWoven, e.oid) {
				unlock(e.oid)
				continue
			}
		default:
			base := cur
			if base.IsZero() {
				base = empty
			}
			tree, err = model.ReplaceSubtree(store, s.Algo, base, e.rel, e.ci.Tree)
			if err != nil {
				return "", err
			}
			// Graft whose resulting tree is already the current state, is
			// recorded somewhere in the woven chain, or was already produced
			// earlier in THIS batch (cross-view rewrites of one change yield
			// identical trees; timestamp ties can order the older copy after
			// the newer, and its ReplaceSubtree would regress content).
			if tree == cur || s.TreeKnown(repo, prevWoven, tree) || batchTrees[tree] {
				unlock(e.oid)
				continue
			}
			batchTrees[tree] = true
		}
		nc := &model.AnchoredCommit{
			TreeOID:   tree,
			Author:    c.Author,
			Committer: c.Committer,
			Message:   c.Message,
		}
		if !prev.IsZero() {
			nc.Parents = []model.OID{prev}
		}
		oid := nc.OID(s.Algo)
		if e.kind == kindFull && scope.IsRoot() && oid == e.oid {
			wovenAs[e.oid] = e.oid // untouched root-local commit: reuse verbatim
		} else {
			s.Objects.PutSynthetic(repo, oid, model.CommitType, nc.Encode())
			wovenAs[e.oid] = oid
		}
		prev = wovenAs[e.oid]
		last = prev
		cur = tree
		unlock(e.oid)
	}

	if !last.IsZero() && last != prevWoven {
		wovenTL := model.Timeline{Repo: repo, Anchor: wovenAnchorFor(scope), Name: branch}
		_ = s.Meta.EnsureTimeline(ctx, wovenTL, prevWoven, model.OID(""))
		if err := s.Meta.CASHead(ctx, wovenTL, prevWoven, last); err != nil {
			_ = err // benign race: the CAS winner stands
		}
	}
	s.advanceMarker(ctx, repo, branch, scope, model.RootAnchor, rootLocal)
	for _, ss := range subs {
		s.advanceMarker(ctx, repo, branch, scope, ss.anchor, ss.head)
	}
	s.mu.Lock()
	s.rootCache[key] = last
	s.mu.Unlock()
	return last, nil
}

// markerStop returns the woven-marker head for one contributing source and
// its source-space exclusion set (the marker's ancestry). A zero head means
// "collect all". A marker that is not an ancestor of the current head
// (stale — the timeline was force-moved) is ignored so the source refolds
// from scratch.
func (s *Service) markerStop(ctx context.Context, repo, branch string, view, src model.AnchorPath, head model.OID) (model.OID, map[model.OID]bool) {
	st, err := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: srcMarker(view, src), Name: branch})
	if err != nil || st.Head.IsZero() || !s.descends(repo, head, st.Head) {
		return "", nil
	}
	stop := map[model.OID]bool{}
	walk := []model.OID{st.Head}
	for len(walk) > 0 {
		oid := walk[0]
		walk = walk[1:]
		if stop[oid] {
			continue
		}
		stop[oid] = true
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			continue
		}
		walk = append(walk, ci.Parents...)
	}
	return st.Head, stop
}

// advanceMarker records how far the weave for one view folded along one
// source chain. FF along the source; a lost CAS is a benign race (the winner
// recorded an equally valid head).
func (s *Service) advanceMarker(ctx context.Context, repo, branch string, view, src model.AnchorPath, head model.OID) {
	if head.IsZero() {
		return
	}
	tl := model.Timeline{Repo: repo, Anchor: srcMarker(view, src), Name: branch}
	expected := model.OID("")
	if st, err := s.Meta.Timeline(ctx, tl); err == nil {
		expected = st.Head
		if expected == head {
			return
		}
	}
	_ = s.Meta.EnsureTimeline(ctx, tl, expected, model.OID(""))
	_ = s.Meta.CASHead(ctx, tl, expected, head)
}

// mergeStop folds extra exclusions into stop.
func mergeStop(stop map[model.OID]bool, extra map[model.OID]bool) {
	for oid := range extra {
		stop[oid] = true
	}
}

// TreeKnown reports whether any commit in from's ancestry already records
// this tree. Cross-view rewrites of one change carry identical trees, so a
// graft whose tree is known carries nothing new to this view. The reverse
// tree→commits index turns the full-history scan into a candidate lookup +
// generation-pruned ancestry checks (docs/09 §2); warming via Generation
// backfills the index over from's whole ancestry first, so no candidate in
// that ancestry can be missed. Exported for the receive-pack staleness guard.
func (s *Service) TreeKnown(repo string, from, tree model.OID) bool {
	if from.IsZero() || tree.IsZero() {
		return false
	}
	_, _ = s.Objects.Generation(repo, from) // warm: CommitInfo backfill feeds treeCommits
	for _, c := range s.Objects.CommitsWithTree(repo, tree) {
		if c == from || s.descends(repo, from, c) {
			return true
		}
	}
	return false
}

// Descends reports whether target is an ancestor of from (exported for the
// receive-pack staleness guard).
func (s *Service) Descends(repo string, from, target model.OID) bool {
	return s.descends(repo, from, target)
}

// ProjectionTip materializes the projection of the root-local chain into sub
// and returns its head (receive-pack staleness guard input).
func (s *Service) ProjectionTip(repo string, sub model.AnchorPath, rootLocal model.OID) (model.OID, error) {
	if rootLocal.IsZero() {
		return "", nil
	}
	res, err := model.SplitHistory(s.Objects.Lookup(repo), s.Algo, rootLocal, sub)
	if err != nil {
		return "", err
	}
	for _, pc := range res.Commits {
		s.Objects.PutSynthetic(repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
	}
	return res.Head, nil
}
