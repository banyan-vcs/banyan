// auth_tokens_test.go — T79/T80/T81 (docs/gateway/authorization.md §5-6, docs/project/roadmap.md M5/M2/M3):
//
//	T79 token auth end to end — mint via RPC, fetch/push with git as the
//	    credential, scope ceiling (read can never push), revocation
//	T80 RPC fine-grained authorization — management endpoints admin-only,
//	    content writes behind the same CanPush the git wire enforces
//	T81 path diff + blame RPCs — byte-compared against git's own answers
package authz

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// newTokenServer boots the full plane with the M5 composition: static
// users wrapped by the metadata-plane token decorator — passwords, SSH keys
// and PATs through the same auth.Service seam.
func newTokenServer(t *testing.T) (*support.Server, *meta.MemoryMetaStore) {
	t.Helper()
	policy := &auth.StaticPolicy{
		Users:  map[string]string{"alice": "a", "bob": "b", "admin": "x"},
		Admins: []string{"admin"},
	}
	ms := meta.NewMemoryMetaStore()
	objects := storage.NewObjectService(ms, storage.NewLocalShardStore(support.TempDirQuiet(t)),
		[]storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	svc := &auth.TokenPolicy{Inner: policy, Tokens: ms}
	mux := http.NewServeMux()
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.Auth = svc
	gw.Register(mux)
	api := rpc.NewServer(ms, objects, model.SHA1)
	api.Auth = svc
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &support.Server{Server: srv}, ms
}

// mintToken creates a PAT over the RPC as user.
func mintToken(t *testing.T, s *support.Server, user, pass, scope string) (plaintext, hash string) {
	t.Helper()
	body := fmt.Sprintf(`{"name":"e2e","scope":%q}`, scope)
	req, _ := http.NewRequest("POST", s.URL+"/api/v1/auth/tokens", strings.NewReader(body))
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	require.Equal(t, http.StatusOK, resp.StatusCode, "mint %s token as %s: %s", scope, user, raw)
	var out struct {
		Token string `json:"token"`
		Hash  string `json:"hash"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.True(t, strings.HasPrefix(out.Token, "banyan_"), "plaintext shape")
	return out.Token, out.Hash
}

func TestTokenAuthE2E(t *testing.T) {
	support.NeedGit(t)
	s, ms := newTokenServer(t)

	readTok, readHash := mintToken(t, s, "alice", "a", "read")
	writeTok, _ := mintToken(t, s, "alice", "a", "write")

	// Seed as alice over the password path (auto-create needs push rights —
	// a credentialed URL; the anonymous shape would 401 and wake the local
	// credential manager).
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "readme-v1\n")
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-v1\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed tk")
	support.Git(t, src, "push", "-q", credURL(s, "alice", "a", "/r/tk.git"), "main")

	// A read token fetches.
	ro := t.TempDir()
	support.Git(t, "", "clone", "-q", credURL(s, "alice", readTok, "/r/tk.git"), ro)
	assert.Equal(t, "readme-v1\n", support.ReadFile(t, filepath.Join(ro, "README.md")))

	// The read token cannot push — scope ceiling, git sees the rejection.
	support.Write(t, filepath.Join(ro, "README.md"), "readme-2\n")
	support.Git(t, ro, "commit", "-qam", "t79: read token")
	out, err := support.GitTry(ro, "push", "-q", "origin", "main")
	require.Error(t, err, "read-scoped token must not push:\n%s", out)
	require.Contains(t, out, "permission denied")

	// The write token pushes the same change.
	out, err = support.GitTry(ro, "push", "-q", credURL(s, "alice", writeTok, "/r/tk.git"), "main")
	require.NoError(t, err, "write token push:\n%s", out)

	// A token for one user does not authenticate as another.
	out, err = support.GitTry("", "-c", "credential.helper=", "clone", "-q", credURL(s, "bob", readTok, "/r/tk.git"), t.TempDir())
	require.Error(t, err, "owner mismatch must reject:\n%s", out)

	// Revocation kills the credential immediately.
	require.NoError(t, ms.RevokeToken(t.Context(), meta.HashToken(readTok)))
	out, err = support.GitTry("", "-c", "credential.helper=", "clone", "-q", credURL(s, "alice", readTok, "/r/tk.git"), t.TempDir())
	require.Error(t, err, "revoked token must reject:\n%s", out)

	// Self-service listing shows the tokens, never the plaintext.
	req, _ := http.NewRequest("GET", s.URL+"/api/v1/auth/tokens", nil)
	req.SetBasicAuth("alice", "a")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var listed struct {
		Tokens []meta.TokenInfo `json:"tokens"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	assert.Len(t, listed.Tokens, 2, "alice sees her two tokens")
	for _, tok := range listed.Tokens {
		assert.NotContains(t, tok.Hash, "banyan_", "list answers hashes, not plaintexts")
		if tok.Hash == readHash {
			assert.True(t, tok.Revoked, "revocation visible in listing")
		}
	}
}

func TestRPCFineGrainedAuthorization(t *testing.T) {
	support.NeedGit(t)
	s, _ := newTokenServer(t)
	// Seed with credentials: the anonymous URL would 401 and wake the local
	// credential manager (Windows GCM pops a dialog).
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "rz\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed rz")
	support.Git(t, src, "push", "-q", credURL(s, "alice", "a", "/r/rz.git"), "main")
	_ = src

	rpcCall := func(method, path, user, pass, body string) int {
		t.Helper()
		var rd *strings.Reader
		if body == "" {
			rd = strings.NewReader("")
		} else {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, s.URL+path, rd)
		req.SetBasicAuth(user, pass)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		resp.Body.Close()
		t.Logf("%s %s as %s -> %d %s", method, path, user, resp.StatusCode, raw)
		return resp.StatusCode
	}

	// Management is admin territory: developers (and developer tokens) are
	// 403, the admin passes.
	require.Equal(t, http.StatusForbidden, rpcCall("POST", "/api/v1/repack", "alice", "a", `{"repo":"rz"}`))
	require.Equal(t, http.StatusOK, rpcCall("POST", "/api/v1/repack", "admin", "x", `{"repo":"rz"}`))
	require.Equal(t, http.StatusForbidden, rpcCall("GET", "/api/v1/cluster/nodes", "alice", "a", ""))
	require.Equal(t, http.StatusOK, rpcCall("GET", "/api/v1/cluster/nodes", "admin", "x", ""))
	require.Equal(t, http.StatusForbidden, rpcCall("PUT", "/api/v1/repos/rz/force-push", "alice", "a", `{"enabled":true}`))
	require.Equal(t, http.StatusOK, rpcCall("PUT", "/api/v1/repos/rz/force-push", "admin", "x", `{"enabled":false}`))

	// An admin-scoped token manages too.
	adminTok, _ := mintToken(t, s, "admin", "x", "admin")
	require.Equal(t, http.StatusOK, rpcCall("POST", "/api/v1/repack", "admin", adminTok, `{"repo":"rz"}`))

	// Content writes sit behind the same CanPush the wire enforces — a
	// read token cannot move refs (403 fires before any CAS attempt).
	readTok, _ := mintToken(t, s, "alice", "a", "read")
	require.Equal(t, http.StatusForbidden, rpcCall("POST", "/api/v1/refs", "alice", readTok, `{"repo":"rz","name":"refs/heads/dev","old":"0000000000000000000000000000000000000000","new":"0000000000000000000000000000000000000000"}`))

	// Repo creation carries the push gate: read tokens cannot create.
	require.Equal(t, http.StatusForbidden, rpcCall("POST", "/api/v1/repos", "alice", readTok, `{"name":"fresh"}`))
	require.Equal(t, http.StatusOK, rpcCall("POST", "/api/v1/repos", "alice", "a", `{"name":"fresh"}`))
}

func TestDiffBlameRPC(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)

	// A tiny history: two commits touching lib/a.txt and README.
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "readme-1\n")
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-1\nline-2\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "t81: first")
	support.Git(t, src, "push", "-q", s.RepoURL("d"), "main")
	first := support.HeadOf(t, src)

	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-1\nline-2 changed\nline-3\n")
	support.Write(t, filepath.Join(src, "top.txt"), "top\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "t81: second")
	support.Git(t, src, "push", "-q", s.RepoURL("d"), "main")

	get := func(path string) string {
		t.Helper()
		resp, err := http.Get(s.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		body := make([]byte, 1<<20)
		n, _ := resp.Body.Read(body)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", path, string(body[:n]))
		return string(body[:n])
	}

	// T81a path diff: the RPC text must be byte-equal to git's own diff
	// (the engine is the gitaly one T63 pinned; this pins the JSON surface).
	from, to := first, support.HeadOf(t, src)
	wantDiff := support.Git(t, src, "diff", from, to)
	assert.Equal(t, wantDiff, get(fmt.Sprintf("/api/v1/diff?repo=d&from=%s&to=%s", from, to)), "diff RPC vs git diff")

	// Path filtering: the lib-only view equals git's path-filtered diff.
	wantLib := support.Git(t, src, "diff", from, to, "--", "lib")
	assert.Equal(t, wantLib, get(fmt.Sprintf("/api/v1/diff?repo=d&from=%s&to=%s&path=lib/", from, to)), "path-prefix diff")

	// T81b blame: byte-equal to git blame --line-porcelain.
	wantBlame := support.Git(t, src, "blame", "--line-porcelain", "HEAD", "--", "lib/a.txt")
	assert.Equal(t, wantBlame, get("/api/v1/blame?repo=d&rev=main&path=lib/a.txt"), "blame RPC vs git blame")
}
