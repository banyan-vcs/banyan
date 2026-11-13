// Package webapp is banyan's web tier — `banyand web`: the browser-facing
// service that owns sessions (in its own database), offers local and OAuth
// login, proxies /api/v1 and the git ref advertisement to the gateway with
// the session's credentials injected, serves the SPA, and (standalone
// profile) is the single control plane over a gateway. In the gitlab
// profile it cooperates with a GitLab deployment: the console narrows to
// banyan's own subtree features and login rides GitLab's OAuth.
//
// The SPA never sees a credential: every proxied call carries the session's
// Authorization header server-side, the exact credential shape git itself
// uses against the gateway.
package webapp

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Profile names the deployment shape.
const (
	ProfileStandalone = "standalone" // full console, the single control plane
	ProfileGitLab     = "gitlab"     // subtree features only, GitLab SSO
)

// Capabilities is what /api/mode advertises; the SPA renders to this list.
type Capabilities struct {
	CreateRepos bool `json:"createRepos"`
	Code        bool `json:"code"`
	History     bool `json:"history"`
	Branches    bool `json:"branches"`
	Subtrees    bool `json:"subtrees"`
	Settings    bool `json:"settings"`
	Tokens      bool `json:"tokens"`
	Import      bool `json:"import"`
}

func capabilitiesFor(profile string) Capabilities {
	if profile == ProfileGitLab {
		// GitLab owns project management and code browsing in that mode;
		// banyan surfaces its own subtree features only.
		return Capabilities{Subtrees: true}
	}
	return Capabilities{
		CreateRepos: true, Code: true, History: true, Branches: true,
		Subtrees: true, Settings: true, Tokens: true, Import: true,
	}
}

// Options configures the web tier.
type Options struct {
	Gateway  string // gateway (banyand) base URL
	BasePath string // URL prefix without trailing slash (GitLab /-/banyan)
	DistDir  string // the SPA build directory
	Profile  string // ProfileStandalone | ProfileGitLab
	DB       *DB
	// SessionTTL bounds a login (default 12h).
	SessionTTL time.Duration
	// Client dials the gateway and providers (tests); default 30s.
	Client *http.Client
}

// Server is the web tier.
type Server struct {
	opts       Options
	providers  []Provider // sorted by config order; may be empty (local only)
	client     *http.Client
	sessionTTL time.Duration
	cookiePath string
	static     *staticServer
}

// New builds the web tier.
func New(opts Options) *Server {
	if opts.Profile == "" {
		opts.Profile = ProfileStandalone
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 12 * time.Hour
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	s := &Server{
		opts:       opts,
		client:     client,
		sessionTTL: opts.SessionTTL,
		cookiePath: opts.BasePath,
		providers:  loadProviders(),
	}
	if s.cookiePath == "" {
		s.cookiePath = "/"
	}
	s.static = newStaticServer(opts.DistDir)
	return s
}

// Register mounts the web tier on the mux (root pattern).
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("/", s)
}

// Handler answers every request (single dispatcher, BASE_PATH aware).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pathname := r.URL.Path
	if s.opts.BasePath != "" {
		if pathname == s.opts.BasePath {
			pathname = "/"
		} else if strings.HasPrefix(pathname, s.opts.BasePath+"/") {
			pathname = pathname[len(s.opts.BasePath):]
		} else {
			// Outside the mount: still the SPA (GitLab routes the whole
			// subpage tree here).
			s.static.serve(w, "index.html")
			return
		}
	}

	switch {
	case pathname == "/api/login" && r.Method == http.MethodPost:
		s.handleLogin(w, r)
	case pathname == "/api/logout" && r.Method == http.MethodPost:
		s.handleLogout(w, r)
	case pathname == "/api/mode" && r.Method == http.MethodGet:
		s.handleMode(w, r)
	case pathname == "/api/session" && r.Method == http.MethodGet:
		s.handleSession(w, r)
	case pathname == "/api/gitlab-names" && r.Method == http.MethodGet:
		s.handleGitLabNames(w, r)
	case strings.HasPrefix(pathname, "/api/auth/"):
		s.handleOAuthStart(w, r, strings.TrimPrefix(pathname, "/api/auth/"))
	case pathname == "/auth/callback":
		s.handleOAuthCallback(w, r)
	case pathname == "/api/imports" && r.Method == http.MethodPost:
		s.handleImportCreate(w, r)
	case pathname == "/api/imports" && r.Method == http.MethodGet:
		s.handleImportList(w, r)
	case shouldProxy(r, pathname):
		// /api/v1/* and the git ref advertisement: session-authenticated
		// gateway calls — the SPA's ref listing rides the same
		// advertisement git gets.
		s.proxy(w, r, pathname)
	case strings.HasPrefix(pathname, "/api/"):
		// Any other /api path is not ours.
		http.NotFound(w, r)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		s.static.serve(w, strings.TrimPrefix(pathname, "/"))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// shouldProxy: /api/v1/* (session-authenticated gateway calls) and the git
// ref advertisement the SPA parses.
func shouldProxy(r *http.Request, pathname string) bool {
	if strings.HasPrefix(pathname, "/api/v1/") || pathname == "/api/v1" {
		return true
	}
	return strings.HasPrefix(pathname, "/r/") && strings.HasSuffix(pathname, "/info/refs") && r.Method == http.MethodGet
}

// --- session lifecycle ---

const cookieName = "banyan-sid"

func (s *Server) sessionOf(r *http.Request) (Session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return Session{}, false
	}
	return s.opts.DB.SessionByID(c.Value)
}

func (s *Server) startSession(w http.ResponseWriter, sess Session) error {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	sess.ID = base64.RawURLEncoding.EncodeToString(raw)
	if err := s.opts.DB.CreateSession(sess, s.sessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: sess.ID, Path: s.cookiePath,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		_ = s.opts.DB.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: s.cookiePath,
		HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
		return
	}
	out := map[string]any{"user": sess.Username, "sso": sess.ProviderID != ""}
	if sess.ProviderID != "" {
		out["provider"] = map[string]string{"id": sess.ProviderID, "kind": sess.ProviderKind}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMode(w http.ResponseWriter, _ *http.Request) {
	prov := make([]map[string]string, 0, len(s.providers)) // never null
	for _, p := range s.providers {
		prov = append(prov, map[string]string{"id": p.ID, "kind": p.Kind, "label": p.Label()})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile":      s.opts.Profile,
		"sso":          len(s.providers) > 0,
		"providers":    prov,
		"capabilities": capabilitiesFor(s.opts.Profile),
	})
}

// --- local login: the gateway's own auth check stays the single source ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if !readJSONBody(w, r, &req) {
		return
	}
	payload, _ := json.Marshal(map[string]string{"user": req.User, "password": req.Password})
	gw, err := s.client.Post(s.opts.Gateway+"/api/v1/auth/login", "application/json", bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "banyan unreachable: " + err.Error()})
		return
	}
	defer gw.Body.Close()
	if gw.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong user or password"})
		return
	}
	sess := Session{Username: req.User, Credential: req.Password}
	if err := s.startSession(w, sess); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": req.User})
}

// --- helpers ---

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func cleanRepoName(name string) string {
	name = strings.TrimSuffix(strings.TrimRight(name, "/"), ".git")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}
