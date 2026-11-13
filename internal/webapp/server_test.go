package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStack boots a web tier against a fake gateway and a temp dist.
func newTestStack(t *testing.T, gateway http.HandlerFunc) (*httptest.Server, *DB) {
	t.Helper()
	gw := httptest.NewServer(gateway)
	t.Cleanup(gw.Close)
	db, err := OpenDB("sqlite:" + filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dist := t.TempDir()
	writeFile(t, filepath.Join(dist, "index.html"), "<!doctype html><title>banyan</title>")
	writeFile(t, filepath.Join(dist, "assets", "app.js"), "console.log(1)")
	srv := New(Options{Gateway: gw.URL, DistDir: dist, DB: db, Client: gw.Client()})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs, db
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// noRedirect performs a request WITHOUT following redirects (OAuth flows).
func noRedirect(method, url, cookie string) (*http.Response, error) {
	req, _ := http.NewRequest(method, url, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return client.Do(req)
}

// postJSONCookie posts with a session cookie and no redirect following.
func postJSONCookie(t *testing.T, url, cookie string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLoginSessionProxyLogout(t *testing.T) {
	var seenAuth string
	stack, _ := newTestStack(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			if r.Header.Get("Authorization") != "" {
				t.Error("login check must not require credentials")
			}
			w.WriteHeader(200)
		case "/api/v1/whoami":
			seenAuth = r.Header.Get("Authorization")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected gateway call %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})

	// Not logged in: the proxy answers 401.
	resp, err := http.Get(stack.URL + "/api/v1/whoami")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous proxy = %d", resp.StatusCode)
	}

	// Local login: the gateway checks the password, the session rides a cookie.
	resp = postJSON(t, stack.URL+"/api/login", map[string]string{"user": "alice", "password": "demo"})
	cookie := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != 200 || !strings.Contains(cookie, "banyan-sid=") || !strings.Contains(cookie, "HttpOnly") {
		t.Fatalf("login = %d cookie=%q", resp.StatusCode, cookie)
	}
	resp.Body.Close()

	req, _ := http.NewRequest("GET", stack.URL+"/api/v1/whoami", nil)
	req.Header.Set("Cookie", cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxied call = %d", resp.StatusCode)
	}
	if want := "Basic " + base64Of("alice:demo"); seenAuth != want {
		t.Fatalf("gateway saw %q, want %q", seenAuth, want)
	}

	// /api/session answers who is logged in.
	req, _ = http.NewRequest("GET", stack.URL+"/api/session", nil)
	req.Header.Set("Cookie", cookie)
	resp, _ = http.DefaultClient.Do(req)
	var sess struct {
		User string `json:"user"`
		SSO  bool   `json:"sso"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()
	if sess.User != "alice" || sess.SSO {
		t.Fatalf("session = %+v", sess)
	}

	// Logout kills the row: the next proxied call is 401 again.
	req, _ = http.NewRequest("POST", stack.URL+"/api/logout", nil)
	req.Header.Set("Cookie", cookie)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", stack.URL+"/api/v1/whoami", nil)
	req.Header.Set("Cookie", cookie)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout proxy = %d", resp.StatusCode)
	}
}

func TestSessionsPersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.db")
	db, err := OpenDB("sqlite:" + path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(Session{ID: "sid-1", Username: "alice", Credential: "demo"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// A "restart": a fresh handle over the same file still knows the session.
	db2, err := OpenDB("sqlite:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	sess, ok := db2.SessionByID("sid-1")
	if !ok || sess.Username != "alice" {
		t.Fatalf("session after restart = %+v ok=%v", sess, ok)
	}
	// Expired rows sweep on sight.
	if err := db2.CreateSession(Session{ID: "sid-2", Username: "bob", Credential: "x"}, -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok := db2.SessionByID("sid-2"); ok {
		t.Fatal("expired session must not answer")
	}
}

func TestStaticSPA(t *testing.T) {
	stack, _ := newTestStack(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	for _, path := range []string{"/", "/r/demo/tree", "/settings/tokens"} {
		resp, err := http.Get(stack.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body[:n]), "banyan") {
			t.Fatalf("GET %s = %d %q", path, resp.StatusCode, body[:n])
		}
		if resp.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("GET %s cache = %q", path, resp.Header.Get("Cache-Control"))
		}
	}
	resp, err := http.Get(stack.URL + "/assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset cache = %q", cc)
	}
}

func TestModeAdvertisesProvidersAndCapabilities(t *testing.T) {
	stack, _ := newTestStack(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	resp, err := http.Get(stack.URL + "/api/mode")
	if err != nil {
		t.Fatal(err)
	}
	var mode struct {
		Profile      string         `json:"profile"`
		SSO          bool           `json:"sso"`
		Providers    []map[string]any `json:"providers"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&mode)
	resp.Body.Close()
	if mode.Profile != ProfileStandalone || mode.SSO || len(mode.Providers) != 0 {
		t.Fatalf("bare mode = %+v", mode)
	}
	if !mode.Capabilities["import"] || !mode.Capabilities["code"] {
		t.Fatalf("standalone capabilities = %+v", mode.Capabilities)
	}
}

func TestModeGitLabProfileNarrows(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	t.Cleanup(gw.Close)
	db, err := OpenDB("sqlite:" + filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := New(Options{Gateway: gw.URL, DistDir: t.TempDir(), Profile: ProfileGitLab, DB: db})
	stack := httptest.NewServer(srv)
	t.Cleanup(stack.Close)
	resp, _ := http.Get(stack.URL + "/api/mode")
	var mode struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&mode)
	resp.Body.Close()
	if !mode.Capabilities["subtrees"] {
		t.Fatal("gitlab profile keeps the subtree features")
	}
	for _, cap := range []string{"code", "history", "branches", "settings", "tokens", "import", "createRepos"} {
		if mode.Capabilities[cap] {
			t.Fatalf("gitlab profile must not expose %s", cap)
		}
	}
}

func TestOAuthRoundTripAgainstFakeGitLab(t *testing.T) {
	// The fake GitLab: authorize (never hit — we parse the redirect), token
	// exchange, and /api/v4/user.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"gl-token-1"}`))
		case "/api/v4/user":
			if r.Header.Get("Authorization") != "Bearer gl-token-1" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"username":"alice","name":"Alice"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(fake.Close)

	t.Setenv("BANYAN_OAUTH_PROVIDERS", fmt.Sprintf(
		`[{"id":"gitlab","kind":"gitlab","url":%q,"client_id":"cid","client_secret":"sec"}]`, fake.URL))

	stack, _ := newTestStack(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })

	// Start: a redirect to the provider carrying a state we can replay.
	resp, err := noRedirect(http.MethodGet, stack.URL+"/api/auth/gitlab", "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("auth start = %d", resp.StatusCode)
	}
	target := resp.Header.Get("Location")
	if !strings.HasPrefix(target, fake.URL+"/oauth/authorize") {
		t.Fatalf("authorize redirect = %q", target)
	}
	state := target[strings.Index(target, "state=")+len("state="):]
	if !strings.Contains(target, "scope=read_api+read_repository") {
		t.Fatalf("gitlab scope missing: %q", target)
	}

	// Callback: token exchange + user identity + session cookie.
	resp, err = noRedirect(http.MethodGet, stack.URL+"/auth/callback?code=c1&state="+state, "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/") {
		t.Fatalf("callback = %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookie := resp.Header.Get("Set-Cookie")

	// The session now carries the GitLab token in git's oauth2 convention.
	req, _ := http.NewRequest("GET", stack.URL+"/api/session", nil)
	req.Header.Set("Cookie", cookie)
	resp, _ = http.DefaultClient.Do(req)
	var sess struct {
		User     string `json:"user"`
		Provider struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"provider"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()
	if sess.User != "alice" || sess.Provider.ID != "gitlab" || sess.Provider.Kind != "gitlab" {
		t.Fatalf("provider session = %+v", sess)
	}
}

func TestImportOrchestration(t *testing.T) {
	done := make(chan struct{})
	stack, _ := newTestStack(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.WriteHeader(200)
		case "/api/v1/imports":
			if r.Header.Get("Authorization") != "Basic "+base64Of("alice:demo") {
				w.WriteHeader(403)
				return
			}
			var call importCall
			_ = json.NewDecoder(r.Body).Decode(&call)
			<-done // the SPA polls meanwhile; make the call observably long
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repo":"` + call.Name + `","refs":3,"defaultBranch":"trunk"}`))
		default:
			w.WriteHeader(404)
		}
	})
	login := postJSON(t, stack.URL+"/api/login", map[string]string{"user": "alice", "password": "demo"})
	cookie := login.Header.Get("Set-Cookie")
	login.Body.Close()

	// A local login must not leak its password to a source host.
	resp := postJSONCookie(t, stack.URL+"/api/imports", cookie, map[string]any{"url": "https://gitlab.com/grp/repo.git"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("session-mode import on a local login = %d, want 400", resp.StatusCode)
	}

	// Token mode: the job is accepted immediately, then driven to success.
	resp = postJSONCookie(t, stack.URL+"/api/imports", cookie, map[string]any{
		"url": "https://gitlab.com/grp/repo.git", "name": "repo",
		"auth": map[string]string{"mode": "token", "token": "glpat-1"}})
	var created struct{ ID string `json:"id"` }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || created.ID == "" {
		t.Fatalf("import create = %d %+v", resp.StatusCode, created)
	}
	close(done)

	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest("GET", stack.URL+"/api/imports", nil)
		req.Header.Set("Cookie", cookie)
		list, _ := http.DefaultClient.Do(req)
		var out struct {
			Imports []ImportJob `json:"imports"`
		}
		_ = json.NewDecoder(list.Body).Decode(&out)
		list.Body.Close()
		if len(out.Imports) == 1 && out.Imports[0].Status == "succeeded" {
			if out.Imports[0].Refs != 3 || out.Imports[0].DefaultBranch != "trunk" {
				t.Fatalf("finished job = %+v", out.Imports[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never succeeded: %+v", out.Imports)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestInterruptedImportsSweep(t *testing.T) {
	db, err := OpenDB("sqlite:" + filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateImport(ImportJob{ID: "j1", Repo: "r", SourceURL: "http://x", Initiator: "alice", Status: "running", StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.FailInterruptedImports(); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.ListImports("alice")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v err=%v", jobs, err)
	}
	if jobs[0].Status != "failed" || !strings.Contains(jobs[0].Error, "interrupted") {
		t.Fatalf("swept job = %+v", jobs[0])
	}
}
