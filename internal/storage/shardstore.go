package storage

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
)

// ObjectService orchestrates the read path (docs/03 §3):
// locate (global index) → local cache → cross-node fetch → decode objects.
// It also implements model.ObjectLookup for direct model-layer assembly/projection use.
type ObjectService struct {
	Meta  MetaStore
	Local *LocalShardStore // all-in-one shard store (nil in gateway mode; tests may use it directly)
	Algo  model.HashAlgo

	// Backend owns where pack bytes live (docs/10 §3): local disk in
	// all-in-one, the scattered fleet in cluster mode.
	Backend ShardBackend

	// Addressing, when set to hashed mode, maps repo names to opaque
	// storage keys at this one seam (docs/03 §6): everything below the
	// ObjectService — backend wire calls and on-disk layouts — speaks keys,
	// never names. Nil = named addressing (the repo name IS the key).
	Addressing *Addressing

	mu         sync.Mutex
	cache      map[string]map[model.OID]*DecodedPack // repo → packOID → decoded result
	cacheLRU   *list.List                            // decoded-pack LRU, byte-budgeted (see MemoMaxBytes)
	cacheBytes int64                                 // total decoded bytes memoized
	packOrder  map[string]map[model.OID][]model.OID  // repo → packOID → object list (whole-pack direct serve detection)
	sizeKnown  map[string]int64                      // repo/packOID → byte size (immutable; whole-pack memoize guard)

	// MemoMaxBytes bounds the total decoded-pack memory (LRU eviction past
	// it); 0 = decodedMemoMaxBytes. Per-pack entry is already gated by
	// packMemoMaxBytes — this is the fleet-wide cap.
	MemoMaxBytes int64

	synthMu sync.Mutex
	synth   map[string]map[model.OID]synthObject // repo → oid → server-synthesized object (projection commits etc., docs/02 §5.2)

	idxMu sync.Mutex
	idx   map[string]*repoIndex // repo → in-memory derived index (docs/03 §1.3 / docs/08)

	// Location cache (docs/11 P0-1, B1): oid → pack placement, LRU per repo.
	// Entries are valid until a repack moves the object: repack invalidates
	// the repo set, and a failed read triggers one invalidate + re-locate to
	// cover repacks initiated by another gateway.
	locMu   sync.Mutex
	loc     map[string]map[model.OID]ObjectLocation
	locLRU  map[string]*list.List
	locElem map[string]map[model.OID]*list.Element

	// Synthetic-object batching (docs/11 P0-2, B3): a weave materialization
	// buffers its synthetics and persists them as ONE pack at flush.
	// Refcounted per repo (nested view entries compose); reads stay correct —
	// the in-memory synth table updates immediately, only persistence defers.
	batchMu  sync.Mutex
	batchDep map[string]int
	batchBuf map[string]map[model.OID]synthObject

	// Clone-shaped response pack cache (docs/11 P2-2, B9): a subtree (or
	// woven-root) clone's closure never equals a stored pack — projection and
	// weave OIDs are synthetic — so ExactPackHit cannot fire and every clone
	// re-encodes online. With no haves, no shallowing and no filter the
	// response pack is a pure function of the wants, so it caches verbatim:
	// tips change → new wants → new key; object graphs are immutable, so
	// entries never invalidate (LRU by bytes only).
	respMu     sync.Mutex
	respCache  map[closureKey]*list.Element
	respLRU    *list.List
	respBytes  int64
	respHits   int
	respStores int

	// Read-amplification watch (docs/11 P2-1): consecutive single-object
	// reads of one pack (a cold gateway's closure walk) switch to ONE
	// whole-pack fetch after packReadAmplify of them — memoized for the
	// rest of the walk and persisted to the gateway disk cache.
	readMu  sync.Mutex
	readAmp map[string]int // "<repo>/<packOID>" → single-object reads so far
}

// packReadAmplify is the single-object-read threshold that flips a pack to
// the whole-pack path.
const packReadAmplify = 8

// packMemoMaxBytes caps which packs may ride the whole-pack decode-and-
// memoize ladder: above it a single object read must stay on per-object
// extraction — parking a multi-hundred-MB pack in gateway memory is exactly
// what the streaming chain exists to avoid.
const packMemoMaxBytes = 32 << 20

// decodedMemoMaxBytes bounds the TOTAL decoded-pack memory — without it, a
// repo of many just-under-the-cap packs memoizes its way past any budget.
const decodedMemoMaxBytes = 256 << 20

// ResponsePackCacheMax bounds the clone-shaped response cache (total bytes,
// LRU). Responses whose closure exceeds it skip the cache AND the
// whole-response buffer (gitwire streams them straight to the wire).
const ResponsePackCacheMax = 128 << 20

type closureKey struct {
	repo  string
	sub   string
	wants string // sorted, joined — the pack's complete input
	ofs   bool   // OFS_DELTA vs REF_DELTA encoding
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
		Meta:      meta,
		Local:     local,
		Backend:   backend,
		Algo:      algo,
		cache:     map[string]map[model.OID]*DecodedPack{},
		cacheLRU:  list.New(),
		packOrder: map[string]map[model.OID][]model.OID{},
		synth:     map[string]map[model.OID]synthObject{},
		idx:       map[string]*repoIndex{},
		loc:       map[string]map[model.OID]ObjectLocation{},
		locLRU:    map[string]*list.List{},
		locElem:   map[string]map[model.OID]*list.Element{},
		batchDep:  map[string]int{},
		batchBuf:  map[string]map[model.OID]synthObject{},
		respCache: map[closureKey]*list.Element{},
		respLRU:   list.New(),
		readAmp:   map[string]int{},
	}
}

// closureKeyOf builds the response-cache key: the wants fully determine the
// pack for the clone shape (sorted so key order is canonical).
func closureKeyOf(repo string, sub model.AnchorPath, wants []model.OID, ofsDelta bool) closureKey {
	sorted := append([]model.OID(nil), wants...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return closureKey{repo: repo, sub: string(sub), ofs: ofsDelta,
		wants: strings.Join(func() []string {
			out := make([]string, len(sorted))
			for i, oid := range sorted {
				out[i] = string(oid)
			}
			return out
		}(), ",")}
}

// CachedResponsePack returns the cached pack for a clone-shaped closure.
func (s *ObjectService) CachedResponsePack(repo string, sub model.AnchorPath, wants []model.OID, ofsDelta bool) ([]byte, bool) {
	key := closureKeyOf(repo, sub, wants, ofsDelta)
	s.respMu.Lock()
	defer s.respMu.Unlock()
	el, ok := s.respCache[key]
	if !ok {
		return nil, false
	}
	s.respLRU.MoveToFront(el)
	s.respHits++
	return el.Value.(closureEntry).raw, true
}

type closureEntry struct {
	key closureKey
	raw []byte
}

// StoreResponsePack caches a clone-shaped response pack (LRU by bytes).
func (s *ObjectService) StoreResponsePack(repo string, sub model.AnchorPath, wants []model.OID, ofsDelta bool, raw []byte) {
	key := closureKeyOf(repo, sub, wants, ofsDelta)
	s.respMu.Lock()
	defer s.respMu.Unlock()
	if el, ok := s.respCache[key]; ok {
		s.respLRU.MoveToFront(el)
		return
	}
	s.respCache[key] = s.respLRU.PushFront(closureEntry{key: key, raw: raw})
	s.respBytes += int64(len(raw))
	s.respStores++
	for s.respBytes > ResponsePackCacheMax && s.respLRU.Len() > 1 {
		oldest := s.respLRU.Back()
		if oldest == nil {
			break
		}
		evicted := oldest.Value.(closureEntry)
		s.respLRU.Remove(oldest)
		delete(s.respCache, evicted.key)
		s.respBytes -= int64(len(evicted.raw))
	}
}

// ResponseCacheStats reports (hits, stores, entries) — observability + tests.
func (s *ObjectService) ResponseCacheStats() (hits, stores, entries int) {
	s.respMu.Lock()
	defer s.respMu.Unlock()
	return s.respHits, s.respStores, s.respLRU.Len()
}

// PutSynthetic materializes a server-synthesized object (idempotent:
// content-addressed). Write-through persists the object so timeline heads
// referencing synthetics (adoption merges, thin commits, projections)
// survive a restart — inside a synth batch (BeginSynthBatch) the
// persistence defers and the whole batch lands as one pack at flush
// (docs/11 P0-2); the in-memory table always updates immediately, so
// same-process reads (the fold's own ReplaceSubtree) see the object at
// once. Re-materialization dedups via the in-memory table; pack CAS keeps
// re-ingest idempotent. Per-object fragments are repack's consolidation job
// (docs/03 §5, M3).
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

// --- location cache (docs/11 P0-1) ---

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
	delete(s.loc, repo)
	delete(s.locElem, repo)
	delete(s.locLRU, repo)
	s.locMu.Unlock()
	s.readMu.Lock()
	for k := range s.readAmp {
		if r, _, ok := strings.Cut(k, "/"); ok && r == repo {
			delete(s.readAmp, k)
		}
	}
	s.readMu.Unlock()
}

// locate resolves one object's placement, LRU-first (docs/11 P0-1).
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
// (docs/11 P0-1): tree-expansion points feed their children here, cutting
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
// fast path, then the cheap whole-pack path (a gateway disk-cache hit — one
// decode memoizes the pack, docs/11 P2-1), then single-object extraction
// (docs/11 P1-1), then the whole-pack network fetch + decode as the final
// fallback (idx-less packs, old nodes, extraction errors).
func (s *ObjectService) objectFromPack(ctx context.Context, repo string, oid model.OID, loc ObjectLocation) (model.ObjectType, []byte, error) {
	s.mu.Lock()
	dp := s.cache[repo][loc.PackOID]
	s.mu.Unlock()
	if dp != nil {
		return objectFromDecoded(dp, oid)
	}
	key, kerr := s.storeKey(ctx, repo)
	if kerr != nil {
		return "", nil, kerr
	}
	holders := s.packHolders(ctx, repo, loc.PackOID, loc.NodeID)
	if raw, ok := s.Backend.CheapPack(ctx, key, loc.PackOID, holders); ok {
		if decoded, derr := DecodePack(raw); derr == nil {
			s.memoizeDecoded(repo, loc.PackOID, decoded)
			return objectFromDecoded(decoded, oid)
		}
	}
	if s.packTooBigToMemo(ctx, repo, loc.PackOID) {
		// A big pack rides ONLY the single-object extraction path: the
		// whole-pack fetch + decode + memoize below would park the entire
		// pack in gateway memory — exactly what the streaming chain avoids.
		t, data, gerr := s.Backend.GetObject(ctx, key, loc.PackOID, oid, holders, s.Algo)
		if gerr != nil {
			return "", nil, fmt.Errorf("%w: %s (big pack %s, extraction failed: %v)",
				model.ErrObjectNotFound, oid, loc.PackOID, gerr)
		}
		return t, data, nil
	}
	// Single-object extraction first. Escalation to the whole-pack fetch +
	// memoize below is cluster-only (PrefersWholePack): there each read is
	// an RPC round trip and one fetch beats a closure walk of them. The
	// local backend never escalates — extraction is a file read, and the
	// escalation would park the pack in memory for nothing (the T50
	// observation: a clone-shaped probe decoding a 20 MiB pack whole to
	// learn three tree shapes).
	if !s.Backend.PrefersWholePack() || !s.ampRead(repo, loc.PackOID) {
		if t, data, err := s.Backend.GetObject(ctx, key, loc.PackOID, oid, holders, s.Algo); err == nil {
			return t, data, nil
		}
	}
	// Amplified on the cluster wire (a closure walk: one whole-pack fetch
	// memoizes everything and lands in the gateway disk cache), or local
	// extraction failed (idx-less pack, old node): whole-pack fallback.
	raw, err := s.Backend.GetPack(ctx, key, loc.PackOID, holders)
	if err != nil {
		return "", nil, err
	}
	dp, err = DecodePack(raw)
	if err != nil {
		return "", nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, loc.PackOID, err)
	}
	s.memoizeDecoded(repo, loc.PackOID, dp)
	return objectFromDecoded(dp, oid)
}

// ampRead records one single-object read of a pack and reports whether the
// whole-pack path should take over.
func (s *ObjectService) ampRead(repo string, packOID model.OID) bool {
	key := repo + "/" + string(packOID)
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.readAmp[key]++
	return s.readAmp[key] >= packReadAmplify
}

// packSizeCached reports a pack's byte size, probing the backend once and
// caching it forever (content addressing makes sizes immutable). -1 =
// unknown (old nodes) — callers treat unknown per their own policy.
func (s *ObjectService) packSizeCached(ctx context.Context, repo string, packOID model.OID) int64 {
	if packOID == "" {
		return -1 // the zero OID marks "not a pack" (the synthetic group), never a probe
	}
	ck := repo + "/" + string(packOID)
	s.mu.Lock()
	if v, ok := s.sizeKnown[ck]; ok {
		s.mu.Unlock()
		return v
	}
	s.mu.Unlock()
	key, err := s.storeKey(ctx, repo)
	if err != nil {
		return -1
	}
	size, err := s.Backend.PackSize(ctx, key, packOID, s.packHolders(ctx, repo, packOID, ""))
	if err != nil || size < 0 {
		return -1
	}
	s.mu.Lock()
	if s.sizeKnown == nil {
		s.sizeKnown = map[string]int64{}
	}
	s.sizeKnown[ck] = size
	s.mu.Unlock()
	return size
}

// packTooBigToMemo reports whether the pack exceeds the whole-pack memoize
// cap (objectFromPack guard). Unknown (-1: old nodes) answers false so
// legacy deployments keep the old ladder.
func (s *ObjectService) packTooBigToMemo(ctx context.Context, repo string, packOID model.OID) bool {
	size := s.packSizeCached(ctx, repo, packOID)
	return size > packMemoMaxBytes
}

// memoizedPack keys the decoded-pack LRU (repo + packOID: packOID alone is
// globally unique, the repo keeps the accounting readable).
type memoizedPack struct {
	repo string
	pack model.OID
	dp   *DecodedPack
	size int64
}

func decodedPackBytes(dp *DecodedPack) int64 {
	var n int64
	for _, b := range dp.Blobs {
		n += int64(len(b))
	}
	for _, b := range dp.Trees {
		n += int64(len(b))
	}
	for _, b := range dp.Commits {
		n += int64(len(b))
	}
	for _, b := range dp.Tags {
		n += int64(len(b))
	}
	return n
}

// memoizeDecoded caches a decoded pack under the byte-budgeted LRU: past
// MemoMaxBytes the least recently used packs drop (reads re-fetch on miss —
// correctness never depends on the cache).
func (s *ObjectService) memoizeDecoded(repo string, packOID model.OID, dp *DecodedPack) {
	entry := memoizedPack{repo: repo, pack: packOID, dp: dp, size: decodedPackBytes(dp)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetDecodedLocked(repo, packOID)
	if s.cache[repo] == nil {
		s.cache[repo] = map[model.OID]*DecodedPack{}
	}
	s.cache[repo][packOID] = dp
	if s.cacheLRU == nil {
		s.cacheLRU = list.New()
	}
	s.cacheLRU.PushFront(entry)
	s.cacheBytes += entry.size
	for s.cacheBytes > s.memoMax() && s.cacheLRU.Len() > 1 {
		oldest := s.cacheLRU.Back()
		if oldest == nil {
			break
		}
		old := oldest.Value.(memoizedPack)
		s.cacheLRU.Remove(oldest)
		if m := s.cache[old.repo]; m != nil {
			delete(m, old.pack)
		}
		s.cacheBytes -= old.size
	}
	// packOrder feeds ExactPackHit and is metadata-only (OID list): it stays
	// regardless of eviction — the pack itself doesn't move.
	if s.packOrder[repo] == nil {
		s.packOrder[repo] = map[model.OID][]model.OID{}
	}
	s.packOrder[repo][packOID] = dp.Order
}

func (s *ObjectService) memoMax() int64 {
	if s.MemoMaxBytes > 0 {
		return s.MemoMaxBytes
	}
	return decodedMemoMaxBytes
}

// forgetDecoded drops a pack from the decoded cache + LRU accounting
// (repack shard cleanup).
func (s *ObjectService) forgetDecoded(repo string, packOID model.OID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetDecodedLocked(repo, packOID)
}

func (s *ObjectService) forgetDecodedLocked(repo string, packOID model.OID) {
	if m := s.cache[repo]; m != nil {
		if dp, ok := m[packOID]; ok {
			delete(m, packOID)
			if s.cacheLRU != nil {
				for e := s.cacheLRU.Front(); e != nil; e = e.Next() {
					mp := e.Value.(memoizedPack)
					if mp.repo == repo && mp.pack == packOID {
						s.cacheLRU.Remove(e)
						s.cacheBytes -= decodedPackBytes(dp)
						break
					}
				}
			}
		}
	}
}

// IngestPack handles one pushed pack (docs/03 §4 write path, docs/10 §5):
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
	key, kerr := s.storeKey(ctx, repo)
	if kerr != nil {
		return "", kerr
	}
	confirmed, err := s.Backend.PutPack(ctx, key, dp.PackOID, packBytes, idxBytes)
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

	s.rememberPackSize(repo, dp.PackOID, int64(len(packBytes)))
	s.memoizeDecoded(repo, dp.PackOID, dp)
	return dp.PackOID, nil
}

// IngestPackFile ingests a spooled pack file — the large-push streaming
// path (docs/03 §4): ONE streaming scan (the Parser over the seekable file
// with no storage, so object bodies never materialize — resident memory is
// object-count metadata plus the bounded delta LRU) collects order/offsets/
// crcs; the pack lands on storage via streaming copies (one independent
// reader per replica); and nothing else is pre-warmed — the decoded-pack
// cache stays cold and commit/tree indexes back-fill lazily on first touch
// (CommitInfo/TreeEntries), so a multi-hundred-GB import costs the same
// resident window as a small push.
func (s *ObjectService) IngestPackFile(ctx context.Context, repo, path string, size int64) (model.OID, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	obs := &idxObserver{}
	parser, err := packfile.NewParser(packfile.NewScanner(f), obs)
	if err != nil {
		return "", err
	}
	checksum, err := parser.Parse()
	if err != nil {
		return "", fmt.Errorf("scan pack: %w", err)
	}
	if len(obs.order) == 0 {
		// A zero-object pack is legal wire traffic: the client pushes refs
		// whose objects the server already holds and git sends the empty
		// pack anyway. Nothing to place or register.
		return model.OID(checksum.String()), nil
	}
	packOID := model.OID(checksum.String())

	var idxBytes []byte
	if len(obs.offsets) == len(obs.order) {
		if b, err := EncodeIdx(s.Algo, packOID, obs.offsets, obs.crcs); err == nil {
			idxBytes = b
		} else {
			log.Printf("ingest %s: idx encode failed (skip): %v", repo, err)
		}
	}
	key, kerr := s.storeKey(ctx, repo)
	if kerr != nil {
		return "", kerr
	}
	open := func() (io.ReadCloser, error) { return os.Open(path) }
	confirmed, err := s.Backend.PutPackStream(ctx, key, packOID, size, open, idxBytes)
	if err != nil {
		return "", err
	}
	s.rememberPackSize(repo, packOID, size)
	if err := s.Meta.PutPack(ctx, repo, packOID, obs.order, confirmed); err != nil {
		return "", err
	}
	// packOrder feeds ExactPackHit; the decoded-pack cache stays cold by
	// design (see the doc note above).
	s.mu.Lock()
	if s.packOrder[repo] == nil {
		s.packOrder[repo] = map[model.OID][]model.OID{}
	}
	s.packOrder[repo][packOID] = obs.order
	s.mu.Unlock()
	return packOID, nil
}

// ExactPackHit implements the hot-path whole-pack direct serve (docs/03 §5):
// when the requested object set exactly equals a stored pack's object set it
// returns that pack OID; the caller can stream the pack bytes verbatim, skipping online re-encoding (re-hash + re-compress).
// The map's value (each object's type) is incidental — only membership is
// probed; the clone-shaped passes hand their typed enumeration straight through.
func (s *ObjectService) ExactPackHit(repo string, oids map[model.OID]model.ObjectType) (model.OID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for packOID, order := range s.packOrder[repo] {
		if len(order) != len(oids) {
			continue
		}
		match := true
		for _, oid := range order {
			if _, ok := oids[oid]; !ok {
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

// Placements reports each object's pack OID — synthetic objects report the
// zero OID. One batched locate feeds the cache (Prefetch), then everything
// is answered from the warmed cache; unknown placement reports zero (the
// caller still reads such objects through Raw). The streaming serve uses
// this to group its output by pack: contiguity keeps the decoded-pack LRU
// from thrashing when a closure spans many packs.
func (s *ObjectService) Placements(ctx context.Context, repo string, oids []model.OID) map[model.OID]model.OID {
	s.Prefetch(ctx, repo, oids)
	s.synthMu.Lock()
	synth := s.synth[repo]
	s.synthMu.Unlock()
	out := make(map[model.OID]model.OID, len(oids))
	for _, oid := range oids {
		if synth != nil {
			if _, ok := synth[oid]; ok {
				out[oid] = ""
				continue
			}
		}
		if loc, ok := s.locHit(repo, oid); ok {
			out[oid] = loc.PackOID
			continue
		}
		out[oid] = ""
	}
	return out
}

// CachedPackSize reports a pack's byte size, probing the backend once and
// caching forever (content addressing makes sizes immutable); -1 = unknown.
// The streaming serve uses the pack-group total as its small-repo cutoff.
func (s *ObjectService) CachedPackSize(ctx context.Context, repo string, packOID model.OID) int64 {
	return s.packSizeCached(ctx, repo, packOID)
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
	key, err := s.storeKey(ctx, repo)
	if err != nil {
		return nil, err
	}
	return s.Backend.GetPack(ctx, key, packOID, s.packHolders(ctx, repo, packOID, ""))
}

// OpenPackStream opens a stored pack for verbatim streaming (exact-pack
// serve, docs/03 §5): node disk → network → response, one small window —
// the bytes never transit gateway memory whole.
func (s *ObjectService) OpenPackStream(ctx context.Context, repo string, packOID model.OID) (io.ReadCloser, int64, error) {
	key, err := s.storeKey(ctx, repo)
	if err != nil {
		return nil, 0, err
	}
	return s.Backend.OpenPack(ctx, key, packOID, s.packHolders(ctx, repo, packOID, ""))
}

// RepoBytes reports the repo's total pack bytes — the scale statistic
// behind the full-clone budget (docs/11 §6) and the hook payload's
// repo_bytes field (docs/04 §7). Sizes are content-immutable and
// probe-cached (ingest primes them), so after the first ask this is map
// lookups over the pack registry.
func (s *ObjectService) RepoBytes(ctx context.Context, repo string) (int64, error) {
	packs, err := s.Meta.ListPacks(ctx, repo)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, p := range packs {
		if sz := s.packSizeCached(ctx, repo, p.PackOID); sz > 0 {
			total += sz
		}
	}
	return total, nil
}

// rememberPackSize primes the size cache at the source of truth (ingest
// knows the byte count; repack knows its output) — RepoBytes then never
// needs a node HEAD round trip.
func (s *ObjectService) rememberPackSize(repo string, packOID model.OID, size int64) {
	if size <= 0 {
		return
	}
	ck := repo + "/" + string(packOID)
	s.mu.Lock()
	if s.sizeKnown == nil {
		s.sizeKnown = map[string]int64{}
	}
	s.sizeKnown[ck] = size
	s.mu.Unlock()
}

func (s *ObjectService) forgetPackSize(repo string, packOID model.OID) {
	s.mu.Lock()
	delete(s.sizeKnown, repo+"/"+string(packOID))
	s.mu.Unlock()
}

// RenameRepo renames a repository end to end (docs/03 §6).
//
//	hashed addressing: the registry flip alone — the storage key derives
//	 from the immutable storage id and never moves.
//	named addressing:  every storage location moves FIRST (idempotent —
//	 re-running after a partial failure only hits laggards), the registry
//	 flips LAST, so the data is never behind the name it is looked up by.
//
// Named-mode boundary (honest): the move fans out to the backend's current
// node view; a node absent from that view keeps the old-named directory and
// must be renamed after it returns (re-run the same call — nodes already
// moved answer OK).
func (s *ObjectService) RenameRepo(ctx context.Context, from, to string) error {
	if s.Addressing != nil && s.Addressing.Mode == "hashed" {
		return s.Meta.RenameRepo(ctx, from, to)
	}
	if err := s.Backend.RenameRepo(ctx, from, to); err != nil {
		return err
	}
	return s.Meta.RenameRepo(ctx, from, to)
}

// DeleteRepo reclaims the repo's storage bytes and drops every registry row
// (repository deletion, gitaly RemoveRepository). Hashed addressing keeps the
// immutable storage key — only the registry rows go; a later repo of the same
// name starts clean (its old key is unreachable garbage until fleet GC).
func (s *ObjectService) DeleteRepo(ctx context.Context, repo string) error {
	if s.Addressing == nil || s.Addressing.Mode != "hashed" {
		if err := s.Backend.DeleteRepo(ctx, repo); err != nil {
			return err
		}
	}
	return s.Meta.DeleteRepo(ctx, repo)
}

// storeKey resolves the repo's storage key (addressing modes, docs/03 §6);
// the identity mapping unless hashed addressing is wired.
func (s *ObjectService) storeKey(ctx context.Context, repo string) (string, error) {
	if s.Addressing == nil {
		return repo, nil
	}
	return s.Addressing.Key(ctx, repo)
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

// TreeStoreFor returns a writable repo view (for assembly, docs/02 §4):
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

// Tree goes through the tree-entry index (docs/03 §1.3): parse once, memoize
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
// The <repo> path segment is the STORAGE KEY (docs/03 §6): the repo name
// itself under named addressing, an opaque @hashed/… path under hashed.
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

// PutPackStream lands the pack from a reader with one streaming copy (the
// large-push ingest path — the whole pack never materializes in memory).
// size cross-checks the copy when positive; idx lands after the pack so a
// crash mid-write leaves at most a pack without its idx.
func (l *LocalShardStore) PutPackStream(repo string, packOID model.OID, size int64, open func() (io.ReadCloser, error), idxBytes []byte) error {
	p := l.packPath(repo, packOID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	src, err := open()
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(p)
	if err != nil {
		return err
	}
	n, cerr := io.Copy(dst, src)
	werr := dst.Close()
	if cerr != nil {
		return cerr
	}
	if werr != nil {
		return werr
	}
	if size > 0 && n != size {
		return fmt.Errorf("put %s: short copy %d/%d", packOID, n, size)
	}
	if len(idxBytes) > 0 {
		return os.WriteFile(l.idxPath(repo, packOID), idxBytes, 0o644)
	}
	return nil
}

// ReceivePack lands a pack from a request-body stream (storenode PUT): one
// streaming copy into a temp file with the trailer verified by a trailing
// hash window (SpoolVerifiedCopy), an OID identity check, then an atomic
// rename — the whole pack never transits memory. size cross-checks the copy
// when positive (a set Content-Length).
func (l *LocalShardStore) ReceivePack(repo string, packOID model.OID, size int64, r io.Reader, idxBytes []byte) error {
	p := l.packPath(repo, packOID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".recv-*")
	if err != nil {
		return err
	}
	n, sum, ok, cerr := SpoolVerifiedCopy(tmp, r, algoOfPackOID(packOID))
	tcErr := tmp.Close()
	if cerr == nil && !ok {
		cerr = fmt.Errorf("pack checksum mismatch")
	}
	if cerr == nil && sum != string(packOID) {
		cerr = fmt.Errorf("pack checksum mismatch (got %s want %s)", sum, packOID)
	}
	if cerr == nil && size > 0 && n != size {
		cerr = fmt.Errorf("put %s: short copy %d/%d", packOID, n, size)
	}
	if cerr == nil {
		cerr = tcErr
	}
	if cerr != nil {
		_ = os.Remove(tmp.Name())
		return cerr
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if len(idxBytes) > 0 {
		return os.WriteFile(l.idxPath(repo, packOID), idxBytes, 0o644)
	}
	return nil
}

// OpenPack opens the pack file for streaming (storenode GET, exact-pack
// serve): disk → response without a memory transit.
func (l *LocalShardStore) OpenPack(repo string, packOID model.OID) (*os.File, int64, error) {
	f, err := os.Open(l.packPath(repo, packOID))
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// PackFileSize stats a pack without opening it (size probes for the
// whole-pack memoize guard).
func (l *LocalShardStore) PackFileSize(repo string, packOID model.OID) (int64, error) {
	st, err := os.Stat(l.packPath(repo, packOID))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// DeletePack removes the pack and idx files from disk (repack old-shard
// cleanup; missing files are tolerated — CAS re-ingest may have skipped writes).
func (l *LocalShardStore) DeletePack(repo string, packOID model.OID) {
	_ = os.Remove(l.packPath(repo, packOID))
	_ = os.Remove(l.idxPath(repo, packOID))
}

// RenameRepo moves the whole repo directory (named addressing, docs/03 §6):
// a missing source is a successful no-op (idempotent re-runs), an existing
// target is refused — a merge would need byte-level reconciliation.
func (l *LocalShardStore) RenameRepo(from, to string) error {
	src := filepath.Join(l.Root, from)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	dst := filepath.Join(l.Root, to)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("rename %s: target %s already exists", from, to)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// DeleteRepo removes the repo's whole storage directory. A missing directory
// is a successful no-op (deletion is idempotent by contract).
func (l *LocalShardStore) DeleteRepo(repo string) error {
	if strings.ContainsAny(repo, `/\`) {
		return fmt.Errorf("delete %s: repo must be a single path segment", repo)
	}
	return os.RemoveAll(filepath.Join(l.Root, repo))
}

// GetDecoded reads back and decodes a pack (miss path; object bodies then stay in cache).
// Single-object reads go through GetObject instead (docs/11 P1-1): idx offset
// + delta-chain inflate, no whole-pack decode.
func (l *LocalShardStore) GetDecoded(repo string, packOID model.OID) (*DecodedPack, error) {
	raw, err := l.GetPackBytes(repo, packOID)
	if err != nil {
		return nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, packOID, err)
	}
	return DecodePack(raw)
}

// GetObject extracts one object from a stored pack via its idx v2 (docs/11
// P1-1): header parse at the located offset + delta-chain inflate, reading
// the pack FILE in windows — a multi-hundred-MB pack costs O(object)
// memory, never O(pack). A missing or unusable idx surfaces as an error —
// callers fall back to the whole-pack path.
func (l *LocalShardStore) GetObject(repo string, packOID, oid model.OID, algo model.HashAlgo) (model.ObjectType, []byte, error) {
	idxRaw, err := os.ReadFile(l.idxPath(repo, packOID))
	if err != nil {
		return "", nil, fmt.Errorf("%w: idx of %s: %v", model.ErrObjectNotFound, packOID, err)
	}
	pi, err := DecodeIdx(idxRaw, algo)
	if err != nil {
		return "", nil, err
	}
	f, err := os.Open(l.packPath(repo, packOID))
	if err != nil {
		return "", nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, packOID, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	return ExtractObjectAt(f, st.Size(), pi, algo, oid)
}
