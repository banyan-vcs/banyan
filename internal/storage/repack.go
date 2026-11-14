package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
)

// Repack merges pack fragments under git's geometric packing strategy
// (docs/storage/storage.md §5, docs/storage/indexing-packing.md §3.2 — `git repack --geometric=2`): packs sorted by
// object count must form a geometric sequence (each ≥ factor × its
// predecessor); the smallest run that violates it merges into one pack.
// Large packs are NEVER touched — merge cost stays proportional to the
// fragment run, not the repository (the old full-merge strategy was
// infeasible past a few GB), and hot packs keep serving verbatim.
//
// Fragment source: every server-side synthesis (projection commits, woven
// rewrites) persists as a single-object pack (PutSynthetic write-through), so
// a busy repo accumulates hundreds of fragments — storage bloat, index
// dilution, and ExactPackHit whole-pack direct serve degrades to online
// re-encoding.
//
// Protocol: register the merged pack FIRST (its registration re-points every
// object location), then drop the old shards. The crash window between the
// two leaves duplicate locations — still correct (either pack resolves the
// object), the next repack cleans up.
type Repacker struct {
	Objects           *ObjectService
	FragmentThreshold int   // repos below this many packs are left alone
	GeometricFactor   int   // adjacent pack object-count ratio (git default 2)
	MergeMaxBytes     int64 // per merge-group input byte cap (0 = default)
}

// repackMergeMaxBytes caps one merge group's total INPUT bytes — the group
// union lives in memory while merging, so equal-sized large packs (which
// the count-based geometric rule happily groups) must not gang up past any
// budget. Oversized groups split at the cap.
const repackMergeMaxBytes = 256 << 20

func NewRepacker(objects *ObjectService, fragmentThreshold int) *Repacker {
	if fragmentThreshold <= 0 {
		fragmentThreshold = 64
	}
	return &Repacker{Objects: objects, FragmentThreshold: fragmentThreshold, GeometricFactor: 2}
}

func (r *Repacker) mergeMax() int64 {
	if r.MergeMaxBytes > 0 {
		return r.MergeMaxBytes
	}
	return repackMergeMaxBytes
}

// geometricGroups splits packs (object counts, ascending) into merge groups
// following git's geometric packing: the sequence holds when each pack is
// at least factor × its PREDECESSOR (adjacent ratio, not cumulative — a
// cumulative test snowballs and swallows the big packs); a run of adjacent
// violations merges into one pack. Returns half-open ranges [lo,hi).
// [1,1,100,100] with factor 2 yields {0,2} and {2,4}; [1,3,10,40] yields
// four singletons (already geometric).
func geometricGroups(counts []int, factor int) [][2]int {
	var groups [][2]int
	i := 0
	for i < len(counts) {
		j := i
		for j+1 < len(counts) && counts[j+1] < factor*counts[j] {
			j++
		}
		groups = append(groups, [2]int{i, j + 1})
		i = j + 1
	}
	return groups
}

// RepackResult reports one round's effect (observability + tests).
type RepackResult struct {
	PacksBefore, PacksAfter int
	Objects                 int
	BytesBefore, BytesAfter int64
	Delta                   bool // the merged pack is delta-encoded
}

// RunOnce executes one repack round for one repo. Below the fragment
// threshold it is a no-op ("" plan, nil error).
func (r *Repacker) RunOnce(ctx context.Context, repo string) (string, error) {
	res, err := r.Objects.Repack(ctx, repo, r.FragmentThreshold, r.mergeMax())
	if err != nil {
		return "", err
	}
	if res.PacksAfter == res.PacksBefore {
		return fmt.Sprintf("no-op: %d packs (threshold %d)", res.PacksBefore, r.FragmentThreshold), nil
	}
	return fmt.Sprintf("merged %d → %d packs, %d objects, %d → %d bytes (delta=%v)",
		res.PacksBefore, res.PacksAfter, res.Objects, res.BytesBefore, res.BytesAfter, res.Delta), nil
}

// Repack is the implementation (on ObjectService: it owns the pack cache and
// in-memory indexes that must follow the swap). Geometric strategy: only the
// merge groups that break the factor-2 sequence are consolidated, one new
// pack per group; singleton groups (large packs included) are untouched.
// mergeMax caps each group's input bytes (0 = default).
func (s *ObjectService) Repack(ctx context.Context, repo string, threshold int, mergeMax int64) (RepackResult, error) {
	packs, err := s.Meta.ListPacks(ctx, repo)
	if err != nil {
		return RepackResult{}, err
	}
	if mergeMax <= 0 {
		mergeMax = repackMergeMaxBytes
	}
	res := RepackResult{PacksBefore: len(packs)}
	if len(packs) < 2 || (threshold > 0 && len(packs) < threshold) {
		res.PacksAfter = len(packs)
		return res, nil
	}

	// 0. Geometric split: ascending object counts → merge groups, then the
	// byte cap splits any group whose input bytes would blow the merge
	// budget (the count rule alone lets equal-sized big packs gang up).
	type sizedPack struct {
		info   meta.PackInfo
		counts int
		size   int64
	}
	sized := make([]sizedPack, 0, len(packs))
	for _, p := range packs {
		objs, oerr := s.Meta.PackObjects(ctx, repo, p.PackOID)
		if oerr != nil {
			log.Printf("repack %s: skip pack %s: %v", repo, p.PackOID, oerr)
			continue
		}
		sz := s.packSizeCached(ctx, repo, p.PackOID)
		if sz < 0 {
			sz = 0 // unknown (old node): the count rule alone governs it
		}
		sized = append(sized, sizedPack{info: p, counts: len(objs), size: sz})
	}
	sort.Slice(sized, func(i, j int) bool {
		if sized[i].counts != sized[j].counts {
			return sized[i].counts < sized[j].counts
		}
		return sized[i].info.PackOID < sized[j].info.PackOID
	})
	counts := make([]int, len(sized))
	for i := range sized {
		counts[i] = sized[i].counts
	}
	max := mergeMax
	var mergeSets [][]meta.PackInfo
	for _, g := range geometricGroups(counts, 2) {
		if g[1]-g[0] < 2 {
			continue
		}
		var cur []sizedPack
		var acc int64
		emit := func() {
			if len(cur) < 2 {
				return
			}
			set := make([]meta.PackInfo, 0, len(cur))
			for _, sp := range cur {
				set = append(set, sp.info)
			}
			mergeSets = append(mergeSets, set)
		}
		for i := g[0]; i < g[1]; i++ {
			if len(cur) > 0 && acc+sized[i].size > max {
				emit()
				cur, acc = nil, 0
			}
			cur = append(cur, sized[i])
			acc += sized[i].size
		}
		emit()
	}
	if len(mergeSets) == 0 {
		res.PacksAfter = len(packs)
		return res, nil
	}

	for _, set := range mergeSets {
		setRes, merr := s.mergePacks(ctx, repo, set)
		if merr != nil {
			return res, merr
		}
		res.Objects += setRes.Objects
		res.BytesBefore += setRes.BytesBefore
		res.BytesAfter += setRes.BytesAfter
		res.Delta = res.Delta || setRes.Delta
	}
	res.PacksAfter = res.PacksBefore
	for _, set := range mergeSets {
		res.PacksAfter -= len(set) - 1
	}
	return res, nil
}

// mergePacks consolidates one geometric group: read the fragments, union by
// object identity (content addressing makes duplicates interchangeable),
// encode one delta pack, register it (registration re-points every object
// location atomically per object — reads switch as of this call), then drop
// the old shards (metadata first, then files, then caches).
func (s *ObjectService) mergePacks(ctx context.Context, repo string, set []meta.PackInfo) (RepackResult, error) {
	// Fast path — append consolidation: see tryAppendMerge.
	if res, ok, err := s.tryAppendMerge(ctx, repo, set); err != nil {
		log.Printf("repack %s: append merge fell back to re-encode: %v", repo, err)
	} else if ok {
		return res, nil
	}
	return s.mergePacksReencode(ctx, repo, set)
}

func (s *ObjectService) mergePacksReencode(ctx context.Context, repo string, set []meta.PackInfo) (RepackResult, error) {
	var res RepackResult
	var blobs, trees, commits, tags = map[model.OID][]byte{}, map[model.OID][]byte{}, map[model.OID][]byte{}, map[model.OID][]byte{}
	// skipped collects fragments whose bytes could not be read (all holders
	// unreachable, racing deletion). Their objects are NOT in the merged
	// pack, so dropping their registration would delete the only rows that
	// still locate those objects — data loss. They stay registered for the
	// next round instead (reads keep working once the holder returns; a
	// genuinely vanished pack is an availability problem, not one a repack
	// may convert into permanent loss).
	skipped := map[model.OID]bool{}
	for _, p := range set {
		raw, rerr := s.PackBytes(ctx, repo, p.PackOID)
		if rerr != nil {
			log.Printf("repack %s: skip pack %s: %v", repo, p.PackOID, rerr)
			skipped[p.PackOID] = true
			continue
		}
		res.BytesBefore += int64(len(raw))
		dp, derr := DecodePack(raw)
		if derr != nil {
			log.Printf("repack %s: skip pack %s: %v", repo, p.PackOID, derr)
			skipped[p.PackOID] = true
			continue
		}
		for oid, b := range dp.Blobs {
			blobs[oid] = b
		}
		for oid, t := range dp.Trees {
			trees[oid] = t
		}
		for oid, c := range dp.Commits {
			commits[oid] = c
		}
		for oid, tg := range dp.Tags {
			tags[oid] = tg
		}
	}
	res.Objects = len(blobs) + len(trees) + len(commits) + len(tags)
	if res.Objects == 0 {
		return res, nil
	}

	// Encode straight to a spool file and register via the streaming ingest:
	// no whole-output buffer, no second full decode (the old IngestPack path
	// re-decoded what had just been encoded — one more whole-group copy).
	tmp, err := os.CreateTemp("", "banyan-repack-*.pack")
	if err != nil {
		return RepackResult{}, err
	}
	tmpName := tmp.Name()
	if err := EncodePackDelta(tmp, blobs, trees, commits, tags); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return RepackResult{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return RepackResult{}, err
	}
	st, serr := os.Stat(tmpName)
	if serr != nil {
		os.Remove(tmpName)
		return RepackResult{}, serr
	}
	newOID, err := s.IngestPackFile(ctx, repo, tmpName, st.Size())
	os.Remove(tmpName)
	if err != nil {
		return RepackResult{}, err
	}
	s.rememberPackSize(repo, newOID, st.Size())
	res.Delta = true
	res.BytesAfter = st.Size()

	key, kerr := s.storeKey(ctx, repo)
	if kerr != nil {
		return RepackResult{}, kerr
	}
	for _, p := range set {
		if p.PackOID == newOID {
			continue // CAS idempotency: the merged pack may equal a fragment
		}
		if skipped[p.PackOID] {
			continue // unreadable fragment: its objects are not covered — never drop
		}
		if derr := s.Meta.DropPack(ctx, repo, p.PackOID); derr != nil {
			log.Printf("repack %s: drop meta %s: %v", repo, p.PackOID, derr)
			continue
		}
		s.Backend.DeletePack(ctx, key, p.PackOID, p.Nodes) // key, not name (hashed addressing)
		s.forgetDecoded(repo, p.PackOID)
		s.forgetPackSize(repo, p.PackOID)
		s.readMu.Lock()
		delete(s.readAmp, repo+"/"+string(p.PackOID))
		s.readMu.Unlock()
		s.mu.Lock()
		delete(s.packOrder[repo], p.PackOID)
		s.mu.Unlock()
	}
	// Every merged-away pack invalidates cached placements: locations only
	// ever change at repack (docs/operations/performance.md P0-1). Other gateways' caches self-heal
	// through Raw's stale-placement retry.
	s.locInvalidate(repo)
	return res, nil
}

// errConsolidateBusy reports an in-flight consolidation for the repo — the
// trigger is dropped (the running round already covers the current state,
// and the next materialization re-asks).
var errConsolidateBusy = errors.New("consolidate: another round is in flight")

// Consolidate merges EVERY pack of a repo into one — an explicit,
// strategy-free collapse. The geometric rule deliberately leaves
// [small-synth-pack, big-push-pack] pairs alone (their adjacent ratio
// holds), but a woven repo's clone closure spans exactly such a pair and
// whole-pack direct serve dies with it: every clone re-encodes online
// (docs/operations/performance.md §3b 合笼). Groups split at the same
// per-group byte cap as Repack, so giant repos consolidate in bounded
// chunks instead of one unbounded union.
func (s *ObjectService) Consolidate(ctx context.Context, repo string) (RepackResult, error) {
	s.consolMu.Lock()
	if s.consolIn[repo] {
		s.consolMu.Unlock()
		return RepackResult{}, errConsolidateBusy
	}
	s.consolIn[repo] = true
	s.consolMu.Unlock()
	defer func() {
		s.consolMu.Lock()
		delete(s.consolIn, repo)
		s.consolMu.Unlock()
	}()

	packs, err := s.Meta.ListPacks(ctx, repo)
	if err != nil {
		return RepackResult{}, err
	}
	res := RepackResult{PacksBefore: len(packs)}
	if len(packs) < 2 {
		res.PacksAfter = len(packs)
		return res, nil
	}
	type sized struct {
		info meta.PackInfo
		size int64
	}
	sizedPacks := make([]sized, 0, len(packs))
	for _, p := range packs {
		sz := s.packSizeCached(ctx, repo, p.PackOID)
		if sz < 0 {
			sz = 0 // unknown (old node): size-blind placement is safe, the cap just loses a guard
		}
		sizedPacks = append(sizedPacks, sized{info: p, size: sz})
	}
	sort.Slice(sizedPacks, func(i, j int) bool { return sizedPacks[i].size < sizedPacks[j].size })
	var mergeSets [][]meta.PackInfo
	var cur []meta.PackInfo
	var acc int64
	emit := func() {
		if len(cur) >= 2 {
			mergeSets = append(mergeSets, cur)
		}
		cur = nil
	}
	for _, sp := range sizedPacks {
		if len(cur) > 0 && acc+sp.size > repackMergeMaxBytes {
			emit()
			acc = 0
		}
		cur = append(cur, sp.info)
		acc += sp.size
	}
	emit()
	if len(mergeSets) == 0 {
		res.PacksAfter = len(packs)
		return res, nil
	}
	for _, set := range mergeSets {
		setRes, merr := s.mergePacks(ctx, repo, set)
		if merr != nil {
			return res, merr
		}
		res.Objects += setRes.Objects
		res.BytesBefore += setRes.BytesBefore
		res.BytesAfter += setRes.BytesAfter
		res.Delta = res.Delta || setRes.Delta
	}
	res.PacksAfter = res.PacksBefore
	for _, set := range mergeSets {
		res.PacksAfter -= len(set) - 1
	}
	return res, nil
}

// tryAppendMerge copies the dominant pack's entry region under a fresh
// header/count and appends the other packs' (deduplicated) objects as
// delta-free entries (docs/operations/performance.md §3b 合笼 fast path).
// Pack entries are position-independent except OFS_DELTA, whose offsets
// are RELATIVE — a verbatim prefix copy keeps every delta valid, so the
// dominant pack's (typically C-git-quality) deltas survive instead of
// being re-delta'd by the Go encoder (which roughly doubled a real push
// pack's bytes). ok=false with nil error means the shape did not qualify;
// the caller re-encodes as before.
func (s *ObjectService) tryAppendMerge(ctx context.Context, repo string, set []meta.PackInfo) (RepackResult, bool, error) {
	var res RepackResult
	if len(set) < 2 {
		return res, false, nil
	}
	if s.Algo != model.SHA1 {
		// newPackWriter hashes the trailer with SHA-1; the re-encode path
		// carries the repo algo through its encoder.
		return res, false, nil
	}
	var base meta.PackInfo
	var baseBytes, total int64
	for _, p := range set {
		sz := s.packSizeCached(ctx, repo, p.PackOID)
		if sz < 0 {
			sz = 0
		}
		total += sz
		if sz > baseBytes {
			baseBytes, base = sz, p
		}
	}
	if baseBytes <= 0 || baseBytes*2 <= total {
		return res, false, nil // no dominant pack: the general path applies
	}
	baseRaw, err := s.PackBytes(ctx, repo, base.PackOID)
	if err != nil {
		return res, false, err
	}
	if len(baseRaw) < 32 || string(baseRaw[:4]) != "PACK" ||
		binary.BigEndian.Uint32(baseRaw[4:8]) != 2 {
		return res, false, fmt.Errorf("append merge: base pack is not a v2 pack")
	}
	baseObjs := map[model.OID]bool{}
	objs, oerr := s.Meta.PackObjects(ctx, repo, base.PackOID)
	if oerr != nil {
		return res, false, oerr
	}
	for _, o := range objs {
		baseObjs[o] = true
	}

	type extraObj struct {
		t    model.ObjectType
		data []byte
	}
	extras := map[model.OID]extraObj{}
	add := func(m map[model.OID][]byte, t model.ObjectType) {
		for oid, data := range m {
			if !baseObjs[oid] {
				extras[oid] = extraObj{t, data}
			}
		}
	}
	skipped := map[model.OID]bool{}
	for _, p := range set {
		if p.PackOID == base.PackOID {
			res.BytesBefore += int64(len(baseRaw))
			continue
		}
		raw, rerr := s.PackBytes(ctx, repo, p.PackOID)
		if rerr != nil {
			skipped[p.PackOID] = true // unreadable fragment: kept, never dropped
			continue
		}
		res.BytesBefore += int64(len(raw))
		dp, derr := DecodePack(raw)
		if derr != nil {
			skipped[p.PackOID] = true
			continue
		}
		add(dp.Blobs, model.BlobType)
		add(dp.Trees, model.TreeType)
		add(dp.Commits, model.CommitType)
		add(dp.Tags, model.TagType)
	}
	count := int64(len(baseObjs) + len(extras))
	res.Objects = int(count)
	if res.Objects == 0 {
		return res, false, nil
	}

	tmp, err := os.CreateTemp("", "banyan-repack-*.pack")
	if err != nil {
		return res, false, err
	}
	tmpName := tmp.Name()
	w := newPackWriter(tmp)
	w.write([]byte("PACK"))
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(count))
	w.write(hdr[:])
	w.write(baseRaw[12 : len(baseRaw)-20]) // entry region verbatim — OFS_DELTA offsets are relative
	for _, o := range extras {
		var typ byte
		switch o.t {
		case model.CommitType:
			typ = writeTypeCommit
		case model.TreeType:
			typ = writeTypeTree
		case model.BlobType:
			typ = writeTypeBlob
		case model.TagType:
			typ = writeTypeTag
		default:
			tmp.Close()
			os.Remove(tmpName)
			return res, false, fmt.Errorf("append merge: object type %q", o.t)
		}
		w.object(typ, len(o.data), nil, o.data)
	}
	if err := w.finish(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return res, false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return res, false, err
	}
	res.Delta = true // the base pack's deltas ride along verbatim
	newOID, size, rerr := s.registerAndSwap(ctx, repo, tmpName, set, skipped)
	if rerr != nil {
		return res, false, rerr
	}
	res.BytesAfter = size
	_ = newOID
	return res, true, nil
}

// registerAndSwap registers the spooled merged pack, then drops the merged
// fragments: register FIRST (the crash window leaves duplicate locations —
// still correct, the next repack cleans up); unreadable fragments keep
// their registration (their objects are not covered by the merge).
func (s *ObjectService) registerAndSwap(ctx context.Context, repo, tmpName string, set []meta.PackInfo, skipped map[model.OID]bool) (model.OID, int64, error) {
	st, serr := os.Stat(tmpName)
	if serr != nil {
		os.Remove(tmpName)
		return "", 0, serr
	}
	newOID, err := s.IngestPackFile(ctx, repo, tmpName, st.Size())
	os.Remove(tmpName)
	if err != nil {
		return "", 0, err
	}
	s.rememberPackSize(repo, newOID, st.Size())
	key, kerr := s.storeKey(ctx, repo)
	if kerr != nil {
		return newOID, st.Size(), kerr
	}
	for _, p := range set {
		if p.PackOID == newOID || skipped[p.PackOID] {
			continue
		}
		if derr := s.Meta.DropPack(ctx, repo, p.PackOID); derr != nil {
			log.Printf("repack %s: drop meta %s: %v", repo, p.PackOID, derr)
			continue
		}
		s.Backend.DeletePack(ctx, key, p.PackOID, p.Nodes) // key, not name (hashed addressing)
		s.forgetDecoded(repo, p.PackOID)
		s.forgetPackSize(repo, p.PackOID)
		s.readMu.Lock()
		delete(s.readAmp, repo+"/"+string(p.PackOID))
		s.readMu.Unlock()
		s.mu.Lock()
		delete(s.packOrder[repo], p.PackOID)
		s.mu.Unlock()
	}
	// Every merged-away pack invalidates cached placements: locations only
	// ever change at repack (docs/operations/performance.md P0-1). Other gateways' caches self-heal
	// through Raw's stale-placement retry.
	s.locInvalidate(repo)
	return newOID, st.Size(), nil
}
