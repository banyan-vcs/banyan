package model

import (
	"fmt"
	"strings"
)

// 统一 DAG 提交历史（设计 v2，docs/01 §10）。
//
// 根本约束：git 的 commit hash 绑定 (root tree, parents, 签名, message)，
// 因此"同一变更跨视图同 hash" ⇔ "两视图共享同一 commit 对象" ⇔ 统一 DAG。
// 由此得到三个原语：
//
//   - SubtreeTip：视图 tip——确定性薄提交（tree=子树, parent=根提交），
//     祖先即统一 DAG，父树历史以原始 hash 进入子树；展示过滤交给
//     git 原生 `git log -- .`（恰为"仅改到子树的提交才展示"）。
//   - AdoptSubtreePush：收编式组装——规范 subtree-merge commit
//     （parents=[根head, 子树push的C原样保留]），C 的 hash 在子树与
//     父视图中完全一致。
//   - 全部对象为纯 git 规范编码，不带任何 banyan 私有头。
//
// 旧 split 投影（SplitHistory）保留作派生分析用途，不再服务读路径。

// SubtreeTip 构造子树视图 tip：tree = 根提交在 path 处的子树，
// parents = [根提交]。
//
// 确定性：签名身份固定、时间取根提交的 committer 时间、message 由
// (path, rootOID) 决定 ⇒ 同一 (rootOID, path) 恒产生同一 hash（可缓存、
// 重启不变、客户端协商稳定）。
func SubtreeTip(lookup ObjectLookup, algo HashAlgo, rootOID OID, path AnchorPath) (*AnchoredCommit, error) {
	rc, err := lookup.Commit(rootOID)
	if err != nil {
		return nil, err
	}
	sub := SubtreeAt(lookup, rc.TreeOID, path)
	if sub.IsZero() {
		return nil, fmt.Errorf("%w: subtree %s at %s", ErrObjectNotFound, path, rootOID)
	}
	viewID := Identity{
		Name:  "banyan-view",
		Email: "view@banyan.local",
		When:  rc.Committer.When,
	}
	return &AnchoredCommit{
		Anchor:     RootAnchor, // 在"以 X 为根"的视图里就是根锚点
		TreeOID:    sub,
		Parents:    []OID{rootOID},
		Author:     viewID,
		Committer:  viewID,
		Message:    fmt.Sprintf("banyan: subtree view %s @ %s\n", path, rootOID),
	}, nil
}

// AdoptSubtreePush 把子树 push 的 commit C 收编进统一 DAG：生成 git
// 规范的 subtree-merge commit——tree = 根树中 path 替换为 C 的树，
// parents = [根 head, C]（C 原样保留），签名与 message 沿用 C（确定性）。
func AdoptSubtreePush(store TreeStore, algo HashAlgo, repo, rootName string, path AnchorPath, childCommit OID, heads HeadResolver) (*AnchoredCommit, error) {
	c, err := store.Commit(childCommit)
	if err != nil {
		return nil, fmt.Errorf("adopt: %w", err)
	}
	state, err := heads.TimelineHead(Timeline{Repo: repo, Anchor: RootAnchor, Name: rootName})
	if err != nil {
		return nil, fmt.Errorf("adopt: root timeline: %w", err)
	}
	var rootTree OID
	var parents []OID
	if !state.Head.IsZero() {
		rc, err := store.Commit(state.Head)
		if err != nil {
			return nil, err
		}
		rootTree = rc.TreeOID
		parents = []OID{state.Head, childCommit}
	} else {
		rootTree = emptyTreeOID(algo)
		parents = []OID{childCommit} // 无根历史：C 即全部
	}
	newRoot, err := ReplaceSubtree(store, algo, rootTree, path, c.TreeOID)
	if err != nil {
		return nil, err
	}
	subject := strings.SplitN(strings.TrimRight(c.Message, "\n"), "\n", 2)[0]
	return &AnchoredCommit{
		Anchor:    RootAnchor,
		TreeOID:   newRoot,
		Parents:   parents,
		Author:    c.Author,
		Committer: c.Committer,
		Message:   fmt.Sprintf("Merge subtree '%s': %s\n", path, subject),
	}, nil
}
