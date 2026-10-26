package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
)

// MetaStore is the central metadata service interface (docs/03 §2.1): a
// composite of four role interfaces, each with its own data shape and access
// pattern, so a backend can swap per block later without gitwire noticing.
// M1 in-memory / M2 SQLite implementations are swappable; callers can't tell.
type MetaStore interface {
	RepoRegistry
	PackRegistry
	ObjectIndex
	TimelineStore
	PlacementStore
	HookRegistry
}

// HookInfo is one registered receive hook (docs/04 §7): an HTTP callback
// evaluated by the serving gateway at push time. Configuration lives in the
// metadata plane so every gateway sees the same hooks — no gateway-local
// state in multi-gateway deployments.
type HookInfo struct {
	ID     string   `json:"id"`      // operator-chosen stable key (upsert by it)
	Dir    string   `json:"dir"`     // scope: "" = repo level, "lib" = directory subtree
	Events []string `json:"events"`  // pre-receive | update | post-receive
	URL    string   `json:"url"`     // callback target (2xx = allow)
	Secret string   `json:"secret,omitempty"`  // sent as X-Banyan-Hook-Secret
	TimeoutMS int   `json:"timeout_ms,omitempty"` // 0 = default 10s
}

// Valid hook event names.
var HookEvents = map[string]bool{"pre-receive": true, "update": true, "post-receive": true}

type HookRegistry interface {
	// ListHooks returns the repo's registered hooks (docs/04 §7).
	ListHooks(ctx context.Context, repo string) ([]HookInfo, error)
	// SetHook upserts one hook by ID. Validates events/ID shape.
	SetHook(ctx context.Context, repo string, h HookInfo) error
	// DeleteHook removes one hook by ID (missing = no-op).
	DeleteHook(ctx context.Context, repo, id string) error
}

func (h HookInfo) validate() error {
	if h.ID == "" || strings.ContainsAny(h.ID, "/\\ ") {
		return fmt.Errorf("hook id must be a non-empty single token")
	}
	if h.URL == "" || !strings.HasPrefix(h.URL, "http://") && !strings.HasPrefix(h.URL, "https://") {
		return fmt.Errorf("hook url must be http(s)")
	}
	for _, seg := range strings.FieldsFunc(h.Dir, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == "." || seg == ".." {
			return fmt.Errorf("hook dir must be a clean directory path")
		}
	}
	if strings.ContainsAny(h.Dir, "\\") || strings.HasPrefix(h.Dir, "/") || strings.HasSuffix(h.Dir, "/") {
		return fmt.Errorf("hook dir must be a clean directory path (no leading/trailing slash)")
	}
	if len(h.Events) == 0 {
		return fmt.Errorf("hook needs at least one event")
	}
	for _, e := range h.Events {
		if !HookEvents[e] {
			return fmt.Errorf("unknown hook event %q", e)
		}
	}
	return nil
}

// RepoRegistry is the repo table.
type RepoRegistry interface {
	CreateRepo(ctx context.Context, repo string, algo model.HashAlgo) error
	GetRepo(ctx context.Context, repo string) (RepoInfo, error)
	// ListRepos enumerates all repos (CLI repo ls / Gitaly RepositoryService).
	ListRepos(ctx context.Context) ([]RepoInfo, error)
	// RenameRepo renames a repository across the registry (docs/03 §6):
	// every repo-keyed row moves in one transaction; the immutable StorageID
	// rides along untouched. Storage-layer moves (named addressing) happen
	// BEFORE this call — see ObjectService.RenameRepo.
	RenameRepo(ctx context.Context, from, to string) error
	// SetForcePush toggles the repo-level force-push switch (docs/04 §6a):
	// off (default) keeps the rollback guard; on, a rewind-shaped subtree
	// update lands and rebuilds the woven views from the new source chains.
	SetForcePush(ctx context.Context, repo string, enabled bool) error
}

// PackRegistry is the central pack table: pack → replica nodes. Packs are
// immutable, so registration is INSERT-ONLY and idempotent (first writer
// wins). GetPack/ListPacks serve cross-node pack fetch and repack.
type PackRegistry interface {
	// PutPack registers a new shard: object → (packOID, nodes), and registers the pack.
	PutPack(ctx context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error
	GetPack(ctx context.Context, repo string, packOID model.OID) (PackInfo, error)
	ListPacks(ctx context.Context, repo string) ([]PackInfo, error)
	// PackObjects lists the object set of one pack (repack merge input).
	PackObjects(ctx context.Context, repo string, packOID model.OID) ([]model.OID, error)
	// DropPack unregisters a pack and its object locations (repack old-shard
	// cleanup after the replacement is registered). Crash window between
	// registration and drop leaves duplicate locations — still correct, the
	// next repack cleans up.
	DropPack(ctx context.Context, repo string, packOID model.OID) error
	// SetPackNodes atomically replaces a pack's holder set (reconciler
	// holder swap after re-replication / trim — docs/10 §8.3). The pack must
	// already be registered.
	SetPackNodes(ctx context.Context, repo string, packOID model.OID, nodes []string) error
}

// PlacementStore is the per-repo replica target (docs/10 §3/§6): 0 or a
// missing row means "use the cluster default". R is adjustable at any time;
// the reconciler converges actual holders toward it.
type PlacementStore interface {
	SetPlacement(ctx context.Context, repo string, replicas int) error
	GetPlacement(ctx context.Context, repo string) (int, error)
}

// ObjectIndex is the global object index (distributed lookup entry,
// docs/03 §1.3): batch point lookup oid → (pack, node).
type ObjectIndex interface {
	LocateObjects(ctx context.Context, repo string, oids []model.OID) (map[model.OID]ObjectLocation, error)
}

// TimelineStore is the branch ref-map: (repo, anchor, name) → {head, base}.
// CASHead is the sole serialization point for ref updates.
type TimelineStore interface {
	Timeline(ctx context.Context, tl model.Timeline) (model.TimelineState, error)
	// CASHead advances the head via optimistic lock; an expected mismatch
	// returns ErrNonFastForward. An all-zero next deletes the timeline.
	CASHead(ctx context.Context, tl model.Timeline, expected, next model.OID) error
	// EnsureTimeline idempotent init: creates with (head, base) when absent
	// (before a subtree timeline's first push, its head is the projection head; base records the fork point).
	EnsureTimeline(ctx context.Context, tl model.Timeline, head, base model.OID) error
	ListTimelines(ctx context.Context, repo string, anchor model.AnchorPath) ([]model.Timeline, error)
	// ListAllTimelines enumerates every timeline of the repo across anchors
	// (root view materialization input, docs/02 §13).
	ListAllTimelines(ctx context.Context, repo string) ([]model.Timeline, error)
}

// ObjectLocation is one global-index location record.
type ObjectLocation struct {
	PackOID model.OID `json:"pack"`
	NodeID  string    `json:"node"`
}

// PackInfo is one central-pack record.
type PackInfo struct {
	PackOID model.OID `json:"pack_oid"`
	Nodes   []string  `json:"nodes"` // holder node IDs, sorted
}

// RepoInfo is repo registration info.
type RepoInfo struct {
	ID            string         `json:"id"`
	Algo          model.HashAlgo `json:"algo"`
	DefaultBranch string         `json:"default_branch"`
	// StorageID is the immutable per-repo storage identity (docs/03 §6):
	// hashed addressing derives its opaque on-disk key from it, so renames
	// never move data. Empty only for rows predating the column.
	StorageID string `json:"storage_id,omitempty"`
	// ForcePush is the repo-level force-push switch (docs/04 §6a): off
	// (default) keeps the subtree rollback guard; on, rewind-shaped subtree
	// updates land and rebuild the woven views.
	ForcePush bool `json:"force_push,omitempty"`
}

// ErrNonFastForward mirrors git push non-fast-forward (docs/02 §4.3).
var ErrNonFastForward = fmt.Errorf("non-fast-forward: timeline head moved")

// ErrRepoNotFound / ErrTimelineUnknown / ErrPackNotFound.
var (
	ErrRepoNotFound    = fmt.Errorf("repo not found")
	ErrTimelineUnknown = fmt.Errorf("timeline unknown")
	ErrPackNotFound    = fmt.Errorf("pack not found")
)

// MemoryMetaStore is the single-node in-memory implementation (M1). WAL semantics
// = atomic reads/writes under one mutex; object writes skip the WAL (CAS idempotent); only head/index commits enter it.
type MemoryMetaStore struct {
	mu         sync.Mutex
	repos      map[string]RepoInfo
	packs      map[string]map[model.OID][]string    // repo → packOID → nodes
	packObjs   map[string]map[model.OID][]model.OID // repo → packOID → objects
	index      map[string]map[model.OID]ObjectLocation
	timelines  map[string]map[model.Timeline]model.TimelineState
	placements map[string]int             // repo → replica target (0 = cluster default)
	hooks      map[string]map[string]HookInfo // repo → hook ID → registration
}

func NewMemoryMetaStore() *MemoryMetaStore {
	return &MemoryMetaStore{
		repos:      map[string]RepoInfo{},
		packs:      map[string]map[model.OID][]string{},
		packObjs:   map[string]map[model.OID][]model.OID{},
		index:      map[string]map[model.OID]ObjectLocation{},
		timelines:  map[string]map[model.Timeline]model.TimelineState{},
		placements: map[string]int{},
		hooks:      map[string]map[string]HookInfo{},
	}
}

func (m *MemoryMetaStore) CreateRepo(_ context.Context, repo string, algo model.HashAlgo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.repos[repo]; exists {
		return fmt.Errorf("repo %s already exists", repo)
	}
	m.repos[repo] = RepoInfo{ID: repo, Algo: algo, DefaultBranch: "main", StorageID: NewStorageID()}
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

func (m *MemoryMetaStore) RenameRepo(_ context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.repos[from]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, from)
	}
	if _, exists := m.repos[to]; exists {
		return fmt.Errorf("repo %s already exists", to)
	}
	delete(m.repos, from)
	m.repos[to] = info
	m.packs[to], m.packs[from] = m.packs[from], nil
	delete(m.packs, from)
	m.packObjs[to], m.packObjs[from] = m.packObjs[from], nil
	delete(m.packObjs, from)
	m.index[to], m.index[from] = m.index[from], nil
	delete(m.index, from)
	m.timelines[to], m.timelines[from] = m.timelines[from], nil
	delete(m.timelines, from)
	if p, ok := m.placements[from]; ok {
		m.placements[to] = p
		delete(m.placements, from)
	}
	return nil
}

func (m *MemoryMetaStore) ListRepos(_ context.Context) ([]RepoInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RepoInfo, 0, len(m.repos))
	for _, info := range m.repos {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryMetaStore) SetForcePush(_ context.Context, repo string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.repos[repo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	info.ForcePush = enabled
	m.repos[repo] = info
	return nil
}

func (m *MemoryMetaStore) ListHooks(_ context.Context, repo string) ([]HookInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []HookInfo
	for _, h := range m.hooks[repo] {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryMetaStore) SetHook(_ context.Context, repo string, h HookInfo) error {
	if err := h.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[repo]; !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	if m.hooks[repo] == nil {
		m.hooks[repo] = map[string]HookInfo{}
	}
	m.hooks[repo][h.ID] = h
	return nil
}

func (m *MemoryMetaStore) DeleteHook(_ context.Context, repo, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hooks[repo] != nil {
		delete(m.hooks[repo], id)
	}
	return nil
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
	if m.packObjs[repo] == nil {
		m.packObjs[repo] = map[model.OID][]model.OID{}
	}
	m.packObjs[repo][packOID] = append([]model.OID(nil), objects...)
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

func (m *MemoryMetaStore) GetPack(_ context.Context, repo string, packOID model.OID) (PackInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodes, ok := m.packs[repo][packOID]
	if !ok {
		return PackInfo{}, fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	return PackInfo{PackOID: packOID, Nodes: append([]string(nil), nodes...)}, nil
}

func (m *MemoryMetaStore) ListPacks(_ context.Context, repo string) ([]PackInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PackInfo
	for packOID, nodes := range m.packs[repo] {
		out = append(out, PackInfo{PackOID: packOID, Nodes: append([]string(nil), nodes...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PackOID < out[j].PackOID })
	return out, nil
}

func (m *MemoryMetaStore) PackObjects(_ context.Context, repo string, packOID model.OID) ([]model.OID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	objs, ok := m.packObjs[repo][packOID]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	return append([]model.OID(nil), objs...), nil
}

func (m *MemoryMetaStore) DropPack(_ context.Context, repo string, packOID model.OID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.packs[repo][packOID]; !ok {
		return fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	delete(m.packs[repo], packOID)
	delete(m.packObjs[repo], packOID)
	for oid, loc := range m.index[repo] {
		if loc.PackOID == packOID {
			delete(m.index[repo], oid)
		}
	}
	return nil
}

func (m *MemoryMetaStore) SetPackNodes(_ context.Context, repo string, packOID model.OID, nodes []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.packs[repo][packOID]; !ok {
		return fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	m.packs[repo][packOID] = append([]string(nil), nodes...)
	for oid, loc := range m.index[repo] {
		if loc.PackOID == packOID && len(nodes) > 0 {
			m.index[repo][oid] = ObjectLocation{PackOID: packOID, NodeID: nodes[0]}
		}
	}
	return nil
}

func (m *MemoryMetaStore) SetPlacement(_ context.Context, repo string, replicas int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[repo]; !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	m.placements[repo] = replicas
	return nil
}

func (m *MemoryMetaStore) GetPlacement(_ context.Context, repo string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[repo]; !ok {
		return 0, fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	return m.placements[repo], nil
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

func (m *MemoryMetaStore) ListAllTimelines(_ context.Context, repo string) ([]model.Timeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Timeline
	for tl := range m.timelines[repo] {
		out = append(out, tl)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Anchor != out[j].Anchor {
			return out[i].Anchor < out[j].Anchor
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
