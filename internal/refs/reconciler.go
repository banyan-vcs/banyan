package refs

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Reconciler converges actual pack holders toward each repo's replica target
// (docs/11 §8.3). Ordering discipline is the whole correctness argument:
// COPY first, REGISTER the new holder set, DELETE last — any interruption
// leaves at worst an over-replicated pack, never a registered holder without
// data, never a delete below the target.
type Reconciler struct {
	Meta        storage.MetaStore
	Health      *HealthTable
	DefaultR    int
	Concurrency int

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

func NewReconciler(meta storage.MetaStore, health *HealthTable, defaultR, concurrency int) *Reconciler {
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
func (r *Reconciler) convergePack(ctx context.Context, repo string, p storage.PackInfo, target int,
	dead map[string]bool, healthy []storage.NodeInfo, res *ReconResult, mu *sync.Mutex, wg *sync.WaitGroup, sem chan struct{}) bool {

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
				_ = r.client(addr).DeletePack(ctx, repo, p.PackOID)
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
		var targets []storage.NodeInfo
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

// copyPack streams one pack from a live holder to the target nodes and
// registers the new holder set: copy → verify checksum → register.
func (r *Reconciler) copyPack(ctx context.Context, repo string, packOID model.OID, aliveSrc []string, targets []storage.NodeInfo) error {
	var raw []byte
	for _, id := range aliveSrc {
		addr, ok := r.Health.addrOf(id)
		if !ok {
			continue
		}
		data, err := r.client(addr).GetPack(ctx, repo, packOID)
		if err != nil {
			continue
		}
		raw = data
		break
	}
	if raw == nil {
		return fmt.Errorf("no readable source among %v", aliveSrc)
	}
	if !storage.VerifyPackChecksum(packOID, raw) {
		return fmt.Errorf("checksum mismatch")
	}
	var confirmed []string
	for _, t := range targets {
		if err := r.client(t.Addr).PutPack(ctx, repo, packOID, raw, nil); err != nil {
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
