// Package console is the product-side control plane (docs/17): users and
// tokens, the logical repository tree ({group}/{project}[/path]), and the
// member/permission model whose inheritance runs along the registered-prefix
// chain. It owns its own database and never touches gateway metadata; the
// gateway consumes a policy snapshot and a signed identity header instead.
package console

import (
	"errors"
	"fmt"
	"strings"
)

// Role is a membership role. The rank order fixes the aggregation: along an
// inheritance chain roles only ever add up (docs/17 §4 — anchor-granularity
// access makes "child less than parent" unenforceable, so it is not offered).
type Role string

const (
	RoleReporter   Role = "reporter"   // fetch only
	RoleDeveloper  Role = "developer"  // fetch + push
	RoleMaintainer Role = "maintainer" // + subprojects, protected config
	RoleOwner      Role = "owner"      // + members, delete
)

func (r Role) Rank() int {
	switch r {
	case RoleReporter:
		return 1
	case RoleDeveloper:
		return 2
	case RoleMaintainer:
		return 3
	case RoleOwner:
		return 4
	}
	return 0
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r.Rank() > 0 }

// Sentinel errors surfaced through the API layer.
var (
	ErrNotFound = errors.New("console: not found")
	ErrExists   = errors.New("console: already exists")
	ErrInvalid  = errors.New("console: invalid argument")
)

// User is a product-side identity. v1 authenticates via personal access
// tokens only; passwords arrive with web sessions (M6.4).
type User struct {
	ID        int64
	Username  string
	Admin     bool
	CreatedAt int64
}

// Token is a personal access token. The plaintext exists once at creation;
// the store keeps only its SHA-256.
type Token struct {
	ID        int64
	UserID    int64
	Name      string
	TokenHash string
	ExpiresAt int64 // unix seconds; 0 = no expiry
	Revoked   bool
	CreatedAt int64
}

// Kind distinguishes the three node types of the resource tree.
type Kind string

const (
	KindGroup   Kind = "group"
	KindProject Kind = "project"
	KindDir     Kind = "dir"
)

// Node is one node of the logical resource tree (docs/17 §3). Groups sit at
// the root, projects under a group, and subprojects ("dirs") under a project.
// A dir's Segment is the repo-relative path and may contain slashes: the
// prefix chain is computed against registered nodes only, so registering
// "services/auth" does not require "services" to exist first.
type Node struct {
	ID         int64
	Kind       Kind
	ParentID   int64 // 0 for groups; group id for projects; project id for dirs
	Segment    string
	RepoUID    string // projects only: immutable physical repo name
	Visibility string // projects only: "private" (default) | "public"
	Protected  string // projects only: JSON array of protected branch names
	Archived   bool
	CreatedAt  int64
}

const (
	Private = "private"
	Public  = "public"
)

// Member is one (node, user) membership row. Attachments are direct; the
// effective role is the max over the chain (policy.go).
type Member struct {
	NodeID    int64
	UserID    int64
	Username  string
	Role      Role
	CreatedAt int64
}

// --- name and path validation ---

// ValidUsername: no dots so the signed identity header ("user.ts.hmac")
// stays unambiguously parseable.
func ValidUsername(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ValidSegment accepts one path element of a group/project/subproject name.
func ValidSegment(s string) bool {
	if len(s) < 1 || len(s) > 64 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return !strings.HasSuffix(s, ".git") // reserved by the git URL shape
}

// CleanSub normalizes a subtree path: no leading/trailing slash, no empty,
// "." or ".." segments. It returns "" (the repo root) for empty input.
func CleanSub(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	segs := strings.Split(p, "/")
	if len(segs) > 15 { // group + project + ≤15 keeps depth ≤ 16 cap intact
		return "", fmt.Errorf("%w: subtree path deeper than 15 segments", ErrInvalid)
	}
	for _, s := range segs {
		if !ValidSegment(s) {
			return "", fmt.Errorf("%w: bad path segment %q", ErrInvalid, s)
		}
	}
	return strings.Join(segs, "/"), nil
}

// SubSegments splits a clean subtree path into its segments; "" yields nil.
func SubSegments(sub string) []string {
	if sub == "" {
		return nil
	}
	return strings.Split(sub, "/")
}

// isSubPrefix reports whether dir is a path-prefix of sub (segment-wise).
func isSubPrefix(dir, sub string) bool {
	if dir == "" {
		return true
	}
	d, s := SubSegments(dir), SubSegments(sub)
	if len(d) > len(s) {
		return false
	}
	for i := range d {
		if d[i] != s[i] {
			return false
		}
	}
	return true
}
