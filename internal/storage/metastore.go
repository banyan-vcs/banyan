package storage

import (
	"context"
	"fmt"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
)

// MetaStore 是中心元数据服务接口（docs/02 §2）：全局对象索引、
// pack 注册表、timeline head（CAS）、WAL 串行点。
// M1 内存实现 / M2 持久化实现可替换，上层不感知。
type MetaStore interface {
	// --- 仓库注册表 ---
	CreateRepo(ctx context.Context, repo string, algo model.HashAlgo) error
	GetRepo(ctx context.Context, repo string) (RepoInfo, error)

	// --- 全局对象索引（分布式检索入口，docs/02 §1.3）---
	// PutPack 登记一个新分片：对象 → (packOID, nodes)，并注册 pack。
	PutPack(ctx context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error
	// LocateObjects 批量定位：oid → 所在 pack 与节点。
	LocateObjects(ctx context.Context, repo string, oids []model.OID) (map[model.OID]ObjectLocation, error)

	// --- timeline heads（唯一串行点，CAS）---
	Timeline(ctx context.Context, tl model.Timeline) (model.TimelineState, error)
	// CASHead 乐观锁推进 head；expected 不符返回 ErrNonFastForward。
	// 全零 next 表示删除 timeline。
	CASHead(ctx context.Context, tl model.Timeline, expected, next model.OID) error
	// EnsureTimeline 幂等初始化：不存在时以 (head, base) 建立（子树
	// timeline 首次 push 前，其 head 是投影 head，base 记分叉点）。
	EnsureTimeline(ctx context.Context, tl model.Timeline, head, base model.OID) error
	ListTimelines(ctx context.Context, repo string, anchor model.AnchorPath) ([]model.Timeline, error)
}

// ObjectLocation 是全局索引的一条定位记录。
type ObjectLocation struct {
	PackOID model.OID
	NodeID  string
}

// RepoInfo 是仓库注册信息。
type RepoInfo struct {
	ID      string
	Algo    model.HashAlgo
	DefaultBranch string
}

// ErrNonFastForward 对应 git push 的 non-fast-forward（docs/01 §4.3）。
var ErrNonFastForward = fmt.Errorf("non-fast-forward: timeline head moved")

// ErrRepoNotFound / ErrTimelineUnknown.
var (
	ErrRepoNotFound     = fmt.Errorf("repo not found")
	ErrTimelineUnknown  = fmt.Errorf("timeline unknown")
)

// MemoryMetaStore 是单机内存实现（M1）。WAL 语义 = 单互斥锁内
// 读写原子；对象写入不进 WAL（CAS 幂等），只有 head/索引提交进。
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
		// ref 删除：head 归零即移除该 timeline 条目。
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
		return nil // 已存在（含已本地提交的情形）：不覆盖
	}
	m.timelines[tl.Repo][tl] = model.TimelineState{Head: head, Base: base}
	return nil
}
