package storage

import (
	"bytes"
	"context"
	"fmt"
	"log"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Repack merges a repo's pack fragments into one delta-encoded pack
// (docs/03 §5, docs/09 §3.2 — git repack -a + pack-objects window deltas).
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
	FragmentThreshold int // repos below this many packs are left alone
}

func NewRepacker(objects *ObjectService, fragmentThreshold int) *Repacker {
	if fragmentThreshold <= 0 {
		fragmentThreshold = 64
	}
	return &Repacker{Objects: objects, FragmentThreshold: fragmentThreshold}
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
	res, err := r.Objects.Repack(ctx, repo, r.FragmentThreshold)
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
// in-memory indexes that must follow the swap).
func (s *ObjectService) Repack(ctx context.Context, repo string, threshold int) (RepackResult, error) {
	packs, err := s.Meta.ListPacks(ctx, repo)
	if err != nil {
		return RepackResult{}, err
	}
	res := RepackResult{PacksBefore: len(packs)}
	if len(packs) < 2 || (threshold > 0 && len(packs) < threshold) {
		res.PacksAfter = len(packs)
		return res, nil
	}

	// 1. Merge: read every fragment, union by object identity (content
	// addressing makes duplicates interchangeable).
	var blobs, trees, commits, tags = map[model.OID][]byte{}, map[model.OID][]byte{}, map[model.OID][]byte{}, map[model.OID][]byte{}
	var bytesBefore int64
	for _, p := range packs {
		raw, rerr := s.PackBytes(ctx, repo, p.PackOID)
		if rerr != nil {
			// A vanished fragment (racing deletion) is skipped; its objects
			// re-register to surviving packs on their next ingest.
			log.Printf("repack %s: skip pack %s: %v", repo, p.PackOID, rerr)
			continue
		}
		bytesBefore += int64(len(raw))
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
		res.PacksAfter = len(packs)
		return res, nil
	}

	// 2. Encode one delta pack (window heuristics are the encoder's) and
	// register it — registration re-points every object location atomically
	// per object, so reads switch to the merged pack as of this call.
	var buf bytes.Buffer
	if err := EncodePackDelta(&buf, blobs, trees, commits, tags); err != nil {
		return RepackResult{}, err
	}
	newOID, err := s.IngestPack(ctx, repo, buf.Bytes())
	if err != nil {
		return RepackResult{}, err
	}
	res.Delta = true
	res.BytesAfter = int64(buf.Len())

	// 3. Drop the old shards (metadata first, then files, then caches).
	for _, p := range packs {
		if p.PackOID == newOID {
			continue // CAS idempotency: the merged pack may equal a fragment
		}
		if derr := s.Meta.DropPack(ctx, repo, p.PackOID); derr != nil {
			log.Printf("repack %s: drop meta %s: %v", repo, p.PackOID, derr)
			continue
		}
		s.Backend.DeletePack(ctx, repo, p.PackOID, p.Nodes)
		s.mu.Lock()
		delete(s.cache[repo], p.PackOID)
		delete(s.packOrder[repo], p.PackOID)
		s.mu.Unlock()
	}
	// Every merged-away pack invalidates cached placements: locations only
	// ever change at repack (docs/11 P0-1). Other gateways' caches self-heal
	// through Raw's stale-placement retry.
	s.locInvalidate(repo)
	packsAfter, err := s.Meta.ListPacks(ctx, repo)
	if err == nil {
		res.PacksAfter = len(packsAfter)
	} else {
		res.PacksAfter = 1
	}
	return res, nil
}
