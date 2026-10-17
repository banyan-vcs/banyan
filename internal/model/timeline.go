package model

import (
	"fmt"
	"strings"
)

// AnchorPath 是规范化的锚定目录路径："/" 或 "/a/b"（无尾斜杠、无空段、无 ".."）。
type AnchorPath string

// ParseAnchorPath 校验并规范化锚点路径。
func ParseAnchorPath(s string) (AnchorPath, error) {
	if s == "" || s == "/" {
		return RootAnchor, nil
	}
	if !strings.HasPrefix(s, "/") {
		return "", fmt.Errorf("anchor path %q: must be absolute", s)
	}
	if strings.HasSuffix(s, "/") {
		return "", fmt.Errorf("anchor path %q: trailing slash", s)
	}
	for _, seg := range strings.Split(s[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("anchor path %q: invalid segment %q", s, seg)
		}
	}
	return AnchorPath(s), nil
}

// Segments 返回路径各段；根锚点返回空。
func (p AnchorPath) Segments() []string {
	if p == RootAnchor {
		return nil
	}
	return strings.Split(strings.TrimPrefix(string(p), "/"), "/")
}

// Parent 返回父锚点；根的自反（组装递归的终止条件）。
func (p AnchorPath) Parent() AnchorPath {
	segs := p.Segments()
	if len(segs) <= 1 {
		return RootAnchor
	}
	return AnchorPath("/" + strings.Join(segs[:len(segs)-1], "/"))
}

func (p AnchorPath) IsRoot() bool { return p == RootAnchor }

// JoinPath 在锚点下拼接一段子路径（用于虚拟仓库 URL → 锚点解析）。
func (p AnchorPath) JoinPath(rel string) AnchorPath {
	if rel == "" {
		return p
	}
	return AnchorPath(strings.TrimSuffix(string(p), "/") + "/" + strings.TrimPrefix(rel, "/"))
}

// Timeline 标识一条 commit 时间线：(repo, anchor, name)（docs/01 §2）。
// 根时间线（anchor="/"）语义等同 git branch；目录时间线即"该子目录
// 作为独立仓库"的历史。
type Timeline struct {
	Repo   string
	Anchor AnchorPath
	Name   string // 类 branch 名，如 "main"
}

// TimelineState 是 MetaStore 中的权威 head 记录。
type TimelineState struct {
	Head OID
	// Base 记录目录时间线分叉自根时间线的哪个 commit（快照分叉，
	// 显式 rebase 语义，docs/01 §5.2 / §7）。根时间线 Base 为零值。
	Base OID
}

// RefName 渲染该时间线在（虚拟）仓库内的 git 可见 ref。
func (t Timeline) RefName() string {
	return "refs/heads/" + t.Name
}

// InternalRef 渲染 banyan 内部 ref（仅 rpc 暴露，不上 git wire，docs/03 §6）。
func (t Timeline) InternalRef() string {
	return "refs/banyan/" + strings.TrimPrefix(string(t.Anchor), "/") + "/" + t.Name
}

// VirtualRepoPath 渲染虚拟仓库 URL 路径（/tree/ 前缀形式，docs/03 §1：
// 标准 git 客户端会把 URL 中 "//" 规范化，故对外统一用 /tree/）。
func (t Timeline) VirtualRepoPath() string {
	if t.Anchor.IsRoot() {
		return "/r/" + t.Repo
	}
	return "/r/" + t.Repo + "/tree" + string(t.Anchor)
}
