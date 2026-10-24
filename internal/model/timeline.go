package model

import (
	"fmt"
	"strings"
)

// AnchorPath is a canonical anchoring directory path: "/" or "/a/b" (no trailing slash, no empty segments, no "..").
type AnchorPath string

// ParseAnchorPath validates and canonicalizes an anchor path.
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

// Segments returns the path segments; the root anchor returns none.
func (p AnchorPath) Segments() []string {
	if p == RootAnchor || p == "" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(string(p), "/"), "/")
}

// Parent returns the parent anchor; self-reflexive at the root (termination condition for assembly recursion).
func (p AnchorPath) Parent() AnchorPath {
	segs := p.Segments()
	if len(segs) <= 1 {
		return RootAnchor
	}
	return AnchorPath("/" + strings.Join(segs[:len(segs)-1], "/"))
}

func (p AnchorPath) IsRoot() bool { return p == RootAnchor }

// JoinPath appends a sub-path under the anchor (for virtual repo URL → anchor resolution).
func (p AnchorPath) JoinPath(rel string) AnchorPath {
	if rel == "" {
		return p
	}
	return AnchorPath(strings.TrimSuffix(string(p), "/") + "/" + strings.TrimPrefix(rel, "/"))
}

// Timeline identifies a commit timeline: (repo, anchor, name) (docs/01 §2).
// The root timeline (anchor="/") is semantically a git branch; a directory
// timeline is the history of "that subdirectory as an independent repo".
type Timeline struct {
	Repo   string     `json:"repo"`
	Anchor AnchorPath `json:"anchor"`
	Name   string     `json:"name"` // branch-like name, e.g. "main"
}

// TimelineState is the authoritative head record in MetaStore.
type TimelineState struct {
	Head OID `json:"head"`
	// Base records which root-timeline commit the directory timeline forked
	// from (snapshot fork, explicit rebase semantics, docs/01 §5.2 / §7).
	// The root timeline has a zero Base.
	Base OID `json:"base"`
}

// RefName renders the git-visible ref of this timeline inside the (virtual) repo.
func (t Timeline) RefName() string {
	return "refs/heads/" + t.Name
}

// InternalRef renders the banyan-internal ref (exposed only via rpc, never on the git wire, docs/03 §6).
func (t Timeline) InternalRef() string {
	return "refs/banyan/" + strings.TrimPrefix(string(t.Anchor), "/") + "/" + t.Name
}

// VirtualRepoPath renders the virtual repo URL path (/tree/ prefix form,
// docs/03 §1: git clients normalize "//" in URLs, so /tree/ is canonical).
func (t Timeline) VirtualRepoPath() string {
	if t.Anchor.IsRoot() {
		return "/r/" + t.Repo
	}
	return "/r/" + t.Repo + "/tree" + string(t.Anchor)
}
