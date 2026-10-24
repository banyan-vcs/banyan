package storage

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
)

// ObjectService orchestrates the read path (docs/02 §3):
// locate (global index) → local cache → cross-node fetch → decode objects.
// It also implements model.ObjectLookup for direct model-layer assembly/projection use.
type ObjectService struct {
	Meta  MetaStore
	Local *LocalShardStore // all-in-one shard store (nil in gateway mode; tests may use it directly)
	Algo  model.HashAlgo

	// Backend owns where pack bytes live (docs/11 §3): local disk in
	// all-in-one, the scattered fleet in cluster mode.
	Backend ShardBackend

	mu        sync.Mutex
	cache     map[string]map[model.OID]*DecodedPack // repo → packOID → decoded result (LRU is M2's business)
	packOrder map[string]map[model.OID][]model.OID  // repo → packOID → object list (whole-pack direct serve detection)

	synthMu sync.Mutex
	synth   map[string]map[model.OID]synthObject // repo → oid → server-synthesized object (projection commits etc., docs/01 §5.2)

	idxMu sync.Mutex
	idx   map[string]*repoIndex // repo → in-memory derived index (docs/02 §1.3 / docs/04)
}

// synthObject is a server-synthesized object (not from push), e.g. a subtree projection commit.
type synthObject struct {
	Type model.ObjectType
	Data []byte
}

func NewObjectService(meta MetaStore, local *LocalShardStore, nodes []Node, algo model.HashAlgo) *ObjectService {
	nodeID := "local-1"
	if len(nodes) > 0 {
		nodeID = nodes[0].ID
	}
	return newObjectService(meta, local, &LocalBackend{Local: local, NodeID: nodeID}, algo)
}

// NewObjectServiceWithBackend wires a cluster-mode service (gateway): the
// backend is the scattered store, Local stays nil.
func NewObjectServiceWithBackend(meta MetaStore, backend ShardBackend, algo model.HashAlgo) *ObjectService {
	return newObjectService(meta, nil, backend, algo)
}

func newObjectService(meta MetaStore, local *LocalShardStore, backend ShardBackend, algo model.HashAlgo) *ObjectService {
	return &ObjectService{
		Meta:      meta,
		Local:     local,
		Backend:   backend,
		Algo:      algo,
		cache:     map[string]map[model.OID]*DecodedPack{},
		packOrder: map[string]map[model.OID][]model.OID{},
		synth:     map[string]map[model.OID]synthObject{},
		idx:       map[string]*repoIndex{},
	}
}

// PutSynthetic materializes a server-synthesized object (idempotent:
// content-addressed). Write-through: the first write also persists the object
// as a single-object pack, so timeline heads referencing synthetics (adoption
// merges, thin commits, projections) survive a restart. Re-materialization
// dedups via the in-memory table; pack CAS keeps re-ingest idempotent. The
// per-object fragments are repack's consolidation job (docs/02 §5, M3).
func (s *ObjectService) PutSynthetic(repo string, oid model.OID, t model.ObjectType, data []byte) {
	s.synthMu.Lock()
	if s.synth[repo] == nil {
		s.synth[repo] = map[model.OID]synthObject{}
	}
	_, exists := s.synth[repo][oid]
	s.synth[repo][oid] = synthObject{Type: t, Data: data}
	s.synthMu.Unlock()
	if exists {
		return
	}
	var blobs, trees, commits map[model.OID][]byte
	switch t {
	case model.BlobType:
		blobs = map[model.OID][]byte{oid: data}
	case model.TreeType:
		trees = map[model.OID][]byte{oid: data}
	default:
		commits = map[model.OID][]byte{oid: data}
	}
	var buf bytes.Buffer
	if err := EncodePack(&buf, blobs, trees, commits, nil); err != nil {
		log.Printf("synth %s/%s: encode: %v (memory-only)", repo, oid, err)
		return
	}
	if _, err := s.IngestPack(context.Background(), repo, buf.Bytes()); err != nil {
		log.Printf("synth %s/%s: persist: %v (memory-only)", repo, oid, err)
	}
}

// Raw is the unified read: synthetic objects first, then pack location via
// the global index. Returns the object type and raw encoded bytes.
func (s *ObjectService) Raw(ctx context.Context, repo string, oid model.OID) (model.ObjectType, []byte, error) {
	s.synthMu.Lock()
	if so, ok := s.synth[repo][oid]; ok {
		s.synthMu.Unlock()
		return so.Type, so.Data, nil
	}
	s.synthMu.Unlock()

	dp, err := s.lookupPack(ctx, repo, oid)
	if err != nil {
		return "", nil, err
	}
	if b, ok := dp.Blobs[oid]; ok {
		return model.BlobType, b, nil
	}
	if t, ok := dp.Trees[oid]; ok {
		return model.TreeType, t, nil
	}
	if c, ok := dp.Commits[oid]; ok {
		return model.CommitType, c, nil
	}
	if tg, ok := dp.Tags[oid]; ok {
		return model.TagType, tg, nil
	}
	return "", nil, fmt.Errorf("%w: %s", model.ErrObjectNotFound, oid)
}

// IngestPack handles one pushed pack (docs/02 §4 write path, docs/11 §5):
// decode → backend placement/persist (returns the CONFIRMED holder set C) →
// metadata registration (registration is the fact) → build in-memory indexes.
func (s *ObjectService) IngestPack(ctx context.Context, repo string, packBytes []byte) (model.OID, error) {
	dp, err := DecodePack(packBytes)
	if err != nil {
		return "", err
	}
	// idx v2: written only when offsets pair up fully; crc section stays 0 for now (TODO(M2), see idx.go).
	var idxBytes []byte
	if len(dp.Offsets) == len(dp.Order) {
		if b, err := EncodeIdx(s.Algo, dp.PackOID, dp.Offsets); err == nil {
			idxBytes = b
		} else {
			log.Printf("ingest %s: idx encode failed (skip): %v", repo, err)
		}
	}
	confirmed, err := s.Backend.PutPack(ctx, repo, dp.PackOID, packBytes, idxBytes)
	if err != nil {
		return "", err
	}
	if err := s.Meta.PutPack(ctx, repo, dp.PackOID, dp.Order, confirmed); err != nil {
		return "", err
	}
	s.buildIndex(repo, dp)
	if n, unique, fails := s.CommitIndexStats(repo); n > 0 {
		log.Printf("index %s: lightweight commits %d → %d unique bmsg (%.1f%% dedup), render-fails %d",
			repo, n, unique, 100*float64(n-unique)/float64(n), fails)
	}

	s.mu.Lock()
	if s.cache[repo] == nil {
		s.cache[repo] = map[model.OID]*DecodedPack{}
	}
	s.cache[repo][dp.PackOID] = dp
	if s.packOrder[repo] == nil {
		s.packOrder[repo] = map[model.OID][]model.OID{}
	}
	s.packOrder[repo][dp.PackOID] = dp.Order
	s.mu.Unlock()
	return dp.PackOID, nil
}

// ExactPackHit implements the hot-path whole-pack direct serve (docs/02 §5):
// when the requested object set exactly equals a stored pack's object set it
// returns that pack OID; the caller can stream the pack bytes verbatim, skipping online re-encoding (re-hash + re-compress).
func (s *ObjectService) ExactPackHit(repo string, oids map[model.OID]bool) (model.OID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for packOID, order := range s.packOrder[repo] {
		if len(order) != len(oids) {
			continue
		}
		match := true
		for _, oid := range order {
			if !oids[oid] {
				match = false
				break
			}
		}
		if match {
			return packOID, true
		}
	}
	return "", false
}

// --- model.ObjectLookup implementation (the model layer reads objects through this) ---

func (s *ObjectService) lookupPack(ctx context.Context, repo string, oid model.OID) (*DecodedPack, error) {
	locs, err := s.Meta.LocateObjects(ctx, repo, []model.OID{oid})
	if err != nil {
		return nil, err
	}
	loc, ok := locs[oid]
	if !ok {
		return nil, fmt.Errorf("%w: %s in repo %s", model.ErrObjectNotFound, oid, repo)
	}
	s.mu.Lock()
	dp := s.cache[repo][loc.PackOID]
	s.mu.Unlock()
	if dp != nil {
		return dp, nil
	}
	holders := s.packHolders(ctx, repo, loc.PackOID, loc.NodeID)
	raw, err := s.Backend.GetPack(ctx, repo, loc.PackOID, holders)
	if err != nil {
		return nil, err
	}
	dp, err = DecodePack(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, loc.PackOID, err)
	}
	s.mu.Lock()
	if s.cache[repo] == nil {
		s.cache[repo] = map[model.OID]*DecodedPack{}
	}
	s.cache[repo][loc.PackOID] = dp
	if s.packOrder[repo] == nil {
		s.packOrder[repo] = map[model.OID][]model.OID{}
	}
	s.packOrder[repo][loc.PackOID] = dp.Order
	s.mu.Unlock()
	return dp, nil
}

// packHolders resolves a pack's full holder set from the pack registry,
// falling back to the single location hint when the pack is not registered
// (should not happen — registration precedes visibility).
func (s *ObjectService) packHolders(ctx context.Context, repo string, packOID model.OID, hint string) []string {
	if info, err := s.Meta.GetPack(ctx, repo, packOID); err == nil {
		return info.Nodes
	}
	if hint != "" {
		return []string{hint}
	}
	return nil
}

// PackBytes fetches raw pack bytes (whole-pack direct serve, repack input),
// routed through the backend with holder fallback.
func (s *ObjectService) PackBytes(ctx context.Context, repo string, packOID model.OID) ([]byte, error) {
	return s.Backend.GetPack(ctx, repo, packOID, s.packHolders(ctx, repo, packOID, ""))
}

func (s *ObjectService) BlobOrTreeOrCommit(ctx context.Context, repo string, oid model.OID) ([]byte, error) {
	_, data, err := s.Raw(ctx, repo, oid)
	return data, err
}

// repoLookup binds a per-repo ObjectLookup view for the model layer.
type repoLookup struct {
	svc  *ObjectService
	repo string
}

func (s *ObjectService) Lookup(repo string) model.ObjectLookup {
	return &repoLookup{svc: s, repo: repo}
}

// TreeStoreFor returns a writable repo view (for assembly, docs/01 §4):
// reads go through cache/packs; PutTree persists assembly-produced new trees into the synthetic-object layer.
func (s *ObjectService) TreeStoreFor(repo string) model.TreeStore {
	return &synthTreeStore{lookup: s.Lookup(repo), svc: s, repo: repo}
}

type synthTreeStore struct {
	lookup model.ObjectLookup
	svc    *ObjectService
	repo   string
}

func (a *synthTreeStore) Blob(oid model.OID) ([]byte, error)      { return a.lookup.Blob(oid) }
func (a *synthTreeStore) Tree(oid model.OID) (*model.Tree, error) { return a.lookup.Tree(oid) }
func (a *synthTreeStore) Commit(oid model.OID) (*model.AnchoredCommit, error) {
	return a.lookup.Commit(oid)
}

func (a *synthTreeStore) PutTree(t *model.Tree) model.OID {
	encoded, err := t.Encode()
	if err != nil {
		return ""
	}
	oid := t.OID(a.svc.Algo)
	a.svc.PutSynthetic(a.repo, oid, model.TreeType, encoded)
	return oid
}

func (r *repoLookup) Blob(oid model.OID) ([]byte, error) {
	t, data, err := r.svc.Raw(context.Background(), r.repo, oid)
	if err != nil {
		return nil, err
	}
	if t != model.BlobType {
		return nil, fmt.Errorf("%w: %s is %s, want blob", model.ErrObjectNotFound, oid, t)
	}
	return data, nil
}

// Tree goes through the tree-entry index (docs/02 §1.3): parse once, memoize
// forever; repeated path walks in projection/closure traversal all hit the cache.
// The returned slice is index-owned; callers must not modify it.
func (r *repoLookup) Tree(oid model.OID) (*model.Tree, error) {
	entries, err := r.svc.TreeEntries(r.repo, oid)
	if err != nil {
		return nil, err
	}
	return &model.Tree{Entries: entries}, nil
}

func (r *repoLookup) Commit(oid model.OID) (*model.AnchoredCommit, error) {
	t, data, err := r.svc.Raw(context.Background(), r.repo, oid)
	if err != nil {
		return nil, err
	}
	if t != model.CommitType {
		return nil, fmt.Errorf("%w: %s is %s, want commit", model.ErrObjectNotFound, oid, t)
	}
	return model.DecodeCommit(data)
}

// LocalShardStore is the M1 local-disk shard store:
// <root>/<repo>/packs/<first two oid hex>/pack-<oid>.pack + .idx (git idx v2).
type LocalShardStore struct {
	Root string
}

func NewLocalShardStore(root string) *LocalShardStore { return &LocalShardStore{Root: root} }

func (l *LocalShardStore) packPath(repo string, packOID model.OID) string {
	s := string(packOID)
	return filepath.Join(l.Root, repo, "packs", s[:2], "pack-"+s+".pack")
}

func (l *LocalShardStore) idxPath(repo string, packOID model.OID) string {
	s := string(packOID)
	return filepath.Join(l.Root, repo, "packs", s[:2], "pack-"+s+".idx")
}

// PutPack persists raw pack bytes and idx v2 (pack only when idx is empty).
func (l *LocalShardStore) PutPack(repo string, packOID model.OID, packBytes, idxBytes []byte) error {
	paths := []string{l.packPath(repo, packOID)}
	if len(idxBytes) > 0 {
		paths = append(paths, l.idxPath(repo, packOID))
	}
	for i, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		data := packBytes
		if i == 1 {
			data = idxBytes
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// GetPackBytes reads raw pack bytes.
func (l *LocalShardStore) GetPackBytes(repo string, packOID model.OID) ([]byte, error) {
	return os.ReadFile(l.packPath(repo, packOID))
}

// DeletePack removes the pack and idx files from disk (repack old-shard
// cleanup; missing files are tolerated — CAS re-ingest may have skipped writes).
func (l *LocalShardStore) DeletePack(repo string, packOID model.OID) {
	_ = os.Remove(l.packPath(repo, packOID))
	_ = os.Remove(l.idxPath(repo, packOID))
}

// GetDecoded reads back and decodes a pack (miss path; object bodies then stay in cache).
// TODO(M2): idx offset lookup → single-object inflate, avoiding a full-pack decode.
func (l *LocalShardStore) GetDecoded(repo string, packOID model.OID) (*DecodedPack, error) {
	raw, err := l.GetPackBytes(repo, packOID)
	if err != nil {
		return nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, packOID, err)
	}
	return DecodePack(raw)
}
