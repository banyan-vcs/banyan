package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
)

// ObjectService 是读路径的编排器（docs/02 §3）：
// 定位（全局索引）→ 本地 cache → 跨节点拉取 → 解码对象。
// 它同时实现 model.ObjectLookup，供模型层组装/投影直接使用。
type ObjectService struct {
	Meta   MetaStore
	Local  *LocalShardStore
	Nodes  []Node
	Algo   model.HashAlgo

	mu    sync.Mutex
	cache map[string]map[model.OID]*DecodedPack // repo → packOID → 解码结果（LRU 化是 M2 的事）
	packOrder map[string]map[model.OID][]model.OID // repo → packOID → 对象清单（整 pack 直传判定）

	synthMu sync.Mutex
	synth   map[string]map[model.OID]synthObject // repo → oid → 服务端合成对象（投影 commit 等，docs/01 §5.2）

	idxMu sync.Mutex
	idx   map[string]*repoIndex // repo → 内存派生索引（docs/02 §1.3 / docs/04）
}

// synthObject 是服务端合成（非 push 而来）的对象，如子树投影 commit。
type synthObject struct {
	Type model.ObjectType
	Data []byte
}

func NewObjectService(meta MetaStore, local *LocalShardStore, nodes []Node, algo model.HashAlgo) *ObjectService {
	return &ObjectService{
		Meta:      meta,
		Local:     local,
		Nodes:     nodes,
		Algo:      algo,
		cache:     map[string]map[model.OID]*DecodedPack{},
		packOrder: map[string]map[model.OID][]model.OID{},
		synth:     map[string]map[model.OID]synthObject{},
		idx:       map[string]*repoIndex{},
	}
}

// PutSynthetic 物化一个服务端合成对象（幂等：内容寻址）。
func (s *ObjectService) PutSynthetic(repo string, oid model.OID, t model.ObjectType, data []byte) {
	s.synthMu.Lock()
	defer s.synthMu.Unlock()
	if s.synth[repo] == nil {
		s.synth[repo] = map[model.OID]synthObject{}
	}
	s.synth[repo][oid] = synthObject{Type: t, Data: data}
}

// Raw 统一读取：合成对象优先，其后按全局索引定位 pack。
// 返回对象类型与原始编码字节。
func (s *ObjectService) Raw(ctx context.Context, repo string, oid model.OID) (model.ObjectType, []byte, error) {
	s.synthMu.Lock()
	if so, ok := s.synth[repo][oid]; ok {
		s.synthMu.Unlock()
		return so.Type, so.Data, nil
	}
	s.synthMu.Unlock()

	dp, err := s.lookupPack(ctx, repo, oid)
	if err != nil {
		return "", nil, err
	}
	if b, ok := dp.Blobs[oid]; ok {
		return model.BlobType, b, nil
	}
	if t, ok := dp.Trees[oid]; ok {
		return model.TreeType, t, nil
	}
	if c, ok := dp.Commits[oid]; ok {
		return model.CommitType, c, nil
	}
	return "", nil, fmt.Errorf("%w: %s", model.ErrObjectNotFound, oid)
}

// IngestPack 处理一次 push 的 pack（docs/02 §4 写路径）：
// 解码 → 落盘真实 pack + idx v2（分片 CAS，不解散）→ 元数据登记
// → 构建 commit-graph / tree-entry 内存索引（docs/02 §1.3 双 idx 体系）。
func (s *ObjectService) IngestPack(ctx context.Context, repo string, packBytes []byte) (model.OID, error) {
	dp, err := DecodePack(packBytes)
	if err != nil {
		return "", err
	}
	nodes, err := PickNodes(s.Nodes, string(dp.PackOID), 3)
	if err != nil {
		return "", err
	}

	// idx v2：offsets 配对齐全才写；crc 段暂为 0（TODO(M2)，见 idx.go）。
	var idxBytes []byte
	if len(dp.Offsets) == len(dp.Order) {
		if b, err := EncodeIdx(s.Algo, dp.PackOID, dp.Offsets); err == nil {
			idxBytes = b
		} else {
			log.Printf("ingest %s: idx encode failed (skip): %v", repo, err)
		}
	}
	if err := s.Local.PutPack(repo, dp.PackOID, packBytes, idxBytes); err != nil {
		return "", err
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	// TODO(M2): 非 Local 节点的远端写入。
	if err := s.Meta.PutPack(ctx, repo, dp.PackOID, dp.Order, ids); err != nil {
		return "", err
	}
	s.buildIndex(repo, dp)

	s.mu.Lock()
	if s.cache[repo] == nil {
		s.cache[repo] = map[model.OID]*DecodedPack{}
	}
	s.cache[repo][dp.PackOID] = dp
	if s.packOrder[repo] == nil {
		s.packOrder[repo] = map[model.OID][]model.OID{}
	}
	s.packOrder[repo][dp.PackOID] = dp.Order
	s.mu.Unlock()
	return dp.PackOID, nil
}

// ExactPackHit 实现"热路径整 pack 直传"（docs/02 §5）：当请求对象集
// 恰好等于某个已存 pack 的对象集时返回该 pack OID，调用方可直接
// 流式回传 pack 字节，省去在线重编码（重哈希 + 重压缩）。
func (s *ObjectService) ExactPackHit(repo string, oids map[model.OID]bool) (model.OID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for packOID, order := range s.packOrder[repo] {
		if len(order) != len(oids) {
			continue
		}
		match := true
		for _, oid := range order {
			if !oids[oid] {
				match = false
				break
			}
		}
		if match {
			return packOID, true
		}
	}
	return "", false
}

// --- model.ObjectLookup 实现（模型层经此读对象）---

func (s *ObjectService) lookupPack(ctx context.Context, repo string, oid model.OID) (*DecodedPack, error) {
	locs, err := s.Meta.LocateObjects(ctx, repo, []model.OID{oid})
	if err != nil {
		return nil, err
	}
	loc, ok := locs[oid]
	if !ok {
		return nil, fmt.Errorf("%w: %s in repo %s", model.ErrObjectNotFound, oid, repo)
	}
	s.mu.Lock()
	dp := s.cache[repo][loc.PackOID]
	s.mu.Unlock()
	if dp != nil {
		return dp, nil
	}
	dp, err = s.Local.GetDecoded(repo, loc.PackOID)
	if err != nil {
		// TODO(M2): 从 loc.NodeID 远端拉取 pack 后重试。
		return nil, err
	}
	s.mu.Lock()
	if s.cache[repo] == nil {
		s.cache[repo] = map[model.OID]*DecodedPack{}
	}
	s.cache[repo][loc.PackOID] = dp
	if s.packOrder[repo] == nil {
		s.packOrder[repo] = map[model.OID][]model.OID{}
	}
	s.packOrder[repo][loc.PackOID] = dp.Order
	s.mu.Unlock()
	return dp, nil
}

func (s *ObjectService) BlobOrTreeOrCommit(ctx context.Context, repo string, oid model.OID) ([]byte, error) {
	_, data, err := s.Raw(ctx, repo, oid)
	return data, err
}

// repoLookup 给模型层按 repo 绑定一个 ObjectLookup 视图。
type repoLookup struct {
	svc  *ObjectService
	repo string
}

func (s *ObjectService) Lookup(repo string) model.ObjectLookup {
	return &repoLookup{svc: s, repo: repo}
}

// TreeStoreFor 返回带写入能力的仓库视图（组装用，docs/01 §4）：
// 读取走缓存/包，PutTree 把组装改写出的新树持久化进合成对象层。
func (s *ObjectService) TreeStoreFor(repo string) model.TreeStore {
	return &synthTreeStore{lookup: s.Lookup(repo), svc: s, repo: repo}
}

type synthTreeStore struct {
	lookup model.ObjectLookup
	svc    *ObjectService
	repo   string
}

func (a *synthTreeStore) Blob(oid model.OID) ([]byte, error)  { return a.lookup.Blob(oid) }
func (a *synthTreeStore) Tree(oid model.OID) (*model.Tree, error) { return a.lookup.Tree(oid) }
func (a *synthTreeStore) Commit(oid model.OID) (*model.AnchoredCommit, error) {
	return a.lookup.Commit(oid)
}

func (a *synthTreeStore) PutTree(t *model.Tree) model.OID {
	encoded, err := t.Encode()
	if err != nil {
		return ""
	}
	oid := t.OID(a.svc.Algo)
	a.svc.PutSynthetic(a.repo, oid, model.TreeType, encoded)
	return oid
}

func (r *repoLookup) Blob(oid model.OID) ([]byte, error) {
	t, data, err := r.svc.Raw(context.Background(), r.repo, oid)
	if err != nil {
		return nil, err
	}
	if t != model.BlobType {
		return nil, fmt.Errorf("%w: %s is %s, want blob", model.ErrObjectNotFound, oid, t)
	}
	return data, nil
}

// Tree 走 tree-entry 索引（docs/02 §1.3）：解析一次、永久记忆化，
// 投影/闭包遍历中的重复路径行走全部命中缓存。
// 返回的切片归索引所有，调用方不得修改。
func (r *repoLookup) Tree(oid model.OID) (*model.Tree, error) {
	entries, err := r.svc.TreeEntries(r.repo, oid)
	if err != nil {
		return nil, err
	}
	return &model.Tree{Entries: entries}, nil
}

func (r *repoLookup) Commit(oid model.OID) (*model.AnchoredCommit, error) {
	t, data, err := r.svc.Raw(context.Background(), r.repo, oid)
	if err != nil {
		return nil, err
	}
	if t != model.CommitType {
		return nil, fmt.Errorf("%w: %s is %s, want commit", model.ErrObjectNotFound, oid, t)
	}
	return model.DecodeCommit(data)
}

// LocalShardStore 是 M1 的本地盘分片存储：
// <root>/<repo>/packs/<oid 前两位>/pack-<oid>.pack + .idx（git idx v2）。
type LocalShardStore struct {
	Root string
}

func NewLocalShardStore(root string) *LocalShardStore { return &LocalShardStore{Root: root} }

func (l *LocalShardStore) packPath(repo string, packOID model.OID) string {
	s := string(packOID)
	return filepath.Join(l.Root, repo, "packs", s[:2], "pack-"+s+".pack")
}

func (l *LocalShardStore) idxPath(repo string, packOID model.OID) string {
	s := string(packOID)
	return filepath.Join(l.Root, repo, "packs", s[:2], "pack-"+s+".idx")
}

// PutPack 落盘真实 pack 字节与 idx v2（idx 为空时仅写 pack）。
func (l *LocalShardStore) PutPack(repo string, packOID model.OID, packBytes, idxBytes []byte) error {
	paths := []string{l.packPath(repo, packOID)}
	if len(idxBytes) > 0 {
		paths = append(paths, l.idxPath(repo, packOID))
	}
	for i, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		data := packBytes
		if i == 1 {
			data = idxBytes
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// GetPackBytes 读取原始 pack 字节。
func (l *LocalShardStore) GetPackBytes(repo string, packOID model.OID) ([]byte, error) {
	return os.ReadFile(l.packPath(repo, packOID))
}

// GetDecoded 读回并解码 pack（miss 路径；对象体随后驻留 cache）。
// TODO(M2): idx 定位 offset → 单对象 inflate，免去整包解码。
func (l *LocalShardStore) GetDecoded(repo string, packOID model.OID) (*DecodedPack, error) {
	raw, err := l.GetPackBytes(repo, packOID)
	if err != nil {
		return nil, fmt.Errorf("%w: pack %s: %v", model.ErrObjectNotFound, packOID, err)
	}
	return DecodePack(raw)
}
