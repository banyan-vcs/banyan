package model

import (
	"bytes"
	"fmt"
	"strings"
)

// Message 对象与轻量提交（设计 v3，docs/01 §11）。
//
// commit 的源信息一分为二：
//
//	信封（envelope）＝ tree 行之外的一切：parents、author、committer、
//	额外头（gpgsig/mergetag 等多行头）、消息正文——存为独立的内容寻址
//	Message 对象（类型标签 bmsg，仅 banyan 内部，永不进入 git wire）；
//	轻量提交记录＝ (tree 边, bmsg 引用) 两个定长指针。
//
// git commit 对象因此成为**运行时渲染产物**：
//
//	render(tree, envelope) = "tree <oid>\n" + envelope   —— 字节级还原。
//
//	对 push 收编的 commit，信封原样保存 ⇒ 渲染恒等 ⇒ hash 恒等；
//	对 banyan 原生 commit（视图 tip / 收编 merge，及未来的快照与栈式
//	变更），渲染是确定性的（§10 的既有模式推广为普适机制）。
//
// 收益：提交记录退化为图上的定长边（CommitGraph 的原生形态）；
// 信封内容寻址去重（机器人/模板化 merge 消息在 monorepo 中大量重复）；
// metadata-only 修改（amend）只新增 bmsg 不动结构。
//
// 不变式：Decompose∘Render = 恒等（字节级）；bmsg 与 git 对象空间
// 因类型标签不同而天然隔离。

// DecomposeCommit 把 git commit 原始字节拆为 (tree, 信封)。
// 信封＝首行 tree 之后的一切原样字节（含多行头的续行与空行）。
// 首行不是 tree 头时报错——防御非规范对象，调用方按原样整体保存回退。
func DecomposeCommit(raw []byte) (OID, []byte, error) {
	i := bytes.IndexByte(raw, '\n')
	if i < 0 {
		return "", nil, fmt.Errorf("decompose commit: no header terminator")
	}
	first := raw[:i]
	if !bytes.HasPrefix(first, []byte("tree ")) {
		return "", nil, fmt.Errorf("decompose commit: first header %q is not tree", first)
	}
	tree := OID(strings.TrimSpace(string(first[5:])))
	if tree.IsZero() {
		return "", nil, fmt.Errorf("decompose commit: empty tree oid")
	}
	return tree, raw[i+1:], nil
}

// RenderCommit 运行时渲染 git commit：tree 行 + 信封，字节级还原。
func RenderCommit(tree OID, envelope []byte) []byte {
	out := make([]byte, 0, len(envelope)+len(tree)+6)
	out = append(out, "tree "...)
	out = append(out, tree...)
	out = append(out, '\n')
	return append(out, envelope...)
}

// MessageOID 计算信封的 bmsg 对象 ID：沿用 git 哈希方案但类型标签为
// bmsg——与 blob/tree/commit 的 OID 空间天然隔离，永不冲突。
func MessageOID(envelope []byte, algo HashAlgo) OID {
	return ObjectOID("bmsg", envelope, algo)
}

// CommitRecord 是轻量提交记录：git oid ↔ (tree 边, bmsg 引用)。
type CommitRecord struct {
	GitOID OID // 渲染产物的 git 对象 ID
	Tree   OID // tree 边（提交的核心语义）
	Msg    OID // bmsg 引用（全部元信息）
}
