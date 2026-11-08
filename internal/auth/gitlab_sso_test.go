package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newFakeGitLab answers /api/v4/user for the tokens it knows.
func newFakeGitLab(t *testing.T, hits *atomic.Int32) *httptest.Server {
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

func bearerReq(token string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestGitLabSSOIdentify(t *testing.T) {
	var hits atomic.Int32
	gl := newFakeGitLab(t, &hits)
	p := NewGitLabSSO(gl.URL)
	p.CacheTTL = 10 * time.Minute

	// Bearer form.
	if id := p.Identify(bearerReq("alice-token")); id.User != "alice" || id.Name != "Alice" {
		t.Fatalf("bearer identity = %+v", id)
	}
	// git's HTTP Basic convention: any username, token as the password.
	for _, user := range []string{"oauth2", "alice"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetBasicAuth(user, "alice-token")
		if id := p.Identify(r); id.User != "alice" {
			t.Fatalf("basic(%q) identity = %+v", user, id)
		}
	}
	// No credentials, wrong tokens: anonymous.
	if id := p.Identify(httptest.NewRequest("GET", "/", nil)); !id.Anonymous() {
		t.Fatalf("no credentials = %+v", id)
	}
	if id := p.Identify(bearerReq("wrong")); !id.Anonymous() {
		t.Fatalf("bad token = %+v", id)
	}
	// All of the above validated exactly once per token (cache holds).
	if n := hits.Load(); n != 2 {
		t.Fatalf("gitlab hits = %d, want 2 (alice-token + wrong)", n)
	}

	// Git access decisions: authenticated only.
	if !p.CanFetch(Identity{User: "alice"}, "r", "/lib") || !p.CanPush(Identity{User: "alice"}, "r", "/lib", "main") {
		t.Fatal("validated identity must fetch and push")
	}
	if p.CanFetch(Identity{}, "r", "") {
		t.Fatal("anonymous must not fetch")
	}
	// Branch visibility is a passthrough (GitLab semantics).
	refs := []Ref{{Name: "refs/heads/main"}}
	if out := p.VisibleRefs(Identity{User: "bob"}, "r", refs); len(out) != 1 {
		t.Fatalf("visible refs = %+v", out)
	}
	// The SSH seam is anonymous in GitLab topology.
	if id := p.IdentifyKey("alice", nil); !id.Anonymous() {
		t.Fatalf("ssh key = %+v", id)
	}
}

// Instance admins answer CanAdmin; the flag lapses with the cache TTL.
func TestGitLabSSOAdmin(t *testing.T) {
	var hits atomic.Int32
	gl := newFakeGitLab(t, &hits)
	base := time.Unix(1700000000, 0)
	p := NewGitLabSSO(gl.URL)
	p.CacheTTL = time.Minute
	p.Now = func() time.Time { return base }

	root := p.Identify(bearerReq("root-token"))
	if root.User != "root" || !p.CanAdmin(root, "r") {
		t.Fatalf("root must be admin: %+v", root)
	}
	alice := p.Identify(bearerReq("alice-token"))
	if p.CanAdmin(alice, "r") {
		t.Fatal("alice is not an instance admin")
	}

	// After the TTL the admin answer lapses until the next validation.
	p.Now = func() time.Time { return base.Add(2 * time.Minute) }
	if p.CanAdmin(root, "r") {
		t.Fatal("stale admin flag must lapse")
	}
	// The same call revalidates (cache expiry) and admin returns.
	root = p.Identify(bearerReq("root-token"))
	if !p.CanAdmin(root, "r") {
		t.Fatal("revalidated admin must answer again")
	}
}

// GitLab unreachable or answering garbage is anonymous (fail-closed), and
// negatives cache like positives.
func TestGitLabSSOFailClosed(t *testing.T) {
	var hits atomic.Int32
	gl := newFakeGitLab(t, &hits)
	p := NewGitLabSSO(gl.URL)
	p.CacheTTL = time.Hour
	gl.Close() // unreachable
	if id := p.Identify(bearerReq("alice-token")); !id.Anonymous() {
		t.Fatalf("unreachable gitlab = %+v", id)
	}

	// Non-JSON 200 bodies are negatives too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "<html>not json</html>")
	}))
	t.Cleanup(srv.Close)
	p2 := NewGitLabSSO(srv.URL)
	p2.CacheTTL = time.Hour
	if id := p2.Identify(bearerReq("alice-token")); !id.Anonymous() {
		t.Fatalf("garbage body = %+v", id)
	}
}
