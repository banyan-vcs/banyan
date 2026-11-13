// Multi-provider token authentication (docs/07 §8d): the same posture as
// GitLabSSO — a token minted by an identity provider gates banyan's HTTP
// face — generalised to several issuers at once. A deployment lists the
// providers it trusts (GitLab instances, github.com or a GitHub Enterprise
// host); a presented token is validated against them in order, first match
// wins. The credential shape is unchanged: HTTP Basic with the token as the
// password (git's oauth2:<token> / x-access-token:<token> conventions) or
// an Authorization: Bearer header.
//
// Validation is one call per provider: GitLab's GET <url>/api/v4/user,
// GitHub's GET <api>/user. Answers cache briefly by (provider, token hash),
// negatives included — a GitLab token costs one wasted GitHub probe per TTL,
// never one per request. Every failure mode is ANONYMOUS, fail-closed.
//
// Admins: GitLab instance admins (is_admin) plus the names each provider
// config lists plus the deployment-wide auth.admins list — GitHub's user
// API carries no admin bit, so the config list is the only source there.
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

// Provider kinds.
const (
	SSOKindGitLab = "gitlab"
	SSOKindGitHub = "github"
)

// SSOProvider is one trusted token issuer.
type SSOProvider struct {
	// ID names the provider in configuration and logs.
	ID string
	// Kind selects the validation dialect: SSOKindGitLab or SSOKindGitHub.
	Kind string
	// URL is the instance base URL. Required for GitLab; empty on GitHub
	// means github.com (API at api.github.com), a GitHub Enterprise host
	// answers at <url>/api/v3.
	URL string
	// Admins are usernames that hold CanAdmin when validated here.
	Admins []string
}

// userURL is the provider's "who am I" endpoint.
func (p SSOProvider) userURL() string {
	base := strings.TrimRight(p.URL, "/")
	if p.Kind == SSOKindGitHub {
		if base == "" || base == "https://github.com" {
			return "https://api.github.com/user"
		}
		return base + "/api/v3/user"
	}
	return base + "/api/v4/user"
}

// MultiSSO authenticates tokens against several providers. All methods are
// safe for concurrent use.
type MultiSSO struct {
	Providers []SSOProvider
	// Admins holds deployment-wide admin usernames (auth.admins).
	Admins []string
	// Client dials the providers; default is a client with a 10s timeout.
	Client *http.Client
	// CacheTTL bounds how long a validation holds (default 60s).
	CacheTTL time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time

	mu     sync.Mutex
	cache  map[string]ssoEntry  // "<provider>/<sha256(token)>" → validation
	admins map[string]time.Time // username → admin until
}

func (p *MultiSSO) ttl() time.Duration {
	if p.CacheTTL > 0 {
		return p.CacheTTL
	}
	return time.Minute
}

func (p *MultiSSO) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *MultiSSO) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Identify: a presented token validates against the providers in order;
// anything less is anonymous.
func (p *MultiSSO) Identify(h *http.Request) Identity {
	token := tokenOf(h)
	if token == "" {
		return Identity{}
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	for _, prov := range p.Providers {
		if e := p.validate(prov, token, sum); e.ok {
			return e.id
		}
	}
	return Identity{}
}

func (p *MultiSSO) validate(prov SSOProvider, token, sum string) ssoEntry {
	key := prov.ID + "/" + sum
	p.mu.Lock()
	if e, ok := p.cache[key]; ok && p.now().Before(e.expires) {
		p.mu.Unlock()
		return e
	}
	p.mu.Unlock()

	e := p.call(prov, token)
	e.expires = p.now().Add(p.ttl())

	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]ssoEntry{}
	}
	p.cache[key] = e
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
	return e
}

// call performs one validation round-trip against one provider. Every
// failure mode — transport error, non-200, unparsable body — is a negative.
func (p *MultiSSO) call(prov SSOProvider, token string) ssoEntry {
	req, err := http.NewRequest(http.MethodGet, prov.userURL(), nil)
	if err != nil {
		return ssoEntry{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return ssoEntry{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ssoEntry{}
	}
	var user struct {
		Username string `json:"username"` // GitLab
		Login    string `json:"login"`    // GitHub
		Name     string `json:"name"`
		IsAdmin  bool   `json:"is_admin"` // GitLab only
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return ssoEntry{}
	}
	name := user.Username
	if prov.Kind == SSOKindGitHub {
		name = user.Login
	}
	if name == "" {
		return ssoEntry{}
	}
	admin := user.IsAdmin && prov.Kind == SSOKindGitLab
	for _, a := range prov.Admins {
		if a == name {
			admin = true
		}
	}
	for _, a := range p.Admins {
		if a == name {
			admin = true
		}
	}
	return ssoEntry{id: Identity{User: name, Name: user.Name}, admin: admin, ok: true}
}

// IdentifyKey: no local SSH keys in token-fronted topologies.
func (p *MultiSSO) IdentifyKey(string, []byte) Identity { return Identity{} }

// VisibleRefs: passthrough — every authenticated user sees every branch.
func (p *MultiSSO) VisibleRefs(_ Identity, _ string, refs []Ref) []Ref { return refs }

// CanFetch: any provider-validated identity.
func (p *MultiSSO) CanFetch(id Identity, _, _ string) bool { return !id.Anonymous() }

// CanPush: any provider-validated identity.
func (p *MultiSSO) CanPush(id Identity, _, _, _ string) bool { return !id.Anonymous() }

// CanAdmin (auth.AdminChecker): admins as seen at the last validation
// (within one TTL).
func (p *MultiSSO) CanAdmin(id Identity, _ string) bool {
	if id.Anonymous() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now().Before(p.admins[id.User])
}
