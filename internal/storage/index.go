package storage

import (
	"bytes"

	"github.com/banyan-vcs/banyan/internal/model"
)

// 内存派生索引（docs/02 §1.3 全局索引的补充层 + docs/04 commit-graph）：
//
//   - CommitGraph：oid → (tree, parents, when)，闭包计算与投影遍历
//     不再逐对象解码 commit 原始字节；push 时增量构建（对象在
//     IngestPack 里本就已解码，构建零额外 IO）。
//   - TreeMemo：oid → 解析后的条目表，路径行走（SubtreeAt/闭包展开）
//     每棵树只解析一次。
//
// 生命周期与进程一致（MetaStore 内存态，M2 随元数据一起持久化）。

// CommitInfo 是 commit-graph 的一条记录。
type CommitInfo struct {
	Tree    model.OID
	Parents []model.OID
	When    int64 // committer unix 时间（排序/generation 用）
}

type repoIndex struct {
	commits map[model.OID]*CommitInfo
	trees   map[model.OID][]model.TreeEntry
	// 轻量提交索引（docs/01 §11，设计 v3）：git oid → (tree 边, bmsg 引用)。
	// 信封内容寻址去重；push 时构建并逐条验证字节级还原（renderFails 应恒 0）。
	records    map[model.OID]*model.CommitRecord
	msgs       map[model.OID][]byte
	renderFails int
}

// buildIndex 在 push 入库时构建/合并索引。
func (s *ObjectService) buildIndex(repo string, dp *DecodedPack) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	ri := s.idx[repo]
	if ri == nil {
		ri = &repoIndex{
			commits: map[model.OID]*CommitInfo{},
			trees:   map[model.OID][]model.TreeEntry{},
			records: map[model.OID]*model.CommitRecord{},
			msgs:    map[model.OID][]byte{},
		}
		s.idx[repo] = ri
	}
	for oid, raw := range dp.Commits {
		if _, done := ri.commits[oid]; done {
			continue
		}
		if c, err := model.DecodeCommit(raw); err == nil {
			ri.commits[oid] = &CommitInfo{Tree: c.TreeOID, Parents: c.Parents, When: c.Committer.When.Unix()}
		}
		// 拆解为轻量记录 + bmsg（字节级还原验证；失败计数而非拒绝——
		// pack 原样在库，服务不受影响，仅轻量化索引缺此条）。
		if _, done := ri.records[oid]; done {
			continue
		}
		if tree, envelope, err := model.DecomposeCommit(raw); err == nil {
			if !bytes.Equal(model.RenderCommit(tree, envelope), raw) {
				ri.renderFails++
				continue
			}
			msgOID := model.MessageOID(envelope, s.Algo)
			if _, exists := ri.msgs[msgOID]; !exists {
				ri.msgs[msgOID] = envelope
			}
			ri.records[oid] = &model.CommitRecord{GitOID: oid, Tree: tree, Msg: msgOID}
		}
	}
	for oid, raw := range dp.Trees {
		if _, done := ri.trees[oid]; done {
			continue
		}
		if t, err := model.DecodeTree(raw); err == nil {
			ri.trees[oid] = t.Entries
		}
	}
}

// CommitIndexStats 返回轻量提交索引统计（提交数/唯一 bmsg/还原失败数）。
func (s *ObjectService) CommitIndexStats(repo string) (commits, uniqueMsgs, renderFails int) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	ri := s.idx[repo]
	if ri == nil {
		return 0, 0, 0
	}
	return len(ri.records), len(ri.msgs), ri.renderFails
}

// CommitInfo 取 commit 结构信息：索引命中直接返回；未命中（合成投影
// commit 等）解析一次并回填索引。
func (s *ObjectService) CommitInfo(repo string, oid model.OID) (*CommitInfo, error) {
	s.idxMu.Lock()
	if ci, ok := s.idx[repo].commits[oid]; ok {
		s.idxMu.Unlock()
		return ci, nil
	}
	s.idxMu.Unlock()

	t, raw, err := s.Raw(nil, repo, oid)
	if err != nil {
		return nil, err
	}
	if t != model.CommitType {
		return nil, model.ErrObjectNotFound
	}
	c, err := model.DecodeCommit(raw)
	if err != nil {
		return nil, err
	}
	ci := &CommitInfo{Tree: c.TreeOID, Parents: c.Parents, When: c.Committer.When.Unix()}
	s.idxMu.Lock()
	if s.idx[repo] == nil {
		s.idx[repo] = &repoIndex{commits: map[model.OID]*CommitInfo{}, trees: map[model.OID][]model.TreeEntry{}}
	}
	s.idx[repo].commits[oid] = ci
	s.idxMu.Unlock()
	return ci, nil
}

// TreeEntries 取 tree 条目表：索引命中直接返回；未命中解析一次并回填。
// 返回切片归索引所有，调用方不得修改。
func (s *ObjectService) TreeEntries(repo string, oid model.OID) ([]model.TreeEntry, error) {
	s.idxMu.Lock()
	if e, ok := s.idx[repo].trees[oid]; ok {
		s.idxMu.Unlock()
		return e, nil
	}
	s.idxMu.Unlock()

	t, raw, err := s.Raw(nil, repo, oid)
	if err != nil {
		return nil, err
	}
	if t != model.TreeType {
		return nil, model.ErrObjectNotFound
	}
	tree, err := model.DecodeTree(raw)
	if err != nil {
		return nil, err
	}
	s.idxMu.Lock()
	if s.idx[repo] == nil {
		s.idx[repo] = &repoIndex{commits: map[model.OID]*CommitInfo{}, trees: map[model.OID][]model.TreeEntry{}}
	}
	s.idx[repo].trees[oid] = tree.Entries
	s.idxMu.Unlock()
	return tree.Entries, nil
}
