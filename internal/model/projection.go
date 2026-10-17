package model

import (
	"fmt"
)

// git 投影（Projection，下行读路径）——docs/01 §5。
//
//   - 根投影：anchor="/" 的 commit 原样就是 git commit（零翻译）；
//   - 子树投影：DAG 版 subtree split——全父遍历，保留分支提交与
//     合并结构，语义对齐 `git log -- <path>` / `git subtree split`；
//     纯函数 ⇒ SHA 稳定 ⇒ 可缓存、可增量。

// ProjectionAlgoVer 是投影算法版本。变更此值 = 开辟新缓存空间，
// 不破坏既有客户端（旧投影对象仍有效，仅不再续写）。
const ProjectionAlgoVer = 1

// SubtreeAt 返回 root 树中 path 处子树的 OID（不存在时返回零值）。
func SubtreeAt(lookup ObjectLookup, root OID, path AnchorPath) OID {
	cur := root
	for _, seg := range path.Segments() {
		tree, err := lookup.Tree(cur)
		if err != nil {
			return ""
		}
		found := false
		for _, e := range tree.Entries {
			if e.Name == seg {
				if !e.IsDir {
					return ""
				}
				cur = e.Target
				found = true
				break
			}
		}
		if !found {
			return ""
		}
	}
	return cur
}

// SubtreeSplit 把一个根时间线 commit 投影为"以 path 子树为根"的
// 投影 commit（docs/01 §5.2）。确定性：同输入必同输出。
func SubtreeSplit(lookup ObjectLookup, algo HashAlgo, rootCommitOID OID, path AnchorPath) (*AnchoredCommit, error) {
	c, err := lookup.Commit(rootCommitOID)
	if err != nil {
		return nil, err
	}
	sub, err := RequireSubtreeAt(lookup, c.TreeOID, path)
	if err != nil {
		return nil, fmt.Errorf("split %s: %w", rootCommitOID, err)
	}
	return &AnchoredCommit{
		Anchor:        RootAnchor, // 投影结果在"以 P 为根"的虚拟仓库里是根锚点
		TreeOID:       sub,
		Parents:       nil, // 由 SplitHistory 统一回填（OID 定稿依赖 parent 先定稿）
		Author:        c.Author,
		Committer:     c.Committer,
		Message:       c.Message,
		ProjectedFrom: rootCommitOID,
	}, nil
}

// RequireSubtreeAt 同 SubtreeAt，但路径不存在时报错。
func RequireSubtreeAt(lookup ObjectLookup, root OID, path AnchorPath) (OID, error) {
	sub := SubtreeAt(lookup, root, path)
	if sub.IsZero() {
		return "", fmt.Errorf("%w: subtree %s", ErrObjectNotFound, path)
	}
	return sub, nil
}

// SplitResult 是子树投影输出。
type SplitResult struct {
	// Commits 为投影 commit（拓扑序，新→旧），仅含该路径发生变更的
	// 提交；分支提交独立成节点，合并点保留多条 parent 边。
	Commits []*AnchoredCommit
	// Head 是虚拟仓库的 head：head 本身或其最近投影祖先。
	Head OID
}

// SplitHistory 沿根时间线做 DAG 版 subtree split（docs/01 §5.2）。
//
// 算法（自旧向新，保证 OID 定稿无环）：
//  1. 全父 DFS 收集可达 commit，后序即拓扑序（旧→新）；
//  2. changed(C) = 路径子树存在，且（无父 或 任一父的子树 ≠ C 的子树）；
//  3. 自旧向新为 changed 提交建投影：Parents[P] = resolve(P)——沿父边
//     找到的最近投影祖先（首个非零，按父序优先），此时后继投影均已
//     定稿，OID 引用不会过期；resolve 对未变更节点做记忆化。
func SplitHistory(lookup ObjectLookup, algo HashAlgo, head OID, path AnchorPath) (*SplitResult, error) {
	const maxCommits = 1000000 // 防御失控；浅化截断由 M3 提供

	commits := map[OID]*AnchoredCommit{}
	visited := map[OID]bool{}
	var topo []OID // 旧→新
	steps := 0

	var visit func(oid OID) error
	visit = func(oid OID) error {
		if visited[oid] {
			return nil
		}
		visited[oid] = true
		c, err := lookup.Commit(oid)
		if err != nil {
			return err
		}
		commits[oid] = c
		steps++
		if steps > maxCommits {
			return fmt.Errorf("split history: exceeded %d commits", maxCommits)
		}
		for _, p := range c.Parents {
			if err := visit(p); err != nil {
				return err
			}
		}
		topo = append(topo, oid)
		return nil
	}
	if err := visit(head); err != nil {
		return nil, err
	}

	// 各 commit 在 path 处的子树（零值 = 路径不存在），记忆化。
	subOf := map[OID]OID{}
	for _, oid := range topo {
		subOf[oid] = SubtreeAt(lookup, commits[oid].TreeOID, path)
	}

	// changed 集：路径有且相对任一父发生变化（含根提交）。
	changed := map[OID]bool{}
	for _, oid := range topo {
		s := subOf[oid]
		if s.IsZero() {
			continue
		}
		c := commits[oid]
		for _, p := range c.Parents {
			if subOf[p] != s {
				changed[oid] = true
				break
			}
		}
		if len(c.Parents) == 0 {
			changed[oid] = true
		}
	}

	// 自旧向新：建投影、回填 parent、定稿 OID。
	res := &SplitResult{}
	projOID := map[OID]OID{}  // changed commit → 投影最终 OID
	resolveMemo := map[OID]OID{} // 未变更 commit → 最近投影祖先

	var resolve func(oid OID) OID
	resolve = func(oid OID) OID {
		if o, ok := projOID[oid]; ok {
			return o
		}
		if o, ok := resolveMemo[oid]; ok {
			return o
		}
		var out OID
		for _, p := range commits[oid].Parents {
			if r := resolve(p); !r.IsZero() {
				out = r
				break
			}
		}
		resolveMemo[oid] = out
		return out
	}

	for _, oid := range topo { // 旧→新：后继投影先定稿
		if !changed[oid] {
			continue
		}
		pc, err := SubtreeSplit(lookup, algo, oid, path)
		if err != nil {
			return nil, err
		}
		var parents []OID
		seenParent := map[OID]bool{}
		for _, p := range commits[oid].Parents {
			r := resolve(p)
			if !r.IsZero() && !seenParent[r] {
				seenParent[r] = true
				parents = append(parents, r)
			}
		}
		pc.Parents = parents
		projOID[oid] = pc.OID(algo) // 自此定稿，不再变更
		res.Commits = append(res.Commits, pc)
	}
	if len(res.Commits) == 0 {
		return res, nil
	}
	res.Head = resolve(head)
	if res.Head.IsZero() {
		res.Head = res.Commits[0].OID(algo) // 理论不可达：head 可达集内必有 changed 节点
	}
	// 输出反转成 新→旧，方便调用方取 head/遍历。
	for i, j := 0, len(res.Commits)-1; i < j; i, j = i+1, j-1 {
		res.Commits[i], res.Commits[j] = res.Commits[j], res.Commits[i]
	}
	return res, nil
}
