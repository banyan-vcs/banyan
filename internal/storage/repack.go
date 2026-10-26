package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Repack merges pack fragments under git's geometric packing strategy
// (docs/03 §5, docs/09 §3.2 — `git repack --geometric=2`): packs sorted by
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
		info   PackInfo
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
	var mergeSets [][]PackInfo
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
			set := make([]PackInfo, 0, len(cur))
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
func (s *ObjectService) mergePacks(ctx context.Context, repo string, set []PackInfo) (RepackResult, error) {
	var res RepackResult
	var blobs, trees, commits, tags = map[model.OID][]byte{}, map[model.OID][]byte{}, map[model.OID][]byte{}, map[model.OID][]byte{}
	for _, p := range set {
		raw, rerr := s.PackBytes(ctx, repo, p.PackOID)
		if rerr != nil {
			// A vanished fragment (racing deletion) is skipped; its objects
			// re-register to surviving packs on their next ingest.
			log.Printf("repack %s: skip pack %s: %v", repo, p.PackOID, rerr)
			continue
		}
		res.BytesBefore += int64(len(raw))
		dp, derr := DecodePack(raw)
		if derr != nil {
			log.Printf("repack %s: skip pack %s: %v", repo, p.PackOID, derr)
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
	// ever change at repack (docs/11 P0-1). Other gateways' caches self-heal
	// through Raw's stale-placement retry.
	s.locInvalidate(repo)
	return res, nil
}
