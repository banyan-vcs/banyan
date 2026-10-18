package storage

import (
	"context"
	"fmt"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
)

// MetaStore is the central metadata service interface (docs/02 §2): global
// object index, pack registry, timeline heads (CAS), WAL serialization point.
// M1 in-memory / M2 persistent implementations are swappable; callers can't tell.
type MetaStore interface {
	// --- repo registry ---
	CreateRepo(ctx context.Context, repo string, algo model.HashAlgo) error
	GetRepo(ctx context.Context, repo string) (RepoInfo, error)

	// --- global object index (distributed lookup entry, docs/02 §1.3) ---
	// PutPack registers a new shard: object → (packOID, nodes), and registers the pack.
	PutPack(ctx context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error
	// LocateObjects batch-locates: oid → its pack and node.
	LocateObjects(ctx context.Context, repo string, oids []model.OID) (map[model.OID]ObjectLocation, error)

	// --- timeline heads (the sole serialization point, CAS) ---
	Timeline(ctx context.Context, tl model.Timeline) (model.TimelineState, error)
	// CASHead advances the head via optimistic lock; an expected mismatch
	// returns ErrNonFastForward. An all-zero next deletes the timeline.
	CASHead(ctx context.Context, tl model.Timeline, expected, next model.OID) error
	// EnsureTimeline idempotent init: creates with (head, base) when absent
	// (before a subtree timeline's first push, its head is the projection head; base records the fork point).
	EnsureTimeline(ctx context.Context, tl model.Timeline, head, base model.OID) error
	ListTimelines(ctx context.Context, repo string, anchor model.AnchorPath) ([]model.Timeline, error)
}

// ObjectLocation is one global-index location record.
type ObjectLocation struct {
	PackOID model.OID
	NodeID  string
}

// RepoInfo is repo registration info.
type RepoInfo struct {
	ID      string
	Algo    model.HashAlgo
	DefaultBranch string
}

// ErrNonFastForward mirrors git push non-fast-forward (docs/01 §4.3).
var ErrNonFastForward = fmt.Errorf("non-fast-forward: timeline head moved")

// ErrRepoNotFound / ErrTimelineUnknown.
var (
	ErrRepoNotFound     = fmt.Errorf("repo not found")
	ErrTimelineUnknown  = fmt.Errorf("timeline unknown")
)

// MemoryMetaStore is the single-node in-memory implementation (M1). WAL semantics
// = atomic reads/writes under one mutex; object writes skip the WAL (CAS idempotent); only head/index commits enter it.
type MemoryMetaStore struct {
	mu        sync.Mutex
	repos     map[string]RepoInfo
	packs     map[string]map[model.OID][]string             // repo → packOID → nodes
	index     map[string]map[model.OID]ObjectLocation       // repo → oid → location
	timelines map[string]map[model.Timeline]model.TimelineState
}

func NewMemoryMetaStore() *MemoryMetaStore {
	return &MemoryMetaStore{
		repos:     map[string]RepoInfo{},
		packs:     map[string]map[model.OID][]string{},
		index:     map[string]map[model.OID]ObjectLocation{},
		timelines: map[string]map[model.Timeline]model.TimelineState{},
	}
}

func (m *MemoryMetaStore) CreateRepo(_ context.Context, repo string, algo model.HashAlgo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.repos[repo]; exists {
		return fmt.Errorf("repo %s already exists", repo)
	}
	m.repos[repo] = RepoInfo{ID: repo, Algo: algo, DefaultBranch: "main"}
	return nil
}

func (m *MemoryMetaStore) GetRepo(_ context.Context, repo string) (RepoInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.repos[repo]
	if !ok {
		return RepoInfo{}, fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	return info, nil
}

func (m *MemoryMetaStore) PutPack(_ context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[repo]; !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	if m.packs[repo] == nil {
		m.packs[repo] = map[model.OID][]string{}
	}
	if m.index[repo] == nil {
		m.index[repo] = map[model.OID]ObjectLocation{}
	}
	m.packs[repo][packOID] = nodes
	for _, oid := range objects {
		m.index[repo][oid] = ObjectLocation{PackOID: packOID, NodeID: nodes[0]}
	}
	return nil
}

func (m *MemoryMetaStore) LocateObjects(_ context.Context, repo string, oids []model.OID) (map[model.OID]ObjectLocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[model.OID]ObjectLocation{}
	idx := m.index[repo]
	for _, oid := range oids {
		if loc, ok := idx[oid]; ok {
			out[oid] = loc
		}
	}
	return out, nil
}

func (m *MemoryMetaStore) Timeline(_ context.Context, tl model.Timeline) (model.TimelineState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.timelines[tl.Repo][tl]; ok {
		return s, nil
	}
	return model.TimelineState{}, ErrTimelineUnknown
}

func (m *MemoryMetaStore) CASHead(_ context.Context, tl model.Timeline, expected, next model.OID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	expected = expected.NormalizeZero()
	next = next.NormalizeZero()
	if m.timelines[tl.Repo] == nil {
		m.timelines[tl.Repo] = map[model.Timeline]model.TimelineState{}
	}
	cur := m.timelines[tl.Repo][tl]
	if cur.Head != expected {
		return ErrNonFastForward
	}
	if next == "" {
		// ref deletion: zeroing the head removes the timeline entry.
		delete(m.timelines[tl.Repo], tl)
		return nil
	}
	m.timelines[tl.Repo][tl] = model.TimelineState{Head: next, Base: cur.Base}
	return nil
}

func (m *MemoryMetaStore) ListTimelines(_ context.Context, repo string, anchor model.AnchorPath) ([]model.Timeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Timeline
	for tl := range m.timelines[repo] {
		if tl.Anchor == anchor {
			out = append(out, tl)
		}
	}
	return out, nil
}

func (m *MemoryMetaStore) EnsureTimeline(_ context.Context, tl model.Timeline, head, base model.OID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timelines[tl.Repo] == nil {
		m.timelines[tl.Repo] = map[model.Timeline]model.TimelineState{}
	}
	if _, exists := m.timelines[tl.Repo][tl]; exists {
		return nil // already exists (incl. locally committed): don't overwrite
	}
	m.timelines[tl.Repo][tl] = model.TimelineState{Head: head, Base: base}
	return nil
}
