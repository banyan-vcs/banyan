// GitLab token authentication as a standalone auth.Service (docs/20 §"子树
// 管理控制面"): when banyan sits behind a GitLab instance, the credential
// set that already exists — GitLab OAuth2 access tokens and personal access
// tokens — authenticates banyan's own HTTP face too. The credential rides
// HTTP Basic (any username, the token as the password — git's
// oauth2:<token> convention) or an Authorization: Bearer header.
//
// Validation is one call, GET <url>/api/v4/user (works for both token
// kinds; requires read_api); answers cache briefly by token hash, including
// negatives. GitLab unreachable or rejecting the token is ANONYMOUS, never
// an error and never a pass — fail-closed, the same posture as the GitLab
// /allowed policy gate. Identity is the GitLab username; instance admins
// (is_admin) answer CanAdmin — they are the management plane for the runtime
// subtree rules (docs/07 §8).
//
// The SSH seam stays anonymous on purpose: in GitLab topology git-over-SSH
// terminates at gitlab-shell, which reaches banyan over gRPC with its own
// token; banyan's own SSH face is not part of that flow. Branch visibility
// is a passthrough — GitLab semantics show every branch to project members,
// the personal-namespace filter is the static policy's demo feature.
package auth

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GitLabSSO authenticates GitLab-issued tokens. All methods are safe for
// concurrent use.
type GitLabSSO struct {
	// URL is the GitLab base URL (e.g. "http://gitlab.example.com:8180").
	URL string
	// Client dials GitLab; default is a client with a 10s timeout.
	Client *http.Client
	// CacheTTL bounds how long a validation holds (default 60s) — revoking
	// a GitLab token takes effect here within one TTL.
	CacheTTL time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time

	mu      sync.Mutex
	byToken map[string]ssoEntry  // sha256(token) → validation
	admins  map[string]time.Time // username → admin until (drives CanAdmin)
}

type ssoEntry struct {
	id      Identity
	admin   bool
	ok      bool
	expires time.Time
}

// NewGitLabSSO builds the policy around a GitLab base URL.
func NewGitLabSSO(url string) *GitLabSSO {
	return &GitLabSSO{URL: strings.TrimRight(url, "/")}
}

func (p *GitLabSSO) ttl() time.Duration {
	if p.CacheTTL > 0 {
		return p.CacheTTL
	}
	return time.Minute
}

func (p *GitLabSSO) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *GitLabSSO) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// tokenOf extracts the GitLab token from the request: Bearer first, then
// HTTP Basic's password field (any username — git URLs embed
// oauth2:<token> or <user>:<token> alike).
func tokenOf(h *http.Request) string {
	if v := h.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimSpace(v[len("Bearer "):])
	}
	if _, pass, ok := basicAuth(h); ok {
		return pass
	}
	return ""
}

// Identify: a presented token validates against GitLab; anything less is
// anonymous.
func (p *GitLabSSO) Identify(h *http.Request) Identity {
	token := tokenOf(h)
	if token == "" {
		return Identity{}
	}
	return p.validate(token)
}

func (p *GitLabSSO) validate(token string) Identity {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))

	p.mu.Lock()
	if e, ok := p.byToken[key]; ok && p.now().Before(e.expires) {
		p.mu.Unlock()
		return e.id
	}
	p.mu.Unlock()

	e := p.callGitLab(token)
	e.expires = p.now().Add(p.ttl())

	p.mu.Lock()
	if p.byToken == nil {
		p.byToken = map[string]ssoEntry{}
	}
	p.byToken[key] = e
	if e.ok {
		if p.admins == nil {
			p.admins = map[string]time.Time{}
		}
		if e.admin {
			p.admins[e.id.User] = e.expires
		} else {
			delete(p.admins, e.id.User)
		}
	}
	p.mu.Unlock()
	return e.id
}

// callGitLab performs the one validation round-trip. Every failure mode —
// transport error, non-200, unparsable body — is a cached negative.
func (p *GitLabSSO) callGitLab(token string) ssoEntry {
	req, err := http.NewRequest(http.MethodGet, p.URL+"/api/v4/user", nil)
	if err != nil {
		return ssoEntry{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client().Do(req)
	if err != nil {
		return ssoEntry{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ssoEntry{}
	}
	var user struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil || user.Username == "" {
		return ssoEntry{}
	}
	return ssoEntry{
		id:    Identity{User: user.Username, Name: user.Name},
		admin: user.IsAdmin,
		ok:    true,
	}
}

// IdentifyKey: no local SSH keys in GitLab topology (gitlab-shell owns the
// SSH seam and reaches banyan over gRPC with its own token).
func (p *GitLabSSO) IdentifyKey(string, []byte) Identity { return Identity{} }

// VisibleRefs: passthrough — every authenticated user sees every branch
// (GitLab's own semantics; the personal-namespace filter is the static
// policy's demo feature, docs/07 §3).
func (p *GitLabSSO) VisibleRefs(_ Identity, _ string, refs []Ref) []Ref { return refs }

// CanFetch: any GitLab-validated identity.
func (p *GitLabSSO) CanFetch(id Identity, _, _ string) bool { return !id.Anonymous() }

// CanPush: any GitLab-validated identity — ref-level policy (protected
// branches) stays with GitLab's own URL; this face trusts the token.
func (p *GitLabSSO) CanPush(id Identity, _, _, _ string) bool { return !id.Anonymous() }

// CanAdmin (auth.AdminChecker): GitLab instance admins, as seen at the last
// validation (within one TTL). An identity that was never validated here —
// or whose admin flag has lapsed — is not an admin.
func (p *GitLabSSO) CanAdmin(id Identity, _ string) bool {
	if id.Anonymous() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now().Before(p.admins[id.User])
}
