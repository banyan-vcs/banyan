package storage

import (
	"bytes"
	"context"

	"github.com/banyan-vcs/banyan/internal/model"
)

// In-memory derived indexes (complement to the docs/02 §1.3 global index + docs/04 commit-graph):
//
//   - CommitGraph: oid → (tree, parents, when); closure computation and
//     projection traversal no longer decode raw commit bytes per object; built
//     incrementally at push (objects are already decoded in IngestPack — zero extra IO).
//   - TreeMemo: oid → parsed entry table; path walking (SubtreeAt/closure
//     expansion) parses each tree exactly once.
//
// Lifetime matches the process (MetaStore in-memory; M2 persists it with metadata).

// CommitInfo is one commit-graph record.
type CommitInfo struct {
	Tree    model.OID
	Parents []model.OID
	When    int64 // committer unix time (for ordering/generation)
}

type repoIndex struct {
	commits map[model.OID]*CommitInfo
	trees   map[model.OID][]model.TreeEntry
	// Lightweight commit index (docs/01 §11, design v3): git oid → (tree edge, bmsg ref).
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
			ri.commits[oid] = &CommitInfo{Tree: c.TreeOID, Parents: c.Parents, When: c.Committer.When.Unix()}
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
		if t, err := model.DecodeTree(raw); err == nil {
			ri.trees[oid] = t.Entries
		}
	}
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
	ci := &CommitInfo{Tree: c.TreeOID, Parents: c.Parents, When: c.Committer.When.Unix()}
	s.idxMu.Lock()
	if s.idx[repo] == nil {
		s.idx[repo] = newRepoIndex()
	}
	s.idx[repo].commits[oid] = ci
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
	tree, err := model.DecodeTree(raw)
	if err != nil {
		return nil, err
	}
	s.idxMu.Lock()
	if s.idx[repo] == nil {
		s.idx[repo] = newRepoIndex()
	}
	s.idx[repo].trees[oid] = tree.Entries
	s.idxMu.Unlock()
	return tree.Entries, nil
}

// newRepoIndex is the single constructor — every map must exist regardless of
// which path creates the entry first (cold-start backfill vs push-side
// buildIndex), or the later writer hits a nil map.
func newRepoIndex() *repoIndex {
	return &repoIndex{
		commits: map[model.OID]*CommitInfo{},
		trees:   map[model.OID][]model.TreeEntry{},
		records: map[model.OID]*model.CommitRecord{},
		msgs:    map[model.OID][]byte{},
	}
}
