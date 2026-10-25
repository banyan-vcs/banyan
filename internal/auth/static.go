package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

// StaticPolicy is the demo authorization implementation (docs/13 §2):
// a static user table with HTTP Basic, admin/protected-branch rules, a
// personal-branch namespace, and optional per-directory fetch restrictions.
// It is deliberately in-process and readable — production replaces it at
// the Service seam, never by weakening the wire enforcement points.
type StaticPolicy struct {
	Users  map[string]string   // user → password (demo-grade; plaintext by design)
	Admins []string            // see and do everything (protected branches, all personal refs)
	ProtectedBranches []string // push restricted to admins (e.g. main)
	// SubtreeAccess restricts fetch of a subtree virtual repo to the listed
	// users (directory granularity). Paths absent from the map are open to
	// every authenticated user.
	SubtreeAccess map[string][]string
}

func (p *StaticPolicy) isAdmin(user string) bool {
	for _, a := range p.Admins {
		if a == user {
			return true
		}
	}
	return false
}

// owner reports the personal-branch owner of a branch name: "alice/wip"
// belongs to alice when alice is a known user.
func (p *StaticPolicy) owner(branch string) string {
	user, _, ok := strings.Cut(branch, "/")
	if !ok {
		return ""
	}
	if _, known := p.Users[user]; !known {
		return ""
	}
	return user
}

func (p *StaticPolicy) isProtected(branch string) bool {
	for _, b := range p.ProtectedBranches {
		if b == branch {
			return true
		}
	}
	return false
}

// Identify verifies HTTP Basic credentials against the user table. A missing
// or wrong Authorization header yields the anonymous identity.
func (p *StaticPolicy) Identify(h *http.Request) Identity {
	user, pass, ok := basicAuth(h)
	if !ok {
		return Identity{}
	}
	want, known := p.Users[user]
	if !known || subtle.ConstantTimeCompare([]byte(pass), []byte(want)) != 1 {
		return Identity{}
	}
	return Identity{User: user, Name: user}
}

func basicAuth(h *http.Request) (user, pass string, ok bool) {
	v := h.Header.Get("Authorization")
	const prefix = "Basic "
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(v[len(prefix):])
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(raw), ":")
	return user, pass, ok && user != ""
}

// VisibleRefs: personal branches (refs/heads/<user>/*) are visible to their
// owner and admins; everything else to any authenticated user.
func (p *StaticPolicy) VisibleRefs(id Identity, _ string, refs []Ref) []Ref {
	out := make([]Ref, 0, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref.Name, "refs/heads/") {
			branch := strings.TrimPrefix(ref.Name, "refs/heads/")
			if owner := p.owner(branch); owner != "" && owner != id.User && !p.isAdmin(id.User) {
				continue
			}
		}
		out = append(out, ref)
	}
	return out
}

// CanFetch: anonymous is always denied; subtree virtual repos may carry a
// user allowlist; the root repo and unrestricted subtrees are open to every
// authenticated user.
func (p *StaticPolicy) CanFetch(id Identity, _, sub string) bool {
	if id.Anonymous() {
		return false
	}
	if allow, restricted := p.SubtreeAccess[sub]; restricted {
		for _, u := range allow {
			if u == id.User || p.isAdmin(id.User) {
				return true
			}
		}
		return false
	}
	return true
}

// CanPush: anonymous denied; personal branches only by their owner (and
// admins); protected branches only by admins; everything else by any
// authenticated user.
func (p *StaticPolicy) CanPush(id Identity, _, _, branch string) bool {
	if id.Anonymous() {
		return false
	}
	if owner := p.owner(branch); owner != "" {
		return owner == id.User || p.isAdmin(id.User)
	}
	if p.isProtected(branch) {
		return p.isAdmin(id.User)
	}
	return true
}
