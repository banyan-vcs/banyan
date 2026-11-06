// auth_tokens_test.go — T79/T80/T81 (docs/07 §5-6, docs/14 M5/M2/M3):
//
//	T79 token auth end to end — mint via RPC, fetch/push with git as the
//	    credential, scope ceiling (read can never push), revocation
//	T80 RPC fine-grained authorization — management endpoints admin-only,
//	    content writes behind the same CanPush the git wire enforces
//	T81 path diff + blame RPCs — byte-compared against git's own answers
package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// newTokenServer boots the full plane with the M5 composition: static
// users wrapped by the metadata-plane token decorator — passwords, SSH keys
// and PATs through the same auth.Service seam.
func newTokenServer(t *testing.T) (*server, *storage.MemoryMetaStore) {
	t.Helper()
	policy := &auth.StaticPolicy{
		Users: map[string]string{"alice": "a", "bob": "b", "admin": "x"},
	}
	meta := storage.NewMemoryMetaStore()
	objects := storage.NewObjectService(meta, storage.NewLocalShardStore(tempDirQuiet(t)),
		[]storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	svc := &auth.TokenPolicy{Inner: policy, Tokens: meta}
	mux := http.NewServeMux()
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	gw.Auth = svc
	gw.Register(mux)
	api := rpc.NewServer(meta, objects, model.SHA1)
	api.Auth = svc
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &server{Server: srv}, meta
}

// mintToken creates a PAT over the RPC as user.
func mintToken(t *testing.T, s *server, user, pass, scope string) (plaintext, hash string) {
	t.Helper()
	body := fmt.Sprintf(`{"name":"e2e","scope":%q}`, scope)
	req, _ := http.NewRequest("POST", s.URL+"/api/v1/auth/tokens", strings.NewReader(body))
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "mint %s token as %s", scope, user)
	var out struct {
		Token string `json:"token"`
		Hash  string `json:"hash"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.True(t, strings.HasPrefix(out.Token, "banyan_"), "plaintext shape")
	return out.Token, out.Hash
}

func TestTokenAuthE2E(t *testing.T) {
	needGit(t)
	s, meta := newTokenServer(t)

	readTok, readHash := mintToken(t, s, "alice", "a", "read")
	writeTok, _ := mintToken(t, s, "alice", "a", "write")

	// Seed as alice over the password path (auto-create needs push rights).
	src := seedRepo(t, s, "tk")

	// A read token fetches.
	home := cloneRepo(t, s, s.repoURL("tk"))
	_ = home
	ro := t.TempDir()
	git(t, "", "clone", "-q", credURL(s, "alice", readTok, "/r/tk.git"), ro)
	assert.Equal(t, "readme-v1\n", readFile(t, filepath.Join(ro, "README.md")))

	// The read token cannot push — scope ceiling, git sees the rejection.
	write(t, filepath.Join(ro, "README.md"), "readme-2\n")
	git(t, ro, "commit", "-qam", "t79: read token")
	out, err := gitTry(ro, "push", "-q", "origin", "main")
	require.Error(t, err, "read-scoped token must not push:\n%s", out)
	require.Contains(t, out, "permission denied")

	// The write token pushes the same change.
	out, err = gitTry(ro, "push", "-q", credURL(s, "alice", writeTok, "/r/tk.git"), "main")
	require.NoError(t, err, "write token push:\n%s", out)

	// A token for one user does not authenticate as another.
	out, err = gitTry("", "-c", "credential.helper=", "clone", "-q", credURL(s, "bob", readTok, "/r/tk.git"), t.TempDir())
	require.Error(t, err, "owner mismatch must reject:\n%s", out)

	// Revocation kills the credential immediately.
	require.NoError(t, meta.RevokeToken(t.Context(), storage.HashToken(readTok)))
	out, err = gitTry("", "-c", "credential.helper=", "clone", "-q", credURL(s, "alice", readTok, "/r/tk.git"), t.TempDir())
	require.Error(t, err, "revoked token must reject:\n%s", out)

	// Self-service listing shows the tokens, never the plaintext.
	req, _ := http.NewRequest("GET", s.URL+"/api/v1/auth/tokens", nil)
	req.SetBasicAuth("alice", "a")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var listed struct {
		Tokens []storage.TokenInfo `json:"tokens"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	assert.Len(t, listed.Tokens, 2, "alice sees her two tokens")
	for _, tok := range listed.Tokens {
		assert.NotContains(t, tok.Hash, "banyan_", "list answers hashes, not plaintexts")
		if tok.Hash == readHash {
			assert.True(t, tok.Revoked, "revocation visible in listing")
		}
	}
	_ = src
}

func TestRPCFineGrainedAuthorization(t *testing.T) {
	needGit(t)
	s, meta := newTokenServer(t)
	seedRepo(t, s, "rz")

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
		resp.Body.Close()
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
	_ = meta
}

func TestDiffBlameRPC(t *testing.T) {
	needGit(t)
	s := newServer(t)

	// A tiny history: two commits touching lib/a.txt and README.
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "README.md"), "readme-1\n")
	write(t, filepath.Join(src, "lib", "a.txt"), "a-1\nline-2\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t81: first")
	git(t, src, "push", "-q", s.repoURL("d"), "main")
	first := headOf(t, src)

	write(t, filepath.Join(src, "lib", "a.txt"), "a-1\nline-2 changed\nline-3\n")
	write(t, filepath.Join(src, "top.txt"), "top\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t81: second")
	git(t, src, "push", "-q", s.repoURL("d"), "main")

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
	from, to := first, headOf(t, src)
	wantDiff := git(t, src, "diff", from, to)
	assert.Equal(t, wantDiff, get(fmt.Sprintf("/api/v1/diff?repo=d&from=%s&to=%s", from, to)), "diff RPC vs git diff")

	// Path filtering: the lib-only view equals git's path-filtered diff.
	wantLib := git(t, src, "diff", from, to, "--", "lib")
	assert.Equal(t, wantLib, get(fmt.Sprintf("/api/v1/diff?repo=d&from=%s&to=%s&path=lib/", from, to)), "path-prefix diff")

	// T81b blame: byte-equal to git blame --line-porcelain.
	wantBlame := git(t, src, "blame", "--line-porcelain", "HEAD", "--", "lib/a.txt")
	assert.Equal(t, wantBlame, get("/api/v1/blame?repo=d&rev=main&path=lib/a.txt"), "blame RPC vs git blame")
}
