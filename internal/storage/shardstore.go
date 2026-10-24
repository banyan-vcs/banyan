package storage

import (
	"bytes"
	"container/list"
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

	// Location cache (docs/12 P0-1, B1): oid → pack placement, LRU per repo.
	// Entries are valid until a repack moves the object: repack invalidates
	// the repo set, and a failed read triggers one invalidate + re-locate to
	// cover repacks initiated by another gateway.
	locMu   sync.Mutex
	loc     map[string]map[model.OID]ObjectLocation
	locLRU  map[string]*list.List
	locElem map[string]map[model.OID]*list.Element

	// Synthetic-object batching (docs/12 P0-2, B3): a weave materialization
	// buffers its synthetics and persists them as ONE pack at flush.
	// Refcounted per repo (nested view entries compose); reads stay correct —
	// the in-memory synth table updates immediately, only persistence defers.
	batchMu  sync.Mutex
	batchDep map[string]int
	batchBuf map[string]map[model.OID]synthObject
}

// locCacheMax bounds one repo's location cache (LRU eviction).
const locCacheMax = 65536

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
		Meta:     meta,
		Local:    local,
		Backend:  backend,
		Algo:     algo,
		cache:    map[string]map[model.OID]*DecodedPack{},
		packOrder: map[string]map[model.OID][]model.OID{},
		synth:    map[string]map[model.OID]synthObject{},
		idx:      map[string]*repoIndex{},
		loc:      map[string]map[model.OID]ObjectLocation{},
		locLRU:   map[string]*list.List{},
		locElem:  map[string]map[model.OID]*list.Element{},
		batchDep: map[string]int{},
		batchBuf: map[string]map[model.OID]synthObject{},
	}
}

// PutSynthetic materializes a server-synthesized object (idempotent:
// content-addressed). Write-through persists the object so timeline heads
// referencing synthetics (adoption merges, thin commits, projections)
// survive a restart — inside a synth batch (BeginSynthBatch) the
// persistence defers and the whole batch lands as one pack at flush
// (docs/12 P0-2); the in-memory table always updates immediately, so
// same-process reads (the fold's own ReplaceSubtree) see the object at
// once. Re-materialization dedups via the in-memory table; pack CAS keeps
// re-ingest idempotent. Per-object fragments are repack's consolidation job
// (docs/02 §5, M3).
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
	s.batchMu.Lock()
	deferred := s.batchDep[repo] > 0
	if deferred {
		if s.batchBuf[repo] == nil {
			s.batchBuf[repo] = map[model.OID]synthObject{}
		}
		s.batchBuf[repo][oid] = synthObject{Type: t, Data: data}
	}
	s.batchMu.Unlock()
	if deferred {
		return
	}
	s.persistSynthOne(repo, oid, t, data)
}

// BeginSynthBatch defers synthetic persistence for this repo (refcounted:
// nested view materializations compose into one batch).
func (s *ObjectService) BeginSynthBatch(repo string) {
	s.batchMu.Lock()
	s.batchDep[repo]++
	s.batchMu.Unlock()
}

// EndSynthBatch closes one BeginSynthBatch level; the final close flushes
// the remaining buffer as one pack.
func (s *ObjectService) EndSynthBatch(repo string) {
	s.batchMu.Lock()
	if s.batchDep[repo] <= 0 {
		s.batchMu.Unlock()
		return
	}
	s.batchDep[repo]--
	flush := s.batchDep[repo] == 0
	s.batchMu.Unlock()
	if flush {
		s.flushSynthBatch(repo)
	}
}

// FlushSynthBatch persists buffered synthetics now without changing the
// batch depth: the durability point before a timeline head CAS —
// registration is the fact, so a committed head must never reference
// objects that only exist in this process's memory (cluster: other
// gateways would miss them).
func (s *ObjectService) FlushSynthBatch(repo string) {
	s.flushSynthBatch(repo)
}

func (s *ObjectService) flushSynthBatch(repo string) {
	s.batchMu.Lock()
	buf := s.batchBuf[repo]
	delete(s.batchBuf, repo)
	s.batchMu.Unlock()
	if len(buf) == 0 {
		return
	}
	var blobs, trees, commits map[model.OID][]byte
	ensure := func() {
		if blobs == nil {
			blobs = map[model.OID][]byte{}
			trees = map[model.OID][]byte{}
			commits = map[model.OID][]byte{}
		}
	}
	for oid, so := range buf {
		ensure()
		switch so.Type {
		case model.BlobType:
			blobs[oid] = so.Data
		case model.TreeType:
			trees[oid] = so.Data
		default:
			commits[oid] = so.Data
		}
	}
	var pack bytes.Buffer
	if err := EncodePack(&pack, blobs, trees, commits, nil); err != nil {
		log.Printf("synth batch %s: encode: %v (per-object fallback)", repo, err)
		s.persistSynthEach(repo, buf)
		return
	}
	if _, err := s.IngestPack(context.Background(), repo, pack.Bytes()); err != nil {
		log.Printf("synth batch %s: persist: %v (per-object fallback)", repo, err)
		s.persistSynthEach(repo, buf)
	}
}

func (s *ObjectService) persistSynthEach(repo string, buf map[model.OID]synthObject) {
	for oid, so := range buf {
		s.persistSynthOne(repo, oid, so.Type, so.Data)
	}
}

func (s *ObjectService) persistSynthOne(repo string, oid model.OID, t model.ObjectType, data []byte) {
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

// --- location cache (docs/12 P0-1) ---

func (s *ObjectService) locHit(repo string, oid model.OID) (ObjectLocation, bool) {
	s.locMu.Lock()
	defer s.locMu.Unlock()
	if m := s.loc[repo]; m != nil {
		if loc, ok := m[oid]; ok {
			if el := s.locElem[repo][oid]; el != nil {
				s.locLRU[repo].MoveToFront(el)
			}
			return loc, true
		}
	}
	return ObjectLocation{}, false
}

func (s *ObjectService) locPut(repo string, oid model.OID, loc ObjectLocation) {
	s.locMu.Lock()
	defer s.locMu.Unlock()
	if s.loc[repo] == nil {
		s.loc[repo] = map[model.OID]ObjectLocation{}
		s.locElem[repo] = map[model.OID]*list.Element{}
		s.locLRU[repo] = list.New()
	}
	if _, exists := s.loc[repo][oid]; !exists {
		s.locElem[repo][oid] = s.locLRU[repo].PushFront(oid)
		if s.locLRU[repo].Len() > locCacheMax {
			if oldest := s.locLRU[repo].Back(); oldest != nil {
				evict := oldest.Value.(model.OID)
				s.locLRU[repo].Remove(oldest)
				delete(s.loc[repo], evict)
				delete(s.locElem[repo], evict)
			}
		}
	}
	s.loc[repo][oid] = loc
}

func (s *ObjectService) locInvalidate(repo string) {
	s.locMu.Lock()
	defer s.locMu.Unlock()
	delete(s.loc, repo)
	delete(s.locElem, repo)
	delete(s.locLRU, repo)
}

// locate resolves one object's placement, LRU-first (docs/12 P0-1).
func (s *ObjectService) locate(ctx context.Context, repo string, oid model.OID) (ObjectLocation, error) {
	if loc, ok := s.locHit(repo, oid); ok {
		return loc, nil
	}
	locs, err := s.Meta.LocateObjects(ctx, repo, []model.OID{oid})
	if err != nil {
		return ObjectLocation{}, err
	}
	loc, ok := locs[oid]
	if !ok {
		return ObjectLocation{}, fmt.Errorf("%w: %s in repo %s", model.ErrObjectNotFound, oid, repo)
	}
	s.locPut(repo, oid, loc)
	return loc, nil
}

// Prefetch batch-locates every not-yet-cached oid in ONE Meta round trip
// (docs/12 P0-1): tree-expansion points feed their children here, cutting
// the cluster's refs RPC count from O(objects) to O(trees).
func (s *ObjectService) Prefetch(ctx context.Context, repo string, oids []model.OID) {
	var missing []model.OID
	for _, oid := range oids {
		if _, ok := s.locHit(repo, oid); !ok {
			missing = append(missing, oid)
		}
	}
	if len(missing) == 0 {
		return
	}
	locs, err := s.Meta.LocateObjects(ctx, repo, missing)
	if err != nil {
		return // best-effort: individual locates will surface real errors
	}
	for _, oid := range missing {
		if loc, ok := locs[oid]; ok {
			s.locPut(repo, oid, loc)
		}
	}
}

// Known reports which of the oids the repo holds — one batched locate plus
// the in-memory synth table (the same-process view Raw would see). Used by
// the fetch negotiation paths (have-ACK checks): no object bytes fetched.
func (s *ObjectService) Known(ctx context.Context, repo string, oids []model.OID) map[model.OID]bool {
	out := map[model.OID]bool{}
	s.synthMu.Lock()
	synth := s.synth[repo]
	s.synthMu.Unlock()
	var missing []model.OID
	for _, oid := range oids {
		if synth != nil {
			if _, ok := synth[oid]; ok {
				out[oid] = true
				continue
			}
		}
		if _, ok := s.locHit(repo, oid); ok {
			out[oid] = true
			continue
		}
		missing = append(missing, oid)
	}
	if len(missing) == 0 {
		return out
	}
	locs, err := s.Meta.LocateObjects(ctx, repo, missing)
	if err != nil {
		return out
	}
	for _, oid := range missing {
		if loc, ok := locs[oid]; ok {
			out[oid] = true
			s.locPut(repo, oid, loc)
		}
	}
	return out
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

	loc, err := s.locate(ctx, repo, oid)
	if err != nil {
		return "", nil, err
	}
	t, data, err := s.objectFromPack(ctx, repo, oid, loc)
	if err == nil {
		return t, data, nil
	}
	// Stale placement (a repack — possibly on another gateway — moved the
	// object, or the pack's decoded cache held a moved-away oid): drop the
	// repo's location cache and retry exactly once with fresh placements.
	s.locInvalidate(repo)
	loc, err = s.locate(ctx, repo, oid)
	if err != nil {
		return "", nil, err
	}
	return s.objectFromPack(ctx, repo, oid, loc)
}

func objectFromDecoded(dp *DecodedPack, oid model.OID) (model.ObjectType, []byte, error) {
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

// objectFromPack reads one object out of its located pack: decoded-cache
// fast path, single-object extraction (docs/12 P1-1), then whole-pack fetch
// + decode as the fallback (idx-less packs, old nodes, extraction errors).
func (s *ObjectService) objectFromPack(ctx context.Context, repo string, oid model.OID, loc ObjectLocation) (model.ObjectType, []byte, error) {
	s.mu.Lock()
	dp := s.cache[repo][loc.PackOID]
	s.mu.Unlock()
	if dp != nil {
		return objectFromDecoded(dp, oid)
	}
	holders := s.packHolders(ctx, repo, loc.PackOID, loc.NodeID)
	if t, data, err := s.Backend.GetObject(ctx, repo, loc.PackOID, oid, holders, s.Algo); err == nil {
		return t, data, nil
	}
	raw, err := s.Backend.GetPack(ctx, repo, loc.PackOID, holders)
	if err != nil {
		return "", nil, err
	}
	dp, err = DecodePack(raw)
	if err != nil {
		return "", nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, loc.PackOID, err)
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
	return objectFromDecoded(dp, oid)
}

// IngestPack handles one pushed pack (docs/02 §4 write path, docs/11 §5):
// decode → backend placement/persist (returns the CONFIRMED holder set C) →
// metadata registration (registration is the fact) → build in-memory indexes.
func (s *ObjectService) IngestPack(ctx context.Context, repo string, packBytes []byte) (model.OID, error) {
	dp, err := DecodePack(packBytes)
	if err != nil {
		return "", err
	}
	// idx v2: written when the observer collected full offset/crc data.
	var idxBytes []byte
	if len(dp.Offsets) == len(dp.Order) {
		if b, err := EncodeIdx(s.Algo, dp.PackOID, dp.Offsets, dp.CRCs); err == nil {
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
// Single-object reads go through GetObject instead (docs/12 P1-1): idx offset
// + delta-chain inflate, no whole-pack decode.
func (l *LocalShardStore) GetDecoded(repo string, packOID model.OID) (*DecodedPack, error) {
	raw, err := l.GetPackBytes(repo, packOID)
	if err != nil {
		return nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, packOID, err)
	}
	return DecodePack(raw)
}

// GetObject extracts one object from a stored pack via its idx v2 (docs/12
// P1-1): header parse at the located offset + delta-chain inflate. A missing
// or unusable idx surfaces as an error — callers fall back to the whole-pack
// path.
func (l *LocalShardStore) GetObject(repo string, packOID, oid model.OID, algo model.HashAlgo) (model.ObjectType, []byte, error) {
	idxRaw, err := os.ReadFile(l.idxPath(repo, packOID))
	if err != nil {
		return "", nil, fmt.Errorf("%w: idx of %s: %v", model.ErrObjectNotFound, packOID, err)
	}
	pi, err := DecodeIdx(idxRaw, algo)
	if err != nil {
		return "", nil, err
	}
	raw, err := l.GetPackBytes(repo, packOID)
	if err != nil {
		return "", nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, packOID, err)
	}
	return ExtractObject(raw, pi, algo, oid)
}
