package storage

import (
	"bytes"
	"context"

	"github.com/banyan-vcs/banyan/internal/model"
)

// In-memory derived indexes (complement to the docs/storage/storage.md §1.3 global index + docs/design/derived-data.md commit-graph):
//
//   - CommitGraph: oid → (tree, parents, when); closure computation and
//     projection traversal no longer decode raw commit bytes per object; built
//     incrementally at push (objects are already decoded in IngestPack — zero extra IO).
//   - Generation: oid → generation number (1 + max over parents, git
//     commit-graph semantics). Ancestry checks prune whole subtrees with it
//     (descends O(history) → O(distance)); memoized, immutable-DAG-safe
//     (new commits only ever add entries, never invalidate old ones).
//   - TreeMemo: oid → parsed entry table; path walking (SubtreeAt/closure
//     expansion) parses each tree exactly once.
//   - treeCommits: tree → commits recording it (reverse index). "Is this tree
//     already in this history" becomes a candidate lookup + ancestry check
//     instead of a full-history scan.
//
// Lifetime matches the process (MetaStore in-memory; M2 persists it with metadata).

// CommitInfo is one commit-graph record.
type CommitInfo struct {
	Tree      model.OID
	Parents   []model.OID
	When      int64     // committer unix time (for ordering/generation)
	WovenFrom model.OID // weave provenance: this commit is the fold of that source commit (docs/design/data-model.md §13); zero = none
}

type repoIndex struct {
	commits map[model.OID]*CommitInfo
	trees   map[model.OID][]model.TreeEntry
	// trees is byte-bounded (idxTreesMaxBytes, FIFO eviction below): the
	// unbounded original held every tree ever decoded for the process
	// lifetime — git.git's closure walks retained ~4.6GB of entry tables
	// (发现⑤'s heap profile: 79% of a 9GB heap in DecodeTree). Misses after
	// eviction re-read through Raw, the authoritative path.
	treeFIFO  []model.OID
	treeSize  map[model.OID]int64
	treeBytes int64
	// gen memoizes generation numbers (0 = not yet computed). Exact under the
	// complete-objects invariant: a value is written only after every known
	// ancestor resolved; missing objects are never cached. Ancestors that
	// could not be resolved taint the entry (genTainted) — the value is then
	// a lower bound and must not drive ancestry pruning.
	gen        map[model.OID]uint32
	genTainted map[model.OID]bool
	// treeCommits is the reverse index tree → commits (one entry per commit;
	// total memory O(commits)). Uncapped on purpose: a capped list can miss
	// the one ancestor carrying the tree and flip TreeKnown to false.
	treeCommits map[model.OID][]model.OID
	// Lightweight commit index (docs/design/data-model.md §11, design v3): git oid → (tree edge, bmsg ref).
	// Envelopes dedupe by content addressing; built at push with per-commit byte-level render verification (renderFails should stay 0).
	records     map[model.OID]*model.CommitRecord
	msgs        map[model.OID][]byte
	renderFails int
}

// buildIndex builds/merges indexes at push ingest time.
func (s *ObjectService) buildIndex(repo string, dp *DecodedPack) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	ri := s.idx[repo]
	if ri == nil {
		ri = newRepoIndex()
		s.idx[repo] = ri
	}
	for oid, raw := range dp.Commits {
		if _, done := ri.commits[oid]; done {
			continue
		}
		if c, err := model.DecodeCommit(raw); err == nil {
			ri.commits[oid] = &CommitInfo{Tree: c.TreeOID, Parents: c.Parents, When: c.Committer.When.Unix(), WovenFrom: c.WovenFrom}
			ri.treeCommits[c.TreeOID] = append(ri.treeCommits[c.TreeOID], oid)
		}
		// Decompose into lightweight record + bmsg (byte-level render verification;
		// failures counted, not rejected — the pack stays in storage untouched, only the lightweight index misses the entry).
		if _, done := ri.records[oid]; done {
			continue
		}
		if tree, envelope, err := model.DecomposeCommit(raw); err == nil {
			if !bytes.Equal(model.RenderCommit(tree, envelope), raw) {
				ri.renderFails++
				continue
			}
			msgOID := model.MessageOID(envelope, s.Algo)
			if _, exists := ri.msgs[msgOID]; !exists {
				ri.msgs[msgOID] = envelope
			}
			ri.records[oid] = &model.CommitRecord{GitOID: oid, Tree: tree, Msg: msgOID}
		}
	}
	for oid, raw := range dp.Trees {
		if _, done := ri.trees[oid]; done {
			continue
		}
		s.indexTreeLocked(repo, ri, oid, raw)
	}
}

// idxTreesMaxBytes bounds one repo's decoded-tree cache. 512MiB holds a
// go-git-class repo's whole tree set; git.git-class repos cycle through the
// FIFO (walks re-read evicted trees through Raw — sequential pack reads,
// not the random per-object pattern that made un-indexed walks crawl).
const idxTreesMaxBytes = 512 << 20

// idxTreeBytes estimates one cached tree's retained bytes: the decoded
// entries' name/mode/target strings track the encoded payload closely.
func idxTreeBytes(raw []byte) int64 { return int64(len(raw)) + 64 }

// indexTree decodes and caches one tree under the byte cap (FIFO eviction;
// the just-inserted tree never evicts itself). Returns the entries and
// whether the decode succeeded. idxMu must NOT be held — takes it once.
func (s *ObjectService) indexTree(repo string, oid model.OID, raw []byte) ([]model.TreeEntry, bool) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	ri := s.idx[repo]
	if ri == nil {
		ri = newRepoIndex()
		s.idx[repo] = ri
	}
	if e, ok := ri.trees[oid]; ok {
		return e, true
	}
	s.indexTreeLocked(repo, ri, oid, raw)
	e, ok := ri.trees[oid]
	return e, ok
}

// indexTreeLocked is indexTree with the caller holding idxMu and ri.
func (s *ObjectService) indexTreeLocked(repo string, ri *repoIndex, oid model.OID, raw []byte) {
	if _, ok := ri.trees[oid]; ok {
		return
	}
	t, err := model.DecodeTree(raw)
	if err != nil {
		return
	}
	budget := s.treesCap()
	sz := idxTreeBytes(raw)
	ri.trees[oid] = t.Entries
	ri.treeSize[oid] = sz
	ri.treeBytes += sz
	ri.treeFIFO = append(ri.treeFIFO, oid)
	for ri.treeBytes > budget && len(ri.treeFIFO) > 1 {
		old := ri.treeFIFO[0]
		ri.treeFIFO = ri.treeFIFO[1:]
		if _, ok := ri.trees[old]; ok {
			delete(ri.trees, old)
			ri.treeBytes -= ri.treeSize[old]
			delete(ri.treeSize, old)
		}
	}
}

// treesCap resolves the tree-cache byte budget (0 = default).
func (s *ObjectService) treesCap() int64 {
	if s.IndexTreesMaxBytes > 0 {
		return s.IndexTreesMaxBytes
	}
	return idxTreesMaxBytes
}

// IndexTypeOf answers an object's type from the in-memory views only — the
// synthetic table and the push-built commit/tree indexes. No IO: a miss
// (blob, tag, or a cold process before any push) simply reports unknown and
// the caller falls back to a real read. The clone-shaped enumeration probe
// (docs/storage/storage.md §5) uses this to type want tips without inflating object bodies.
func (s *ObjectService) IndexTypeOf(repo string, oid model.OID) (model.ObjectType, bool) {
	s.synthMu.Lock()
	if so, ok := s.synth[repo][oid]; ok {
		s.synthMu.Unlock()
		return so.Type, true
	}
	s.synthMu.Unlock()
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	ri := s.idx[repo]
	if ri == nil {
		return "", false
	}
	if _, ok := ri.commits[oid]; ok {
		return model.CommitType, true
	}
	if _, ok := ri.trees[oid]; ok {
		return model.TreeType, true
	}
	return "", false
}

// CommitIndexStats returns lightweight commit index stats (commits / unique bmsgs / render failures).
func (s *ObjectService) CommitIndexStats(repo string) (commits, uniqueMsgs, renderFails int) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	ri := s.idx[repo]
	if ri == nil {
		return 0, 0, 0
	}
	return len(ri.records), len(ri.msgs), ri.renderFails
}

// CommitInfo fetches commit structure: an index hit returns directly; a miss
// (synthetic projection commits, or a cold process before the first push —
// e.g. after a restart with persistent metadata) is parsed once and
// back-filled into the index.
func (s *ObjectService) CommitInfo(repo string, oid model.OID) (*CommitInfo, error) {
	s.idxMu.Lock()
	if ri := s.idx[repo]; ri != nil { // nil on a cold start: must not deref
		if ci, ok := ri.commits[oid]; ok {
			s.idxMu.Unlock()
			return ci, nil
		}
	}
	s.idxMu.Unlock()

	t, raw, err := s.Raw(context.Background(), repo, oid)
	if err != nil {
		return nil, err
	}
	if t != model.CommitType {
		return nil, model.ErrObjectNotFound
	}
	c, err := model.DecodeCommit(raw)
	if err != nil {
		return nil, err
	}
	ci := &CommitInfo{Tree: c.TreeOID, Parents: c.Parents, When: c.Committer.When.Unix(), WovenFrom: c.WovenFrom}
	s.idxMu.Lock()
	if s.idx[repo] == nil {
		s.idx[repo] = newRepoIndex()
	}
	if _, done := s.idx[repo].commits[oid]; !done {
		s.idx[repo].commits[oid] = ci
		s.idx[repo].treeCommits[ci.Tree] = append(s.idx[repo].treeCommits[ci.Tree], oid)
	}
	s.idxMu.Unlock()
	return ci, nil
}

// TreeEntries fetches tree entries: index hit returns directly; a miss is parsed
// once and back-filled. The returned slice is index-owned; callers must not modify it.
func (s *ObjectService) TreeEntries(repo string, oid model.OID) ([]model.TreeEntry, error) {
	s.idxMu.Lock()
	if ri := s.idx[repo]; ri != nil { // nil on a cold start: must not deref
		if e, ok := ri.trees[oid]; ok {
			s.idxMu.Unlock()
			return e, nil
		}
	}
	s.idxMu.Unlock()

	t, raw, err := s.Raw(context.Background(), repo, oid)
	if err != nil {
		return nil, err
	}
	if t != model.TreeType {
		return nil, model.ErrObjectNotFound
	}
	if entries, ok := s.indexTree(repo, oid, raw); ok { // cached under the byte cap
		return entries, nil
	}
	tree, derr := model.DecodeTree(raw) // surface the specific decode failure
	if derr != nil {
		return nil, derr
	}
	return tree.Entries, nil
}

// newRepoIndex is the single constructor — every map must exist regardless of
// which path creates the entry first (cold-start backfill vs push-side
// buildIndex), or the later writer hits a nil map.
func newRepoIndex() *repoIndex {
	return &repoIndex{
		commits:     map[model.OID]*CommitInfo{},
		trees:       map[model.OID][]model.TreeEntry{},
		treeSize:    map[model.OID]int64{},
		gen:         map[model.OID]uint32{},
		genTainted:  map[model.OID]bool{},
		treeCommits: map[model.OID][]model.OID{},
		records:     map[model.OID]*model.CommitRecord{},
		msgs:        map[model.OID][]byte{},
	}
}

// Generation returns the commit's generation number (1 for a root commit,
// 1 + max(parents) otherwise — git commit-graph semantics) and whether it is
// EXACT. A missing ancestor makes the value a lower bound; such entries are
// tainted and reported exact=false — ancestry pruning must only use exact
// values (an understated generation could prune a true ancestor). Results
// are memoized; the walk warms CommitInfo (and the tree reverse index) over
// the whole ancestry of the query, so follow-up ancestry questions on the
// same history are index hits.
func (s *ObjectService) Generation(repo string, oid model.OID) (uint32, bool) {
	if oid.IsZero() {
		return 0, false
	}
	s.idxMu.Lock()
	ri := s.idx[repo]
	if ri == nil {
		ri = newRepoIndex()
		s.idx[repo] = ri
	}
	if g, ok := ri.gen[oid]; ok {
		tainted := ri.genTainted[oid]
		s.idxMu.Unlock()
		return g, !tainted
	}
	s.idxMu.Unlock()

	// Iterative post-order DFS: recursion would follow worst-case linear
	// histories deeper than is comfortable for goroutine stacks.
	type frame struct {
		oid     model.OID
		parents []model.OID
		next    int
		tainted bool
	}
	var stack []*frame
	onStack := map[model.OID]bool{}
	push := func(o model.OID) {
		s.idxMu.Lock()
		_, resolved := s.idx[repo].gen[o]
		s.idxMu.Unlock()
		if resolved || onStack[o] {
			return
		}
		ci, err := s.CommitInfo(repo, o)
		if err != nil {
			return // missing object: gen stays unknown, never cached
		}
		onStack[o] = true
		stack = append(stack, &frame{oid: o, parents: ci.Parents})
	}
	push(oid)
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		if f.next < len(f.parents) {
			p := f.parents[f.next]
			f.next++
			s.idxMu.Lock()
			_, resolved := s.idx[repo].gen[p]
			s.idxMu.Unlock()
			if resolved || onStack[p] {
				continue
			}
			ci, err := s.CommitInfo(repo, p)
			if err != nil {
				f.tainted = true // unresolvable parent: this gen will be a lower bound
				continue
			}
			onStack[p] = true
			stack = append(stack, &frame{oid: p, parents: ci.Parents})
			continue
		}
		g := uint32(1)
		tainted := f.tainted
		s.idxMu.Lock()
		for _, p := range f.parents {
			pg, ok := s.idx[repo].gen[p]
			if !ok || s.idx[repo].genTainted[p] {
				tainted = true
				continue
			}
			if pg >= g {
				g = pg + 1
			}
		}
		s.idx[repo].gen[f.oid] = g
		if tainted {
			s.idx[repo].genTainted[f.oid] = true
		}
		s.idxMu.Unlock()
		onStack[f.oid] = false
		stack = stack[:len(stack)-1]
	}
	s.idxMu.Lock()
	g, resolved := s.idx[repo].gen[oid]
	tainted := s.idx[repo].genTainted[oid]
	s.idxMu.Unlock()
	if !resolved {
		return 0, false
	}
	return g, !tainted
}

// CommitsWithTree returns the commits that record this tree (reverse index).
// The slice is index-owned; callers must not modify it. Empty when unknown.
func (s *ObjectService) CommitsWithTree(repo string, tree model.OID) []model.OID {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if ri := s.idx[repo]; ri != nil {
		return ri.treeCommits[tree]
	}
	return nil
}
