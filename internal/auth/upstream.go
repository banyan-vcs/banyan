package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The console control plane (docs/17) fronts the gateway and delegates the
// deep enforcement to it, through two artifacts:
//
//   - a signed identity header (X-Banyan-Identity): the console verifies the
//     client's PAT, then vouches for the username with an HMAC the gateway
//     can check offline. Credentials and the user table never leave the
//     console; the gateway never sees a password or token hash.
//   - a policy snapshot v2 synced from the console's /internal/authz/policy/v2
//     endpoint: usernames (personal-branch ownership), instance admins, and
//     per-repo anchors carrying direct memberships. The gateway derives
//     effective roles with the same max-over-prefix-chain formula the console
//     uses (docs/17 §4), so aggregation semantics live in one shape only.

// AnonymousUser is the pseudo-identity the console signs for unauthenticated
// access to public repositories: the gateway keeps a single enforcement path
// (identity required), while public read access stays expressible.
const AnonymousUser = "anonymous"

// SignIdentity produces the X-Banyan-Identity header value vouching for user
// at the given time.
func SignIdentity(secret []byte, user string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s.%s", user, ts)
	return user + "." + ts + "." + hex.EncodeToString(mac.Sum(nil))
}

// ParseIdentity verifies a header value produced by SignIdentity. It fails
// closed on any malformation, replay outside the validity window, or a
// signature mismatch.
func ParseIdentity(secret []byte, v string, now time.Time) (string, bool) {
	i := strings.LastIndex(v, ".")
	if i < 0 {
		return "", false
	}
	sigHex := v[i+1:]
	rest := v[:i]
	j := strings.LastIndex(rest, ".")
	if j < 0 {
		return "", false
	}
	user, tsStr := rest[:j], rest[j+1:]
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || ts > now.Add(identitySlack).Unix() || ts < now.Add(-identitySlack).Unix() {
		return "", false
	}
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s.%s", user, tsStr)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil || len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
		return "", false
	}
	return user, true
}

// identitySlack is the acceptance window for signed identities (clock skew
// between console and gateway).
const identitySlack = 30 * time.Second

// --- snapshot v2 ---

// UpstreamSnapshot is the console-sourced policy image the gateway enforces.
type UpstreamSnapshot struct {
	Version int                     `json:"version"`
	Admins  []string                `json:"admins"`
	Users   []string                `json:"users"` // all usernames (personal-branch ownership)
	Repos   map[string]UpstreamRepo `json:"repos"` // repo_uid → policy
}

// UpstreamRepo is one repository's policy surface.
type UpstreamRepo struct {
	Public            bool                      `json:"public"`
	ProtectedBranches []string                  `json:"protected_branches"`
	Anchors           map[string]UpstreamAnchor `json:"anchors"` // "" = repo root
}

// UpstreamAnchor carries the direct memberships at one anchor; effective
// roles are derived by walking prefix anchors.
type UpstreamAnchor struct {
	Members map[string]string `json:"members"` // username → direct role
}

// RoleRank orders roles for the max aggregation (mirror of console.Role).
func RoleRank(role string) int {
	switch role {
	case "reporter":
		return 1
	case "developer":
		return 2
	case "maintainer":
		return 3
	case "owner":
		return 4
	}
	return 0
}

// anchorPrefix reports whether anchor a ("" = root) is a path prefix of sub.
func anchorPrefix(a, sub string) bool {
	if a == "" {
		return true
	}
	as, subs := strings.Split(a, "/"), strings.Split(sub, "/")
	if len(as) > len(subs) {
		return false
	}
	for i := range as {
		if as[i] != subs[i] {
			return false
		}
	}
	return true
}

// --- UpstreamPolicy ---

// UpstreamPolicy is the gateway-side auth.Service for console-fronted
// deployments: identity arrives as the signed header, rules as snapshot v2.
// Behavior mirrors RemotePolicy (docs/07 §4): last snapshot stays in effect
// on sync failure; no snapshot yet = fail closed. A repo absent from the
// snapshot triggers one out-of-band refresh (bounded to once per second) so
// a just-created project is usable without waiting out the TTL.
type UpstreamPolicy struct {
	ConsoleURL string
	Secret     []byte
	HTTP       *http.Client

	mu       sync.Mutex
	snap     *UpstreamSnapshot
	at       time.Time
	lastMiss time.Time
}

const upstreamSyncTTL = 10 * time.Second

// Refresh invalidates the cached snapshot: the next decision re-syncs from
// the console regardless of the TTL. The console kicks it after
// policy-affecting mutations so a grant lands in ~zero latency instead of
// one TTL later.
func (p *UpstreamPolicy) Refresh() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.at = time.Time{}
}

func NewUpstreamPolicy(consoleURL string, secret []byte) *UpstreamPolicy {
	return &UpstreamPolicy{ConsoleURL: consoleURL, Secret: secret,
		HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// IdentityHeaderName is the header SignIdentity/ParseIdentity exchange.
const IdentityHeaderName = "X-Banyan-Identity"

func (p *UpstreamPolicy) Identify(h *http.Request) Identity {
	v := h.Header.Get(IdentityHeaderName)
	if v == "" {
		return Identity{}
	}
	user, ok := ParseIdentity(p.Secret, v, time.Now())
	if !ok {
		return Identity{}
	}
	return Identity{User: user, Name: user}
}

// IdentifyKey stays closed in this topology: SSH terminates at the console
// (docs/17 §5, M6.3); key material never reaches the gateway.
func (p *UpstreamPolicy) IdentifyKey(string, []byte) Identity { return Identity{} }

// snapshot returns the current policy image, refreshing when stale; force
// requests a refresh (rate-limited).
func (p *UpstreamPolicy) snapshot(force bool) *UpstreamSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !force && p.snap != nil && time.Since(p.at) < upstreamSyncTTL {
		return p.snap
	}
	if force && p.snap != nil && time.Since(p.at) < time.Second {
		return p.snap // miss-triggered refreshes are bounded
	}
	resp, err := p.HTTP.Get(p.ConsoleURL + "/internal/authz/policy/v2")
	if err != nil {
		return p.snap // keep last known; nil = fail closed
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p.snap
	}
	var snap UpstreamSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return p.snap
	}
	p.snap, p.at = &snap, time.Now()
	return p.snap
}

// repo returns the repo policy, force-refreshing once when unknown (new
// project race: console created it moments ago).
func (p *UpstreamPolicy) repo(repo string) (UpstreamRepo, bool) {
	snap := p.snapshot(false)
	if snap == nil {
		return UpstreamRepo{}, false
	}
	if r, ok := snap.Repos[repo]; ok {
		return r, true
	}
	if snap = p.snapshot(true); snap != nil {
		if r, ok := snap.Repos[repo]; ok {
			return r, true
		}
	}
	return UpstreamRepo{}, false
}

func (p *UpstreamPolicy) isAdmin(id Identity) bool {
	snap := p.snapshot(false)
	if snap == nil {
		return false
	}
	for _, a := range snap.Admins {
		if a == id.User {
			return true
		}
	}
	return false
}

// effRole aggregates the direct memberships over the anchor prefix chain of
// sub — the same max walk the console performs (docs/17 §4).
func (p *UpstreamPolicy) effRole(id Identity, r UpstreamRepo, sub string) int {
	if p.isAdmin(id) {
		return RoleRank("owner")
	}
	best := 0
	for anchor, a := range r.Anchors {
		if anchorPrefix(anchor, sub) {
			if rank := RoleRank(a.Members[id.User]); rank > best {
				best = rank
			}
		}
	}
	return best
}

func (p *UpstreamPolicy) VisibleRefs(id Identity, _ string, refs []Ref) []Ref {
	snap := p.snapshot(false)
	if snap == nil {
		return nil
	}
	known := func(u string) bool {
		for _, n := range snap.Users {
			if n == u {
				return true
			}
		}
		return false
	}
	out := make([]Ref, 0, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref.Name, "refs/heads/") {
			branch := strings.TrimPrefix(ref.Name, "refs/heads/")
			if owner, _, ok := strings.Cut(branch, "/"); ok && known(owner) {
				if owner != id.User && !p.isAdmin(id) {
					continue
				}
			}
		}
		out = append(out, ref)
	}
	return out
}

// wireSub normalizes the subtree argument gitwire passes (an anchor path
// like "/" or "/services/auth", docs/02 §2) into the snapshot's anchor-key
// shape ("" or "services/auth").
func wireSub(sub string) string {
	return strings.TrimPrefix(sub, "/")
}

func (p *UpstreamPolicy) CanFetch(id Identity, repo, sub string) bool {
	r, ok := p.repo(repo)
	if !ok {
		return false
	}
	if r.Public {
		return true // includes the console-signed anonymous reader
	}
	if id.Anonymous() {
		return false
	}
	return p.effRole(id, r, wireSub(sub)) >= RoleRank("reporter")
}

func (p *UpstreamPolicy) CanPush(id Identity, repo, sub, branch string) bool {
	r, ok := p.repo(repo)
	if !ok || id.Anonymous() {
		return false
	}
	sub = wireSub(sub)
	rank := p.effRole(id, r, sub)
	if rank == 0 {
		return false
	}
	// Personal branches are exclusive to their owner and admins (docs/07
	// §3): rank never buys someone else's namespace.
	if owner, _, ok := strings.Cut(branch, "/"); ok && p.knownUser(owner) {
		return owner == id.User || p.isAdmin(id)
	}
	// Protected branches guard the project's integration branch at the ROOT
	// anchor. Subtree timelines have their own collaboration semantics
	// (convergence queues, docs/15); pushing a subtree's main is ordinary
	// anchor-granularity work, not an integration-branch move.
	if sub == "" {
		for _, b := range r.ProtectedBranches {
			if b == branch {
				return rank >= RoleRank("owner")
			}
		}
	}
	return rank >= RoleRank("developer")
}

// knownUser checks the snapshot's username list (personal-branch ownership).
func (p *UpstreamPolicy) knownUser(u string) bool {
	snap := p.snapshot(false)
	if snap == nil {
		return false
	}
	for _, n := range snap.Users {
		if n == u {
			return true
		}
	}
	return false
}
