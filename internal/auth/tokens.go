// Token authentication as a decorator over any auth.Service (docs/gateway/authorization.md §6,
// docs/project/roadmap.md M5): personal access tokens live in the metadata plane (storage.
// meta.TokenRegistry — all-in-one and refs-serving deployments own it), the
// wrapped policy keeps providing passwords, SSH keys, visibility and branch
// rules. This is the extensibility seam in action: the wire layers still
// only ever see auth.Service; deployments compose
//
//	TokenPolicy{Inner: StaticPolicy{...}, Tokens: meta}
//
// or TokenPolicy{Inner: UpstreamPolicy{...}} where the metadata plane is
// local. Tokens never bypass the inner rules — they carry a scope CEILING
// (read < write < admin) on top of whatever the inner policy allows.
package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// AdminChecker is the optional management-plane extension of Service: the
// RPC layer gates repo/cluster management endpoints on it when the
// configured policy implements it (static admins, snapshot admins, or an
// admin-scoped token). Policies that don't implement it keep the
// authenticated-only behavior.
type AdminChecker interface {
	// CanAdmin decides management access: repo "" asks for global admin
	// (cluster/meta operations), a repo name for that repository's
	// management (repack, force-push switch, rename, delete).
	CanAdmin(id Identity, repo string) bool
}

// TokenPolicy wraps an inner Service with metadata-plane personal access
// tokens. Identification order per request: HTTP Basic whose password is a
// `banyan_`-prefixed token resolves against the registry (expiry and
// revocation enforced at read time); anything else delegates to Inner.
// A non-empty username must match the token owner — the console convention
// (docs/web/console.md §4); CI jobs mint their own per-user token.

type TokenPolicy struct {
	Inner  Service
	Tokens meta.TokenRegistry
	// Now overrides the clock (tests).
	Now func() time.Time
}

func (p *TokenPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Identify: token credentials resolve against the registry; everything
// else (passwords, missing headers) delegates to the inner policy. A
// presented token that fails any check is anonymous — no fallback to the
// inner table, the caller chose the token path.
func (p *TokenPolicy) Identify(h *http.Request) Identity {
	user, pass, ok := basicAuth(h)
	if ok && strings.HasPrefix(pass, "banyan_") {
		if id, valid := p.identifyToken(user, pass); valid {
			return id
		}
		return Identity{}
	}
	return p.Inner.Identify(h)
}

func (p *TokenPolicy) identifyToken(user, plaintext string) (Identity, bool) {
	t, err := p.Tokens.TokenByHash(context.Background(), meta.HashToken(plaintext))
	if err != nil || t.Revoked {
		return Identity{}, false
	}
	if t.Expires != 0 && t.Expires < p.now().Unix() {
		return Identity{}, false
	}
	if user != "" && user != t.User {
		return Identity{}, false
	}
	return Identity{User: t.User, Name: t.User, Scope: t.Scope}, true
}

// IdentifyKey delegates SSH keys to the inner policy — tokens are an HTTP
// credential; the key seam stays where it was.
func (p *TokenPolicy) IdentifyKey(user string, keyBlob []byte) Identity {
	return p.Inner.IdentifyKey(user, keyBlob)
}

// VisibleRefs delegates: branch visibility is the inner policy's rule set
// (personal namespaces, snapshot roles). Scope never widens visibility.
func (p *TokenPolicy) VisibleRefs(id Identity, repo string, refs []Ref) []Ref {
	return p.Inner.VisibleRefs(id, repo, refs)
}

// CanFetch: every valid scope reaches here; the inner policy still decides
// repo/directory access.
func (p *TokenPolicy) CanFetch(id Identity, repo, sub string) bool {
	return p.Inner.CanFetch(id, repo, sub)
}

// CanPush: the token ceiling is write — a read-scoped token can never push
// even where the inner policy would allow it.
func (p *TokenPolicy) CanPush(id Identity, repo, sub, branch string) bool {
	if id.Scope != "" && meta.ScopeRank(id.Scope) < meta.ScopeRank(meta.ScopeWrite) {
		return false
	}
	return p.Inner.CanPush(id, repo, sub, branch)
}

// CanAdmin: an admin-scoped token, or the inner policy's own admins.
func (p *TokenPolicy) CanAdmin(id Identity, repo string) bool {
	if id.Scope != "" {
		return meta.ScopeRank(id.Scope) >= meta.ScopeRank(meta.ScopeAdmin)
	}
	if ac, ok := p.Inner.(AdminChecker); ok {
		return ac.CanAdmin(id, repo)
	}
	return false
}
