package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/banyan-vcs/banyan/internal/metrics"
	"github.com/banyan-vcs/banyan/internal/model"
)

// The single writer's contention gauge (served at /metrics): every CAS
// rejected with non-fast-forward, counted at the CAS implementations so
// every topology (in-process meta, cluster refs) reports it.
var mCASConflicts = metrics.GetCounter("banyan_cas_conflicts_total",
	"ref CAS attempts rejected with non-fast-forward (contention gauge)")

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
	RepoBlobStore
	PoolRegistry
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
	DeleteHook(ctx context.Context, repo string, id string) error
}

// RepoBlobStore holds opaque per-repo blobs (the gitaly custom-hooks
// tarballs: banyan's hook plane is the HTTP callback registry, but the
// tarball round-trip keeps backup/restore portable).
type RepoBlobStore interface {
	SetRepoBlob(ctx context.Context, repo, key string, data []byte) error
	// GetRepoBlob answers the stored blob; missing = (nil, false, nil).
	GetRepoBlob(ctx context.Context, repo, key string) ([]byte, bool, error)
}

// PoolInfo records one repository↔object-pool relationship (docs/19). A
// repository is a member of at most one pool; the member that seeded the
// pool carries Upstream — the pool's origin in GitLab's fork dedup flow.
type PoolInfo struct {
	Member   string // repository name linked to the pool
	Pool     string // pool repository name (an @pools/ relative path)
	Upstream bool   // member is the pool's origin repository
}

// PoolRegistry tracks object-pool membership (gitaly ObjectPoolService).
// banyan has no alternates: linking materializes shared objects into the
// member, and this registry is the durable record of the relationship.
type PoolRegistry interface {
	// LinkPool upserts the member's pool record (one pool per member).
	LinkPool(ctx context.Context, member, pool string, upstream bool) error
	// UnlinkPool drops the member's pool record; missing = no-op.
	UnlinkPool(ctx context.Context, member string) error
	// PoolOf answers the member's pool; ("", false, nil) when unlinked.
	PoolOf(ctx context.Context, member string) (string, bool, error)
	// ListPoolMembers returns every membership record, member-sorted.
	ListPoolMembers(ctx context.Context) ([]PoolInfo, error)
	// DeletePoolRecords drops every record referencing the pool (member
	// records pointing at it and the pool's own upstream entry).
	DeletePoolRecords(ctx context.Context, pool string) error
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
	// BETWEEN BeginRename and this call — see ObjectService.RenameRepo. The
	// flip also lifts any rename fence covering `from`.
	RenameRepo(ctx context.Context, from, to string) error
	// BeginRename installs the rename fence — the write barrier's reject
	// side (docs/03 §6b). While the fence stands, every mutating operation
	// touching either name fails with ErrRepoRenaming, so a concurrent push
	// that started under the old name fails cleanly instead of landing a
	// confirmed-but-invisible write under the name that is about to die.
	// The flip (RenameRepo) or AbortRename lifts it; re-running BeginRename
	// is idempotent and re-installs the same fence after a crash.
	BeginRename(ctx context.Context, from, to string) error
	// AbortRename lifts the fence after a failed or abandoned rename
	// (idempotent; a fence that never existed is a no-op).
	AbortRename(ctx context.Context, from, to string) error
	// DeleteRepo drops every repo-keyed row (repos, timelines, packs,
	// pack_nodes, objects, placement, hooks) in one transaction. Storage
	// bytes are reclaimed separately via ObjectService.DeleteRepo. A missing
	// repo is a successful no-op — deletion is idempotent by contract.
	DeleteRepo(ctx context.Context, repo string) error
	// SetForcePush toggles the repo-level force-push switch (docs/04 §6a):
	// off (default) keeps the rollback guard; on, a rewind-shaped subtree
	// update lands and rebuilds the woven views from the new source chains.
	SetForcePush(ctx context.Context, repo string, enabled bool) error
	// SetDefaultBranch changes the advertised HEAD symref target (gitaly
	// CreateRepository default-branch semantics).
	SetDefaultBranch(ctx context.Context, repo string, branch string) error
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

// ErrRepoNotFound / ErrTimelineUnknown / ErrPackNotFound / ErrRepoRenaming.
var (
	ErrRepoNotFound    = fmt.Errorf("repo not found")
	ErrTimelineUnknown = fmt.Errorf("timeline unknown")
	ErrPackNotFound    = fmt.Errorf("pack not found")
	// ErrRepoRenaming: a rename fence covers this name (docs/03 §6b) — the
	// caller retries; pushes surface it as a retryable failure so the client
	// re-negotiates under the settled name.
	ErrRepoRenaming = fmt.Errorf("repo rename in progress")
	// ErrRepoRenamedAway: this name was retired by a completed rename (its
	// tombstone stands) — creating under it would ack pushes into a fresh,
	// disconnected repository. The name recycles when the renamed-to repo is
	// deleted.
	ErrRepoRenamedAway = fmt.Errorf("repo name retired by rename")
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
	repoBlobs  map[string]map[string][]byte  // repo → blob key → bytes (gitaly custom-hooks)
	pools      map[string]PoolInfo          // member repo → pool record
	renames    map[string]string            // fence: from → to (rename in flight, docs/03 §6b)
	retired    map[string]string            // tombstone: from → to (rename completed)
	tokens     map[string]TokenInfo         // sha256 → token (docs/07 §6)
}

func NewMemoryMetaStore() *MemoryMetaStore {
	return &MemoryMetaStore{
		repos:      map[string]RepoInfo{},
		packs:      map[string]map[model.OID][]string{},
		packObjs:   map[string]map[model.OID][]model.OID{},
		index:      map[string]map[model.OID]ObjectLocation{},
		timelines:  map[string]map[model.Timeline]model.TimelineState{},
		placements: map[string]int{},
		repoBlobs:  map[string]map[string][]byte{},
		hooks:      map[string]map[string]HookInfo{},
		tokens:     map[string]TokenInfo{},
		pools:      map[string]PoolInfo{},
		renames:    map[string]string{},
		retired:    map[string]string{},
	}
}

// fenceHit reports whether a rename fence covers the name (either side of an
// in-flight rename). Callers hold m.mu.
func (m *MemoryMetaStore) fenceHit(name string) bool {
	if _, ok := m.renames[name]; ok {
		return true
	}
	for _, to := range m.renames {
		if to == name {
			return true
		}
	}
	return false
}

// fenceErr is the fenced-mutation rejection (callers hold m.mu and have
// already confirmed the hit).
func fenceErr(name string) error {
	return fmt.Errorf("%w: %s", ErrRepoRenaming, name)
}

func (m *MemoryMetaStore) CreateRepo(_ context.Context, repo string, algo model.HashAlgo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
	// Rename tombstone (docs/03 §6b): a retired name must not be
	// resurrected — a late push under it would ack into a fresh,
	// disconnected repository.
	if to, ok := m.retired[repo]; ok {
		return fmt.Errorf("%w: %s was renamed to %s", ErrRepoRenamedAway, repo, to)
	}
	if _, exists := m.repos[repo]; exists {
		return fmt.Errorf("repo %s already exists", repo)
	}
	m.repos[repo] = RepoInfo{ID: repo, Algo: algo, DefaultBranch: "main", StorageID: NewStorageID()}
	return nil
}

// BeginRename installs the fence (docs/03 §6b). Fail-fast checks — source
// exists, target free, no overlapping rename — run here so a doomed rename
// never blocks the repo.
func (m *MemoryMetaStore) BeginRename(_ context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[from]; !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, from)
	}
	if _, exists := m.repos[to]; exists {
		return fmt.Errorf("repo %s already exists", to)
	}
	// Idempotent re-install of the SAME fence (rename re-run after a crash);
	// only a DIFFERENT overlapping rename is refused.
	if cur, ok := m.renames[from]; ok {
		if cur == to {
			return nil
		}
		return fenceErr(from + "->" + to)
	}
	for _, dst := range m.renames {
		if dst == from || dst == to {
			return fenceErr(from + "->" + to)
		}
	}
	m.renames[from] = to
	return nil
}

func (m *MemoryMetaStore) AbortRename(_ context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.renames[from] == to {
		delete(m.renames, from)
	}
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
	if h, ok := m.hooks[from]; ok {
		m.hooks[to] = h
		delete(m.hooks, from)
	}
	if b, ok := m.repoBlobs[from]; ok {
		m.repoBlobs[to] = b
		delete(m.repoBlobs, from)
	}
	if p, ok := m.pools[from]; ok {
		p.Member = to
		m.pools[to] = p
		delete(m.pools, from)
	}
	// The flip retires the fence into a tombstone: mutations under `to` are
	// legal from here, while re-creating `from` stays refused (a late push
	// under the dead name must not ack into a disconnected fresh repo).
	// Tombstones pointing at the old name retarget to the new one — the
	// hazard chain follows the data.
	delete(m.renames, from)
	m.retired[from] = to
	for name, dst := range m.retired {
		if dst == from && name != from {
			m.retired[name] = to
		}
	}
	return nil
}

// DeleteRepo drops every repo-keyed row. Missing = no-op (idempotent).
func (m *MemoryMetaStore) DeleteRepo(_ context.Context, repo string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
	delete(m.repos, repo)
	delete(m.packs, repo)
	delete(m.packObjs, repo)
	delete(m.index, repo)
	delete(m.timelines, repo)
	delete(m.placements, repo)
	delete(m.hooks, repo)
	delete(m.repoBlobs, repo)
	// A deleted repo leaves its pool (as member) and takes its pool records
	// with it (as pool): members of a vanished pool become unlinked.
	delete(m.pools, repo)
	for member, p := range m.pools {
		if p.Pool == repo {
			delete(m.pools, member)
		}
	}
	// Deletion retires the name for real: tombstones blocking older names
	// (retired into this repo) lift — the recycled name is creatable again.
	delete(m.retired, repo)
	for name, dst := range m.retired {
		if dst == repo {
			delete(m.retired, name)
		}
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
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
	info, ok := m.repos[repo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	info.ForcePush = enabled
	m.repos[repo] = info
	return nil
}

func (m *MemoryMetaStore) SetDefaultBranch(_ context.Context, repo string, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
	info, ok := m.repos[repo]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	info.DefaultBranch = branch
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
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
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
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
	if m.hooks[repo] != nil {
		delete(m.hooks[repo], id)
	}
	return nil
}

func (m *MemoryMetaStore) SetRepoBlob(_ context.Context, repo, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
	if m.repoBlobs[repo] == nil {
		m.repoBlobs[repo] = map[string][]byte{}
	}
	m.repoBlobs[repo][key] = append([]byte(nil), data...)
	return nil
}

func (m *MemoryMetaStore) GetRepoBlob(_ context.Context, repo, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.repoBlobs[repo][key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), data...), true, nil
}

func (m *MemoryMetaStore) LinkPool(_ context.Context, member, pool string, upstream bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(member) || m.fenceHit(pool) {
		return fenceErr(member)
	}
	m.pools[member] = PoolInfo{Member: member, Pool: pool, Upstream: upstream}
	return nil
}

func (m *MemoryMetaStore) UnlinkPool(_ context.Context, member string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(member) {
		return fenceErr(member)
	}
	delete(m.pools, member)
	return nil
}

func (m *MemoryMetaStore) PoolOf(_ context.Context, member string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[member]
	return p.Pool, ok, nil
}

func (m *MemoryMetaStore) ListPoolMembers(_ context.Context) ([]PoolInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PoolInfo, 0, len(m.pools))
	for _, p := range m.pools {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Member < out[j].Member })
	return out, nil
}

func (m *MemoryMetaStore) DeletePoolRecords(_ context.Context, pool string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(pool) {
		return fenceErr(pool)
	}
	for member, p := range m.pools {
		if p.Pool == pool {
			delete(m.pools, member)
		}
	}
	return nil
}

func (m *MemoryMetaStore) PutPack(_ context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
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
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
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
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
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
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
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
	if m.fenceHit(tl.Repo) {
		return fenceErr(tl.Repo)
	}
	expected = expected.NormalizeZero()
	next = next.NormalizeZero()
	if m.timelines[tl.Repo] == nil {
		m.timelines[tl.Repo] = map[model.Timeline]model.TimelineState{}
	}
	cur := m.timelines[tl.Repo][tl]
	if cur.Head != expected {
		mCASConflicts.With().Inc()
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
	if m.fenceHit(tl.Repo) {
		return fenceErr(tl.Repo)
	}
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
