package refs

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Reconciler converges actual pack holders toward each repo's replica target
// (docs/10 §8.3). Ordering discipline is the whole correctness argument:
// COPY first, REGISTER the new holder set, DELETE last — any interruption
// leaves at worst an over-replicated pack, never a registered holder without
// data, never a delete below the target.
type Reconciler struct {
	Meta        meta.MetaStore
	Health      *HealthTable
	DefaultR    int
	Concurrency int
	// Addressing resolves repo names to storage keys for node calls
	// (docs/03 §6): hashed mode copies/trims under the opaque key while the
	// registry stays name-keyed. Nil = named (identity).
	Addressing *meta.Addressing

	clientsMu sync.Mutex
	clients   map[string]*storage.NodeClient
}

// ReconResult reports one convergence pass (observability + tests).
type ReconResult struct {
	UnderReplicated int `json:"under_replicated"` // packs needing a copy
	Copied          int `json:"copied"`           // packs brought back to target
	Trimmed         int `json:"trimmed"`          // over-replicated packs reduced
	Failed          int `json:"failed"`           // packs that could not converge this pass
}

func NewReconciler(meta meta.MetaStore, health *HealthTable, defaultR, concurrency int) *Reconciler {
	if defaultR <= 0 {
		defaultR = 3
	}
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Reconciler{Meta: meta, Health: health, DefaultR: defaultR, Concurrency: concurrency, clients: map[string]*storage.NodeClient{}}
}

func (r *Reconciler) client(addr string) *storage.NodeClient {
	r.clientsMu.Lock()
	defer r.clientsMu.Unlock()
	if c, ok := r.clients[addr]; ok {
		return c
	}
	c := storage.NewNodeClient("http://" + addr)
	r.clients[addr] = c
	return c
}

// RunOnce scans every repo's packs against the health view and converges.
// Bounded by Concurrency parallel pack copies.
func (r *Reconciler) RunOnce(ctx context.Context) ReconResult {
	res := ReconResult{}
	dead := r.Health.deadSet()
	healthy := r.Health.healthy()

	repos, err := r.Meta.ListRepos(ctx)
	if err != nil {
		log.Printf("reconciler: list repos: %v", err)
		return res
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, r.Concurrency)
	var mu sync.Mutex // guards res
	for _, repoInfo := range repos {
		repo := repoInfo.ID
		target := r.DefaultR
		if p, err := r.Meta.GetPlacement(ctx, repo); err == nil && p > 0 {
			target = p
		}
		packs, err := r.Meta.ListPacks(ctx, repo)
		if err != nil {
			log.Printf("reconciler %s: list packs: %v", repo, err)
			continue
		}
		for _, p := range packs {
			if r.convergePack(ctx, repo, p, target, dead, healthy, &res, &mu, &wg, sem) {
				// async path counted inside convergePack
			}
		}
	}
	wg.Wait()
	return res
}

// convergePack applies one pack's under/over-replication fix. Returns whether
// work was dispatched (all bookkeeping is under mu).
func (r *Reconciler) convergePack(ctx context.Context, repo string, p meta.PackInfo, target int,
	dead map[string]bool, healthy []meta.NodeInfo, res *ReconResult, mu *sync.Mutex, wg *sync.WaitGroup, sem chan struct{}) bool {

	holders := p.Nodes
	var alive []string
	for _, id := range holders {
		if !dead[id] {
			alive = append(alive, id)
		}
	}

	// Over-replication (e.g., a node rejoined after its packs were
	// re-replicated elsewhere, or R was lowered): trim to target — register
	// first, delete last.
	if len(holders) > target && len(alive) == len(holders) {
		keep := append([]string(nil), holders[:target]...)
		drop := holders[target:]
		if err := r.Meta.SetPackNodes(ctx, repo, p.PackOID, keep); err != nil {
			log.Printf("reconciler %s: trim register %s: %v", repo, p.PackOID, err)
			return false
		}
		for _, id := range drop {
			if addr, ok := r.Health.addrOf(id); ok {
				_ = r.client(addr).DeletePack(ctx, r.nodeKey(ctx, repo), p.PackOID)
			}
		}
		mu.Lock()
		res.Trimmed++
		mu.Unlock()
		return false
	}

	// Under-replication: copy from a live holder to fresh healthy nodes,
	// then swap the registered holder set.
	if len(alive) < target {
		mu.Lock()
		res.UnderReplicated++
		mu.Unlock()
		holding := map[string]bool{}
		for _, id := range holders {
			holding[id] = true
		}
		var targets []meta.NodeInfo
		for _, n := range healthy {
			if !holding[n.ID] {
				targets = append(targets, n)
			}
		}
		need := target - len(alive)
		if need > len(targets) {
			need = len(targets)
		}
		if need == 0 {
			return false // not enough healthy nodes to reach target this pass
		}
		targets = targets[:need]

		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := r.copyPack(ctx, repo, p.PackOID, alive, targets); err != nil {
				log.Printf("reconciler %s: copy %s: %v", repo, p.PackOID, err)
				mu.Lock()
				res.Failed++
				mu.Unlock()
				return
			}
			mu.Lock()
			res.Copied++
			mu.Unlock()
		}()
		return true
	}
	return false
}

// nodeKey resolves the storage key for node-facing calls (docs/03 §6);
// identity under named addressing, the opaque @hashed path otherwise.
// Resolution failure degrades to the name — the call fails on the node
// instead of blocking the whole convergence pass.
func (r *Reconciler) nodeKey(ctx context.Context, repo string) string {
	key, err := r.Addressing.Key(ctx, repo)
	if err != nil {
		log.Printf("reconciler %s: addressing: %v", repo, err)
		return repo
	}
	return key
}

// SweepOptions tunes one fleet-sweep pass (docs/10 §8.7).
type SweepOptions struct {
	// MinAge protects in-flight writes: a byte-land → register span is one
	// sequential call chain measured in milliseconds, while a sweep pass is
	// an operator/batch concern — files younger than MinAge are never
	// touched. The default (an hour) buys ~5 orders of magnitude of margin.
	MinAge time.Duration
	// DryRun reports what a pass would reclaim without deleting.
	DryRun bool
}

// SweepResult reports one fleet-sweep pass (observability + tests).
type SweepResult struct {
	KeysDropped  int `json:"keys_dropped"`  // whole storage keys removed (deleted/renamed-away repos)
	PacksDropped int `json:"packs_dropped"` // unregistered or unheld packs removed
	// Failed counts per-node sweep errors (a node listing badly does not
	// stop the rest of the fleet).
	Failed int `json:"failed"`
}

func (o *SweepOptions) fill() {
	if o.MinAge <= 0 {
		o.MinAge = time.Hour
	}
}

// SweepOnce reclaims orphan bytes across the fleet (docs/10 §8.7) — the GC
// arm the reconciler never had: bytes a registry row no longer vouches for.
// Two phases per node, registry-driven:
//
//	whole-key:   a storage key no live repo resolves to (a deleted repo —
//	             hashed keeps its immutable key; named keeps its directory —
//	             or a rename laggard) is removed whole. Safe unordered: a
//	             repo row always exists before any byte lands under its key.
//	pack-level:  within a live key, a pack the registry does not know, or
//	             knows but holds ELSEWHERE (a trim whose node delete failed)
//	             is removed. Safety rests on register-follows-land being one
//	             sequential span, backed by the MinAge grace.
//
// tmp leftovers (.put-* from crashed landings) are cleaned per key under the
// same grace. DryRun reports the same numbers without deleting.
func (r *Reconciler) SweepOnce(ctx context.Context, opt SweepOptions) SweepResult {
	opt.fill()
	res := SweepResult{}
	healthy := r.Health.healthy()
	if len(healthy) == 0 {
		return res
	}

	// Live keys: every repo row (pools included — a pool is a repo) maps to
	// exactly one storage key; hashed keys derive from the immutable
	// StorageID and never collide with a dead name.
	repos, err := r.Meta.ListRepos(ctx)
	if err != nil {
		log.Printf("sweep: list repos: %v", err)
		res.Failed++
		return res
	}
	live := map[string]string{} // storage key → repo name
	for _, info := range repos {
		key, kerr := r.Addressing.Key(ctx, info.ID)
		if kerr != nil {
			log.Printf("sweep %s: addressing: %v", info.ID, kerr)
			continue
		}
		live[key] = info.ID
	}

	cutoff := time.Now().Add(-opt.MinAge)
	for _, n := range healthy {
		keys, err := r.client(n.Addr).ListKeys(ctx)
		if err != nil {
			log.Printf("sweep %s: list keys: %v", n.ID, err)
			res.Failed++
			continue
		}
		for _, key := range keys {
			repo, isLive := live[key]
			packs, err := r.client(n.Addr).ListRepoPacks(ctx, key)
			if err != nil {
				log.Printf("sweep %s %s: list packs: %v", n.ID, key, err)
				res.Failed++
				continue
			}
			if !isLive {
				// Whole-key orphan — only once nothing inside is young.
				if newestOf(packs).After(cutoff) {
					continue
				}
				if !opt.DryRun {
					if err := r.client(n.Addr).DeleteRepo(ctx, key); err != nil {
						log.Printf("sweep %s %s: drop key: %v", n.ID, key, err)
						res.Failed++
						continue
					}
				}
				res.KeysDropped++
				continue
			}
			registry := map[model.OID]map[string]bool{}
			regPacks, err := r.Meta.ListPacks(ctx, repo)
			if err != nil {
				log.Printf("sweep %s: registry packs: %v", repo, err)
				res.Failed++
				continue
			}
			for _, p := range regPacks {
				holders := map[string]bool{}
				for _, id := range p.Nodes {
					holders[id] = true
				}
				registry[p.PackOID] = holders
			}
			for _, e := range packs {
				if !time.Unix(e.ModUnix, 0).Before(cutoff) {
					continue // younger than the grace: possibly in flight
				}
				holders, known := registry[e.OID]
				if known && holders[n.ID] {
					continue // registered and held here: live data
				}
				if !opt.DryRun {
					if err := r.client(n.Addr).DeletePack(ctx, key, e.OID); err != nil {
						log.Printf("sweep %s %s %s: drop pack: %v", n.ID, key, e.OID, err)
						res.Failed++
						continue
					}
				}
				res.PacksDropped++
			}
			if !opt.DryRun {
				_, _, _ = r.client(n.Addr).CleanTmp(ctx, key, opt.MinAge)
			}
		}
	}
	return res
}

// newestOf returns the newest mtime among entries (zero time when empty).
func newestOf(packs []storage.NodePackEntry) time.Time {
	var t time.Time
	for _, e := range packs {
		if mt := time.Unix(e.ModUnix, 0); mt.After(t) {
			t = mt
		}
	}
	return t
}

// copyPack repairs one pack's replicas without ever holding it: the source
// is verified by a streaming trailer-hash pass, then each target streams
// its own copy straight from the source (the node's landing verifies the
// trailer + OID again on receipt). Register the new holder set last.
func (r *Reconciler) copyPack(ctx context.Context, repo string, packOID model.OID, aliveSrc []string, targets []meta.NodeInfo) error {
	key := r.nodeKey(ctx, repo)
	var srcAddr string
	for _, id := range aliveSrc {
		if addr, ok := r.Health.addrOf(id); ok {
			srcAddr = addr
			break
		}
	}
	if srcAddr == "" {
		return fmt.Errorf("no readable source among %v", aliveSrc)
	}
	if err := r.client(srcAddr).VerifyPack(ctx, key, packOID); err != nil {
		return fmt.Errorf("verify source: %w", err)
	}
	size, serr := r.client(srcAddr).HeadPack(ctx, key, packOID) // -1: no size check
	if serr != nil {
		size = -1
	}
	open := func() (io.ReadCloser, error) {
		rc, _, err := r.client(srcAddr).OpenPack(ctx, key, packOID)
		return rc, err
	}
	var confirmed []string
	for _, t := range targets {
		if err := r.client(t.Addr).PutPackStream(ctx, key, packOID, size, open, nil); err != nil {
			log.Printf("reconciler: put %s to %s: %v", packOID, t.ID, err)
			continue
		}
		confirmed = append(confirmed, t.ID)
	}
	if len(confirmed) == 0 {
		return fmt.Errorf("no target accepted the copy")
	}
	newHolders := append(append([]string(nil), aliveSrc...), confirmed...)
	return r.Meta.SetPackNodes(ctx, repo, packOID, newHolders)
}
