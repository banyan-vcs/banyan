package model

import (
	"fmt"
)

// 组装算法（Assembly，上行写路径）——docs/01 §4。
//
// 信息流：子目录本地 commits ──ReplaceSubtree──▶ 父目录新 tree
//        ──合成──▶ 根 synthetic commit 入根时间线。
// 输入全部是不可变对象 ⇒ 无环、确定性、可缓存。

// TreeStore 在 ObjectLookup 之上增加树写入：组装与投影过程中改写出的
// 新树必须持久化（服务端语义），MemoryLookup / storage 层各自实现。
type TreeStore interface {
	ObjectLookup
	PutTree(t *Tree) OID
}

// ReplaceSubtree 返回把 baseRoot 中 path 处子树替换为 subtree 后的新根树 OID。
// 路径上每一层的 tree 都被改写并经 store.PutTree 持久化（git tree 语义：
// 不可变、内容寻址）。path 为根时直接返回 subtree。
func ReplaceSubtree(store TreeStore, algo HashAlgo, baseRoot OID, path AnchorPath, subtree OID) (OID, error) {
	segs := path.Segments()
	if len(segs) == 0 {
		return subtree, nil
	}
	return replaceAt(store, algo, baseRoot, segs, subtree)
}

func replaceAt(store TreeStore, algo HashAlgo, treeOID OID, segs []string, subtree OID) (OID, error) {
	tree, err := store.Tree(treeOID)
	if err != nil {
		return "", fmt.Errorf("replace subtree: %w", err)
	}
	name := segs[0]
	replaced := false
	entries := make([]TreeEntry, 0, len(tree.Entries)+1)
	for _, e := range tree.Entries {
		if e.Name == name {
			if !e.IsDir && len(segs) > 1 {
				return "", fmt.Errorf("replace subtree: %q in %s is a file, not a directory", name, treeOID)
			}
			if len(segs) == 1 {
				entries = append(entries, TreeEntry{Name: name, Mode: "40000", Target: subtree, IsDir: true})
				replaced = true
			} else {
				child, err := replaceAt(store, algo, e.Target, segs[1:], subtree)
				if err != nil {
					return "", err
				}
				entries = append(entries, TreeEntry{Name: name, Mode: "40000", Target: child, IsDir: true})
				replaced = true
			}
		} else {
			entries = append(entries, e)
		}
	}
	if !replaced {
		if len(segs) == 1 {
			entries = append(entries, TreeEntry{Name: name, Mode: "40000", Target: subtree, IsDir: true})
		} else {
			// 中间层不存在：逐层建出缺失目录。
			child, err := replaceAt(store, algo, emptyTreeOID(algo), segs[1:], subtree)
			if err != nil {
				return "", err
			}
			entries = append(entries, TreeEntry{Name: name, Mode: "40000", Target: child, IsDir: true})
		}
	}
	return store.PutTree(&Tree{Entries: entries}), nil
}

func emptyTreeOID(algo HashAlgo) OID {
	return ObjectOID(TreeType, nil, algo)
}

// HeadResolver 提供时间线当前 head（由 MetaStore 实现）。
type HeadResolver interface {
	TimelineHead(tl Timeline) (TimelineState, error)
}

// CurrentTree 计算目录 D 的当前树（docs/01 §4.1）：
//
//	CurrentTree(D) = base(D) ⊕ replace-children(D)
//
// base(D) 取 D 本地时间线 head 的树；再对每个子目录把其 CurrentTree
// 替换进来。骨架实现采用惰性单层替换：只替换"有本地时间线"的子目录。
// 完整的递归组装在 M1 的 derived 引擎中补全（含多子目录合并序）。
func CurrentTree(lookup ObjectLookup, heads HeadResolver, algo HashAlgo, tl Timeline) (OID, error) {
	state, err := heads.TimelineHead(tl)
	if err != nil {
		return "", err
	}
	commit, err := lookup.Commit(state.Head)
	if err != nil {
		return "", err
	}
	root := commit.TreeOID
	// 枚举同 repo 下锚定在本目录子层的活跃时间线，逐个替换。
	// TODO(M1): 由 MetaStore.ListTimelines 提供；这里保留单层语义骨架。
	return root, nil
}

// AssembleRoot 在子目录提交后执行向上组装（docs/01 §4.2）：
//
//	1. root_new = ReplaceSubtree(CurrentTree("/"), path, C.TreeOID)
//	2. synthetic commit 入根时间线（M1 同步模式）
//
// 返回待写入的合成 commit；调用方负责把新对象入 CAS 与根 head CAS。
func AssembleRoot(store TreeStore, heads HeadResolver, algo HashAlgo, repo, rootName string, path AnchorPath, childCommit OID, by Identity) (*AnchoredCommit, error) {
	child, err := store.Commit(childCommit)
	if err != nil {
		return nil, fmt.Errorf("assemble: %w", err)
	}
	rootTL := Timeline{Repo: repo, Anchor: RootAnchor, Name: rootName}
	state, err := heads.TimelineHead(rootTL)
	if err != nil {
		return nil, fmt.Errorf("assemble: root timeline: %w", err)
	}
	var baseRoot OID
	var parents []OID
	if !state.Head.IsZero() {
		rootCommit, err := store.Commit(state.Head)
		if err != nil {
			return nil, err
		}
		baseRoot = rootCommit.TreeOID
		parents = []OID{state.Head}
	} else {
		baseRoot = emptyTreeOID(algo)
	}
	newRoot, err := ReplaceSubtree(store, algo, baseRoot, path, child.TreeOID)
	if err != nil {
		return nil, err
	}
	return &AnchoredCommit{
		Anchor:     RootAnchor,
		TreeOID:    newRoot,
		Parents:    parents,
		Author:     by,
		Committer:  by,
		Message:    fmt.Sprintf("banyan: assemble %s@%s\n", path, childCommit),
		Synthetic:  true,
	}, nil
}
