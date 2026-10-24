// Package refs implements the refs service (docs/11 §4): the serialization
// point of the cluster — refs/timelines CAS (the git "precedence judgment"),
// the object index, placement records, the storage-node health table and the
// replica reconciler. One process, one writer; HA story per docs/11 §4.
package refs

import (
	"sync"
	"time"

	"github.com/banyan-vcs/banyan/internal/storage"
)

// Node states (docs/11 §8.1): two thresholds plus a planned mode.
const (
	StateHealthy     = "healthy"
	StateSuspect     = "suspect"     // missed heartbeats: excluded from placement, no copying
	StateDead        = "dead"        // past the deadline: triggers re-replication
	StateMaintenance = "maintenance" // planned: excluded from placement, no re-replication
)

// HealthTable tracks storage nodes by heartbeat age (docs/11 §8.1).
type HealthTable struct {
	mu           sync.Mutex
	nodes        map[string]*nodeRecord
	suspectAfter time.Duration
	deadAfter    time.Duration
}

type nodeRecord struct {
	ID          string
	Addr        string
	LastSeen    time.Time
	State       string
	StateAt     time.Time
	Maintenance bool
}

// NodeStatus is the externally visible health record.
type NodeStatus struct {
	ID       string    `json:"id"`
	Addr     string    `json:"addr"`
	State    string    `json:"state"`
	LastSeen time.Time `json:"last_seen"`
}

func NewHealthTable(suspectAfter, deadAfter time.Duration) *HealthTable {
	if suspectAfter <= 0 {
		suspectAfter = 30 * time.Second
	}
	if deadAfter <= suspectAfter {
		deadAfter = suspectAfter + 5*time.Minute
	}
	return &HealthTable{
		nodes:        map[string]*nodeRecord{},
		suspectAfter: suspectAfter,
		deadAfter:    deadAfter,
	}
}

func (h *HealthTable) Heartbeat(id, addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.nodes[id]
	if !ok {
		rec = &nodeRecord{ID: id, State: StateHealthy}
		h.nodes[id] = rec
	}
	rec.Addr = addr
	rec.LastSeen = time.Now()
}

func (h *HealthTable) SetMaintenance(id string, on bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.nodes[id]
	if !ok {
		return false
	}
	rec.Maintenance = on
	return true
}

// Tick recomputes states from heartbeat age (idempotent; driven by a ticker
// in production and called directly in tests).
func (h *HealthTable) Tick() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for _, rec := range h.nodes {
		state := rec.State
		switch {
		case rec.Maintenance:
			state = StateMaintenance
		case now.Sub(rec.LastSeen) >= h.deadAfter:
			state = StateDead
		case now.Sub(rec.LastSeen) >= h.suspectAfter:
			state = StateSuspect
		default:
			state = StateHealthy
		}
		if state != rec.State {
			rec.State = state
			rec.StateAt = now
		}
	}
}

// Snapshot lists every known node with its current state.
func (h *HealthTable) Snapshot() []NodeStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]NodeStatus, 0, len(h.nodes))
	for _, rec := range h.nodes {
		out = append(out, NodeStatus{ID: rec.ID, Addr: rec.Addr, State: rec.State, LastSeen: rec.LastSeen})
	}
	return out
}

// healthy returns the nodes eligible for placement, as storage.NodeInfo.
func (h *HealthTable) healthy() []storage.NodeInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []storage.NodeInfo
	for _, rec := range h.nodes {
		if rec.State == StateHealthy {
			out = append(out, storage.NodeInfo{ID: rec.ID, Addr: rec.Addr, State: rec.State})
		}
	}
	return out
}

// Healthy exposes the placement-eligible set (tests, status endpoints).
func (h *HealthTable) Healthy() []storage.NodeInfo { return h.healthy() }

// addrOf resolves a node ID to its address through the table (any state —
// a dead node's address is still where its files were).
func (h *HealthTable) addrOf(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.nodes[id]
	if !ok || rec.Addr == "" {
		return "", false
	}
	return rec.Addr, true
}

// deadSet returns the IDs of nodes in the dead state.
func (h *HealthTable) deadSet() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]bool{}
	for _, rec := range h.nodes {
		if rec.State == StateDead {
			out[rec.ID] = true
		}
	}
	return out
}
