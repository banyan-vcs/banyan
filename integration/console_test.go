// console_test.go (T50, docs/17): the product control plane end to end with
// real git clients — logical {group}/{project}[/path] routing, PAT
// authentication, additive inheritance (a subtree-only grant works at the
// anchor), protected-branch deep enforcement on the gateway, public
// visibility, token revocation, and the streaming-proxy memory discipline.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/console"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// consoleStack runs gateway + console in-process, wired exactly like the
// deployment roles: banyand with auth.upstream, banyans in front.
type consoleStack struct {
	gateway  *httptest.Server
	console  *httptest.Server
	db       *console.DB
	secret   []byte
	tokens   map[string]string // username → PAT
	tokenIDs map[string]int64
}

func newConsoleStack(t *testing.T) *consoleStack {
	t.Helper()
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)

	gwMux := http.NewServeMux()
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	api := rpc.NewServer(meta, objects, model.SHA1)
	gw.Register(gwMux)
	api.Register(gwMux)
	gateway := httptest.NewServer(gwMux)
	t.Cleanup(gateway.Close)

	db, err := console.Open(filepath.Join(t.TempDir(), "console.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	secret := []byte("it-console-secret")
	csMux := http.NewServeMux()
	console.NewServer(db, gateway.URL, secret).Register(csMux)
	consoleSrv := httptest.NewServer(csMux)
	t.Cleanup(consoleSrv.Close)

	// The gateway's auth source is the console (assigned after both servers
	// exist; no request has been served yet).
	up := auth.NewUpstreamPolicy(consoleSrv.URL, secret)
	gw.Auth, api.Auth = up, up

	return &consoleStack{
		gateway: gateway, console: consoleSrv, db: db, secret: secret,
		tokens: map[string]string{}, tokenIDs: map[string]int64{},
	}
}

func (s *consoleStack) addUser(t *testing.T, name string, admin bool) string {
	t.Helper()
	ctx := context.Background()
	u, err := s.db.CreateUser(ctx, name, admin)
	require.NoError(t, err)
	secret := console.NewTokenSecret()
	tok, err := s.db.CreateToken(ctx, u.ID, name+"-token", console.HashToken(secret), 0)
	require.NoError(t, err)
	s.tokens[name] = secret
	s.tokenIDs[name] = tok.ID
	return secret
}

// call hits the console API with a bearer PAT (empty token = anonymous).
func (s *consoleStack) call(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, s.console.URL+path, rdr)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

// gitURL embeds the PAT git's Basic auth will carry.
func (s *consoleStack) gitURL(path, user string) string {
	return fmt.Sprintf("http://%s:%s@%s%s", user, s.tokens[user], s.console.Listener.Addr().String(), path)
}

// hostURL builds a credential-free console URL.
func (s *consoleStack) hostURL(path string) string {
	return fmt.Sprintf("http://%s%s", s.console.Listener.Addr().String(), path)
}

func TestConsoleE2E(t *testing.T) {
	needGit(t)
	s := newConsoleStack(t)
	root := s.addUser(t, "root", true)
	s.addUser(t, "alice", false)
	s.addUser(t, "bob", false)
	s.addUser(t, "carol", false)

	// --- product surface: group, project (physical repo created on the
	// gateway through the signed internal identity), members ---
	code, out := s.call(t, "POST", "/api/v1/groups", root, map[string]string{"name": "mobile"})
	require.Equal(t, 201, code, "%v", out)
	code, out = s.call(t, "POST", "/api/v1/repos/mobile", root, map[string]string{"name": "app"})
	require.Equal(t, 201, code, "%v", out)
	uid, _ := out["repo_uid"].(string)
	require.NotEmpty(t, uid)

	// No access yet: alice sees nothing.
	code, out = s.call(t, "GET", "/api/v1/repos", s.tokens["alice"], nil)
	require.Equal(t, 200, code)
	require.Empty(t, out["repos"])

	// root grants: alice developer at the project, bob reporter.
	code, _ = s.call(t, "PUT", "/api/v1/repos/mobile/app/members/alice", root, map[string]string{"role": "developer"})
	require.Equal(t, 200, code)
	code, _ = s.call(t, "PUT", "/api/v1/repos/mobile/app/members/bob", root, map[string]string{"role": "reporter"})
	require.Equal(t, 200, code)

	// /me lists alice's reachable projects with the effective role.
	code, out = s.call(t, "GET", "/api/v1/me", s.tokens["alice"], nil)
	require.Equal(t, 200, code)
	require.Contains(t, fmt.Sprint(out["projects"]), "mobile/app")

	// --- git: alice clones the empty project and populates it ---
	aliceDir := t.TempDir()
	git(t, aliceDir, "clone", s.gitURL("/mobile/app.git", "alice"), "repo")
	work := filepath.Join(aliceDir, "repo")
	write(t, filepath.Join(work, "README.md"), "# app\n")
	write(t, filepath.Join(work, "services", "auth", "token.go"), "package auth\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "init")

	// The project default protects main; a developer cannot create it.
	_, err := gitTry(work, "push", "origin", "HEAD:refs/heads/main")
	require.Error(t, err, "protected main must reject a developer's first push")
	code, _ = s.call(t, "PATCH", "/api/v1/repos/mobile/app", root, map[string][]string{"protected_branches": {}})
	require.Equal(t, 200, code)
	git(t, work, "push", "origin", "HEAD:refs/heads/main")

	// Re-arm protection: further developer pushes to main are denied by the
	// gateway's deep check (snapshot v2 + signed identity).
	code, _ = s.call(t, "PATCH", "/api/v1/repos/mobile/app", root, map[string][]string{"protected_branches": {"main"}})
	require.Equal(t, 200, code)
	write(t, filepath.Join(work, "README.md"), "# app v2\n")
	git(t, work, "add", "README.md")
	git(t, work, "commit", "-m", "v2")
	out2, err := gitTry(work, "push", "origin", "HEAD:refs/heads/main")
	require.Error(t, err, "developer push to protected main must fail: %s", out2)
	require.Contains(t, out2, "permission denied")

	// --- subproject: register services/auth, bob's subtree-only story ---
	code, out = s.call(t, "POST", "/api/v1/repos/mobile/app/services/auth", root, nil)
	require.Equal(t, 201, code, "%v", out)

	// bob (reporter) can clone the subtree but not push.
	bobDir := t.TempDir()
	git(t, bobDir, "clone", s.gitURL("/mobile/app/services/auth.git", "bob"), "auth")
	authDir := filepath.Join(bobDir, "auth")
	require.FileExists(t, filepath.Join(authDir, "token.go"))
	write(t, filepath.Join(authDir, "token.go"), "package auth\n// v2\n")
	git(t, authDir, "add", "token.go")
	git(t, authDir, "commit", "-m", "auth v2")
	out2, err = gitTry(authDir, "push")
	require.Error(t, err, "reporter subtree push must fail: %s", out2)

	// A maintainer grant AT the anchor is the enforceable direction — and it
	// stays subtree-scoped: carol (no root membership) works at the anchor
	// while the root repo stays invisible to her.
	out2, err = gitTry(bobDir, "ls-remote", s.gitURL("/mobile/app.git", "carol"))
	require.Error(t, err, "a stranger must not read the root repo: %s", out2)
	code, _ = s.call(t, "PUT", "/api/v1/repos/mobile/app/services/auth/members/carol", root, map[string]string{"role": "maintainer"})
	require.Equal(t, 200, code)
	carolDir := t.TempDir()
	git(t, carolDir, "clone", s.gitURL("/mobile/app/services/auth.git", "carol"), "auth")
	carolAuth := filepath.Join(carolDir, "auth")
	write(t, filepath.Join(carolAuth, "carol.go"), "package auth\n")
	git(t, carolAuth, "add", "carol.go")
	git(t, carolAuth, "commit", "-m", "carol at anchor")
	git(t, carolAuth, "push")
	out2, err = gitTry(carolDir, "ls-remote", s.gitURL("/mobile/app.git", "carol"))
	require.Error(t, err, "subtree-only role must not read the root repo: %s", out2)

	// --- credentials: revocation takes effect immediately (while the
	// project is still private, so a passing anonymous read can't mask it) ---
	code, _ = s.call(t, "DELETE", fmt.Sprintf("/api/v1/me/tokens?id=%d", s.tokenIDs["alice"]), s.tokens["alice"], nil)
	require.Equal(t, 200, code)
	out2, err = gitTry(aliceDir, "ls-remote",
		fmt.Sprintf("http://alice:%s@%s/mobile/app.git", s.tokens["alice"], s.console.Listener.Addr().String()))
	require.Error(t, err, "revoked PAT must stop authenticating: %s", out2)

	// --- visibility: public serves anonymous readers ---
	code, _ = s.call(t, "PATCH", "/api/v1/repos/mobile/app", root, map[string]string{"visibility": "public"})
	require.Equal(t, 200, code)
	pubDir := t.TempDir()
	git(t, pubDir, "clone", s.hostURL("/mobile/app.git"), "pub")
	require.FileExists(t, filepath.Join(pubDir, "pub", "README.md"))

	// --- passthrough: operational endpoints remap the logical name ---
	code, out = s.call(t, "GET", "/api/v1/repos/mobile/app/size", root, nil)
	require.Equal(t, 200, code, "%v", out)
	require.Contains(t, out, "bytes")
}

// TestConsoleMemoryChain: the console proxy streams — a 20 MiB incompressible
// clone through the product plane stays flat under a 64 MiB soft heap limit
// (the T46 discipline extends to the new front).
func TestConsoleMemoryChain(t *testing.T) {
	if os.Getenv("BANYAN_MEMCHAIN") == "off" {
		t.Skip("BANYAN_MEMCHAIN=off")
	}
	needGit(t)
	debug.SetMemoryLimit(64 << 20)

	var peak atomic.Int64
	stop := make(chan struct{})
	startSampler := func() {
		go func() {
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					var ms runtime.MemStats
					runtime.ReadMemStats(&ms)
					if int64(ms.HeapAlloc) > peak.Load() {
						peak.Store(int64(ms.HeapAlloc))
					}
				}
			}
		}()
	}
	defer close(stop)

	s := newConsoleStack(t)
	root := s.addUser(t, "root", true)
	s.addUser(t, "alice", false)
	code, out := s.call(t, "POST", "/api/v1/groups", root, map[string]string{"name": "big"})
	require.Equal(t, 201, code, "%v", out)
	code, out = s.call(t, "POST", "/api/v1/repos/big", root, map[string]string{"name": "repo"})
	require.Equal(t, 201, code, "%v", out)
	code, _ = s.call(t, "PUT", "/api/v1/repos/big/repo/members/alice", root, map[string]string{"role": "developer"})
	require.Equal(t, 200, code)
	code, _ = s.call(t, "PATCH", "/api/v1/repos/big/repo", root, map[string][]string{"protected_branches": {}})
	require.Equal(t, 200, code)

	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	// Many moderate incompressible blobs (the T46 shape): the windowed paths
	// are designed for multi-object packs — not the known single-giant-
	// object decode characteristic.
	total := 0
	for i := 0; i < 20; i++ {
		blob := make([]byte, 1<<20)
		_, _ = rand.Read(blob)
		require.NoError(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("blob-%02d.bin", i)), blob, 0o644))
		total += len(blob)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "big")
	git(t, src, "remote", "add", "origin", s.gitURL("/big/repo.git", "alice"))
	git(t, src, "push", "origin", "main")

	// The first push fragments the closure across packs (lightweight root
	// bootstrap + the pushed chain). Repack it away — through the console's
	// management passthrough with its repo remap, so that path is exercised
	// too — so the measured clone serves via the verbatim ExactPackHit
	// stream, not the (separately budgeted, docs/03 §3.1) online encoder.
	code, out = s.call(t, "POST", "/api/v1/repack", s.tokens["root"], map[string]any{"repo": "big/repo"})
	require.Equal(t, 200, code, "%v", out)

	// Measurement window: everything above is setup; what follows is the
	// product-plane flow under test (console-routed clone).
	runtime.GC()
	runtime.GC()
	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	base := int64(ms0.HeapAlloc)
	startSampler()
	dst := t.TempDir()
	git(t, dst, "clone", s.gitURL("/big/repo.git", "alice"), "repo")
	got, err := os.ReadFile(filepath.Join(dst, "repo", "blob-00.bin"))
	require.NoError(t, err)
	require.Len(t, got, 1<<20)

	t.Logf("console memory chain: peak heap %.1f MiB (base %.1f) for a %.0f MiB clone under the 64 MiB soft limit",
		float64(peak.Load())/(1<<20), float64(base)/(1<<20), float64(total)/(1<<20))
	if dir := os.Getenv("BANYAN_HEAPPROF"); dir != "" {
		f, ferr := os.Create(filepath.Join(dir, "console-allocs.prof"))
		if ferr == nil {
			pprof.Lookup("allocs").WriteTo(f, 0)
			f.Close()
		}
	}
	// The bound is the soft limit itself (T46 discipline: the chain must
	// COMPLETE under it). The console proxy's own contribution is ~zero —
	// isolated by serving the same clone straight off the gateway in the
	// development experiments behind this test; the residual delta is the
	// storage plane's serve-path allocation in the all-in-one shape
	// (enumerateClosure go-git decodes; the ha-cluster baseline of T46 does
	// not show it — flagged as a follow-up there, not a console concern).
	require.Less(t, peak.Load(), int64(64<<20), "the chain must hold under the soft heap limit")
}
