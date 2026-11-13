package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitLab answers /api/v4/user for the tokens it knows.
func fakeGitLab(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer alice-token":
			json.NewEncoder(w).Encode(map[string]any{"username": "alice", "name": "Alice", "is_admin": false})
		case "Bearer root-token":
			json.NewEncoder(w).Encode(map[string]any{"username": "root", "name": "Root", "is_admin": true})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeGitHub answers the enterprise-shape /api/v3/user (and /user) for the
// tokens it knows — github.com itself would answer api.github.com/user.
func fakeGitHub(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	handler := func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer octo-token":
			json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "name": "The Octocat"})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/user", handler)
	mux.HandleFunc("/user", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestMultiSSOIdentifyAcrossProviders(t *testing.T) {
	ghHits, glHits := &atomic.Int32{}, &atomic.Int32{}
	gh := fakeGitHub(t, ghHits)
	gl := fakeGitLab(t, glHits)
	p := &MultiSSO{
		// GitHub first: a GitLab token costs one wasted probe per TTL,
		// then lands on the GitLab provider.
		Providers: []SSOProvider{
			{ID: "gh", Kind: SSOKindGitHub, URL: gh.URL},
			{ID: "gl", Kind: SSOKindGitLab, URL: gl.URL},
		},
		CacheTTL: 10 * time.Minute,
	}

	if id := p.Identify(bearerReq("octo-token")); id.User != "octocat" || id.Name != "The Octocat" {
		t.Fatalf("github identity = %+v", id)
	}
	id := p.Identify(bearerReq("alice-token"))
	if id.User != "alice" || id.Name != "Alice" {
		t.Fatalf("gitlab identity after github miss = %+v", id)
	}
	if ghHits.Load() != 2 {
		t.Fatalf("github probes = %d, want 2 (octo + wasted alice)", ghHits.Load())
	}

	// git's Basic convention rides too (any username, token as password).
	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("oauth2", "alice-token")
	if id := p.Identify(r); id.User != "alice" {
		t.Fatalf("basic identity = %+v", id)
	}

	// Wrong tokens and no credentials are anonymous, fail-closed.
	if id := p.Identify(bearerReq("nope")); !id.Anonymous() {
		t.Fatalf("bad token = %+v", id)
	}
	if id := p.Identify(httptest.NewRequest("GET", "/", nil)); !id.Anonymous() {
		t.Fatalf("no credentials = %+v", id)
	}

	// Caching: repeat Identifies cost nothing within the TTL (positives
	// and negatives alike).
	glBefore, ghBefore := glHits.Load(), ghHits.Load()
	p.Identify(bearerReq("alice-token"))
	p.Identify(bearerReq("octo-token"))
	p.Identify(bearerReq("nope"))
	if glHits.Load() != glBefore || ghHits.Load() != ghBefore {
		t.Fatalf("cache miss: gitlab %d→%d github %d→%d", glBefore, glHits.Load(), ghBefore, ghHits.Load())
	}
}

func TestMultiSSOAdmins(t *testing.T) {
	gl := fakeGitLab(t, nil)
	gh := fakeGitHub(t, nil)

	// GitLab is_admin drives CanAdmin without any list.
	p := &MultiSSO{Providers: []SSOProvider{{ID: "gl", Kind: SSOKindGitLab, URL: gl.URL}}, CacheTTL: 10 * time.Minute}
	if id := p.Identify(bearerReq("root-token")); !p.CanAdmin(id, "") {
		t.Fatal("gitlab is_admin ignored")
	}
	if id := p.Identify(bearerReq("alice-token")); p.CanAdmin(id, "") {
		t.Fatal("non-admin gitlab user must not CanAdmin")
	}

	// Provider-level and deployment-wide admin lists apply to both kinds
	// (GitHub's user API carries no admin bit).
	p2 := &MultiSSO{
		Providers: []SSOProvider{
			{ID: "gh", Kind: SSOKindGitHub, URL: gh.URL, Admins: []string{"octocat"}},
		},
		Admins:  []string{"root"},
		CacheTTL: 10 * time.Minute,
	}
	if id := p2.Identify(bearerReq("octo-token")); !p2.CanAdmin(id, "") {
		t.Fatal("provider admin list ignored")
	}
	if id := p2.Identify(bearerReq("root-token")); p2.CanAdmin(id, "") {
		t.Fatal("the global admin list must not validate a token the providers reject")
	}

	p3 := &MultiSSO{
		Providers: []SSOProvider{{ID: "gl", Kind: SSOKindGitLab, URL: gl.URL}},
		Admins:    []string{"alice"},
		CacheTTL:  10 * time.Minute,
	}
	if id := p3.Identify(bearerReq("alice-token")); !p3.CanAdmin(id, "") {
		t.Fatal("deployment-wide admins list ignored")
	}
}

func TestMultiSSOWireDecisions(t *testing.T) {
	gl := fakeGitLab(t, nil)
	p := &MultiSSO{Providers: []SSOProvider{{ID: "gl", Kind: SSOKindGitLab, URL: gl.URL}}}
	id := p.Identify(bearerReq("alice-token"))
	if !p.CanFetch(id, "demo", "") || !p.CanPush(id, "demo", "", "main") {
		t.Fatal("validated identity must fetch/push")
	}
	if p.CanFetch(Identity{}, "demo", "") {
		t.Fatal("anonymous must not fetch")
	}
	if p.IdentifyKey("alice", []byte("key")).User != "" {
		t.Fatal("SSH seam must stay anonymous in token-fronted mode")
	}
	refs := []Ref{{Name: "refs/heads/main", OID: "a"}}
	if got := p.VisibleRefs(id, "demo", refs); len(got) != 1 {
		t.Fatal("VisibleRefs must be a passthrough")
	}
}

func TestSSOProviderUserURL(t *testing.T) {
	cases := []struct {
		p    SSOProvider
		want string
	}{
		{SSOProvider{Kind: SSOKindGitLab, URL: "http://gl:8180/"}, "http://gl:8180/api/v4/user"},
		{SSOProvider{Kind: SSOKindGitHub}, "https://api.github.com/user"},
		{SSOProvider{Kind: SSOKindGitHub, URL: "https://github.com"}, "https://api.github.com/user"},
		{SSOProvider{Kind: SSOKindGitHub, URL: "https://github.example.com"}, "https://github.example.com/api/v3/user"},
	}
	for _, c := range cases {
		if got := c.p.userURL(); got != c.want {
			t.Fatalf("userURL(%+v) = %s, want %s", c.p, got, c.want)
		}
	}
}
