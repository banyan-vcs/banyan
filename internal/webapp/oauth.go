// oauth.go — provider-backed login. Configuration sources, merged and
// de-duplicated by provider id (earlier wins): the BANYAN_OAUTH_PROVIDERS
// env JSON, the OAUTH_PROVIDERS_FILE ({"providers":[…]}, default
// /config/oauth-providers.json), and the two legacy single-GitLab shapes —
// the BANYAN_GITLAB_* env triple and the OAUTH_CONFIG_FILE the GitLab demo
// provisioning writes (/config/gitlab-oauth.json).
//
// Flows are the standard authorization-code + state round trip. GitLab
// asks read_api + read_repository (console display + repo import); GitHub
// asks repo (private-repo import needs it; the consent screen says so).
// The state is DB-backed, so any instance of a scaled web tier answers the
// callback.
package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Provider is one configured OAuth issuer.
type Provider struct {
	ID           string
	Kind         string // gitlab | github
	URL          string // what THIS process dials (gitlab: instance; github: "" = github.com, else GHE)
	ExternalURL  string // what the BROWSER reaches for the authorize redirect
	ClientID     string
	ClientSecret string
}

// Label is the login-page name.
func (p Provider) Label() string {
	if p.Kind == "github" {
		host := "github.com"
		if p.URL != "" {
			host = strings.TrimPrefix(strings.TrimPrefix(p.URL, "https://"), "http://")
		}
		return "GitHub · " + host
	}
	host := strings.TrimPrefix(strings.TrimPrefix(p.ExternalURL, "https://"), "http://")
	return "GitLab · " + host
}

func (p Provider) externalBase() string {
	if p.Kind == "github" && p.ExternalURL == "" {
		if p.URL == "" {
			return "https://github.com"
		}
		return p.URL
	}
	return p.ExternalURL
}

func (p Provider) authorizeURL() string {
	if p.Kind == "github" {
		return p.externalBase() + "/login/oauth/authorize"
	}
	return p.externalBase() + "/oauth/authorize"
}

func (p Provider) tokenURL() string {
	if p.Kind == "github" {
		return p.externalBase() + "/login/oauth/access_token"
	}
	return strings.TrimRight(p.URL, "/") + "/oauth/token"
}

func (p Provider) userURL() string {
	if p.Kind == "github" {
		if p.URL == "" || p.URL == "https://github.com" {
			return "https://api.github.com/user"
		}
		return strings.TrimRight(p.URL, "/") + "/api/v3/user"
	}
	return strings.TrimRight(p.URL, "/") + "/api/v4/user"
}

func (p Provider) scope() string {
	if p.Kind == "github" {
		return "repo"
	}
	return "read_api read_repository"
}

// parseProvidersJSON accepts both shapes the wild uses: a bare array and
// the {"providers":[…]} object the file format specifies.
func parseProvidersJSON(raw string) []Provider {
	var list []struct {
		ID           string `json:"id"`
		Kind         string `json:"kind"`
		URL          string `json:"url"`
		ExternalURL  string `json:"external_url"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		var obj struct {
			Providers []struct {
				ID           string `json:"id"`
				Kind         string `json:"kind"`
				URL          string `json:"url"`
				ExternalURL  string `json:"external_url"`
				ClientID     string `json:"client_id"`
				ClientSecret string `json:"client_secret"`
			} `json:"providers"`
		}
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			return nil
		}
		list = obj.Providers
	}
	out := make([]Provider, 0, len(list))
	for _, p := range list {
		out = append(out, Provider{p.ID, p.Kind, p.URL, p.ExternalURL, p.ClientID, p.ClientSecret})
	}
	return out
}

// loadProviders merges every configuration source; invalid entries are
// skipped (a half-configured provider must never appear on the login page).
func loadProviders() []Provider {
	var ordered []Provider
	seen := map[string]bool{}
	add := func(p Provider) {
		if p.ID == "" || p.ClientID == "" || p.ClientSecret == "" {
			return
		}
		if p.Kind != "gitlab" && p.Kind != "github" {
			return
		}
		if p.Kind == "gitlab" && p.URL == "" {
			return
		}
		if seen[p.ID] {
			return
		}
		seen[p.ID] = true
		if p.ExternalURL == "" {
			p.ExternalURL = p.URL
		}
		p.URL = strings.TrimRight(p.URL, "/")
		p.ExternalURL = strings.TrimRight(p.ExternalURL, "/")
		ordered = append(ordered, p)
	}

	if raw := os.Getenv("BANYAN_OAUTH_PROVIDERS"); raw != "" {
		for _, p := range parseProvidersJSON(raw) {
			add(p)
		}
	}
	if raw, err := os.ReadFile(envOr("OAUTH_PROVIDERS_FILE", "/config/oauth-providers.json")); err == nil {
		for _, p := range parseProvidersJSON(string(raw)) {
			add(p)
		}
	}
	// Legacy single GitLab (env triple, then the demo provisioning file).
	glURL := os.Getenv("BANYAN_GITLAB_URL")
	glID := os.Getenv("BANYAN_GITLAB_CLIENT_ID")
	glSec := os.Getenv("BANYAN_GITLAB_CLIENT_SECRET")
	glExt := envOr("BANYAN_GITLAB_EXTERNAL_URL", glURL)
	if glURL != "" && glID != "" && glSec != "" {
		add(Provider{ID: "gitlab", Kind: "gitlab", URL: glURL, ExternalURL: glExt, ClientID: glID, ClientSecret: glSec})
	}
	if raw, err := os.ReadFile(envOr("OAUTH_CONFIG_FILE", "/config/gitlab-oauth.json")); err == nil {
		var f struct {
			URL          string `json:"url"`
			ExternalURL  string `json:"external_url"`
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if json.Unmarshal(raw, &f) == nil {
			ext := f.ExternalURL
			if ext == "" {
				ext = f.URL
			}
			add(Provider{ID: "gitlab", Kind: "gitlab", URL: f.URL, ExternalURL: ext, ClientID: f.ClientID, ClientSecret: f.ClientSecret})
		}
	}
	return ordered
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (s *Server) providerByID(id string) (Provider, bool) {
	for _, p := range s.providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// redirectURI derives the OAuth callback URL from the request's headers —
// behind GitLab nginx this needs the original Host ($http_host) and proto.
func (s *Server) redirectURI(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return proto + "://" + host + s.opts.BasePath + "/auth/callback"
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := s.providerByID(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown login provider"})
		return
	}
	state := randomHex(16)
	if state == "" || s.opts.DB.PutOAuthState(state, p.ID, 10*time.Minute) != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start the login flow"})
		return
	}
	q := url.Values{
		"client_id":     {p.ClientID},
		"redirect_uri":  {s.redirectURI(r)},
		"response_type": {"code"},
		"scope":         {p.scope()},
		"state":         {state},
	}
	http.Redirect(w, r, p.authorizeURL()+"?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		http.Redirect(w, r, s.opts.BasePath+"/login?error="+url.QueryEscape(msg), http.StatusFound)
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		fail("login callback rejected (missing code or state) — try again")
		return
	}
	providerID := s.opts.DB.TakeOAuthState(state)
	p, ok := s.providerByID(providerID)
	if !ok {
		fail("login callback rejected (bad or expired state) — try again")
		return
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"code":          {code},
		"redirect_uri":  {s.redirectURI(r)},
	}
	req, err := http.NewRequest(http.MethodPost, p.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		fail(err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.Kind == "github" {
		req.Header.Set("Accept", "application/json") // GitHub defaults otherwise
	}
	resp, err := s.client.Do(req)
	if err != nil {
		fail("login provider unreachable: " + err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Sprintf("token exchange failed (%s) — try again", resp.Status))
		return
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		fail("token exchange returned no token — try again")
		return
	}

	// Who is this? The provider's own /user endpoint.
	whoReq, _ := http.NewRequest(http.MethodGet, p.userURL(), nil)
	whoReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	whoReq.Header.Set("Accept", "application/json")
	who, err := s.client.Do(whoReq)
	if err != nil {
		fail("login provider unreachable: " + err.Error())
		return
	}
	defer who.Body.Close()
	if who.StatusCode != http.StatusOK {
		fail("could not identify the login user — try again")
		return
	}
	var u struct {
		Username string `json:"username"` // GitLab
		Login    string `json:"login"`    // GitHub
	}
	if err := json.NewDecoder(who.Body).Decode(&u); err != nil {
		fail("could not identify the login user — try again")
		return
	}
	name := u.Username
	if p.Kind == "github" {
		name = u.Login
	}
	if name == "" {
		fail("the login user has no username?")
		return
	}
	if err := s.startSession(w, Session{
		Username: name, Credential: tok.AccessToken,
		ProviderID: p.ID, ProviderKind: p.Kind,
	}); err != nil {
		fail("could not open a session: " + err.Error())
		return
	}
	http.Redirect(w, r, s.opts.BasePath+"/", http.StatusFound)
}

// --- GitLab project names (display only) ---

// gitlabNames maps sha256(project_id) → path_with_namespace for repos the
// session's token can see (read_api), TTL-cached per token. GitHub needs
// no such mapping — repo ids are their names.
var nameCache sync.Map // token hash → cachedNames

type cachedNames struct {
	names   map[string]string
	expires time.Time
}

func (s *Server) handleGitLabNames(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
		return
	}
	if sess.ProviderKind != "gitlab" {
		writeJSON(w, http.StatusOK, map[string]any{"names": map[string]string{}})
		return
	}
	p, ok := s.providerByID(sess.ProviderID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"names": map[string]string{}})
		return
	}
	key := hashOf(sess.Credential)
	if hit, ok := nameCache.Load(key); ok {
		if c := hit.(cachedNames); time.Now().Before(c.expires) {
			writeJSON(w, http.StatusOK, map[string]any{"names": c.names})
			return
		}
	}
	names := map[string]string{}
	req, _ := http.NewRequest(http.MethodGet,
		strings.TrimRight(p.URL, "/")+"/api/v4/projects?membership=true&simple=true&per_page=100", nil)
	req.Header.Set("Authorization", "Bearer "+sess.Credential)
	if resp, err := s.client.Do(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var projects []struct {
				ID                int    `json:"id"`
				PathWithNamespace string `json:"path_with_namespace"`
			}
			if json.NewDecoder(resp.Body).Decode(&projects) == nil {
				for _, pr := range projects {
					names[hashOf(fmt.Sprint(pr.ID))] = pr.PathWithNamespace
				}
			}
		}
	}
	nameCache.Store(key, cachedNames{names: names, expires: time.Now().Add(time.Minute)})
	writeJSON(w, http.StatusOK, map[string]any{"names": names})
}
