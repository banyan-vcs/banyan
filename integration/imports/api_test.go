// T92 — the import API (POST /api/v1/imports): one synchronous call that
// creates the target repo and pulls a smart-HTTP remote through
// internal/importer. Bootstrap-style: the source is another banyan's own
// git face. The web tier's import orchestration (job state, credential
// wrapping) rides this face; this pins the gateway contract itself.
package imports

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// newAuthImportServer boots the destination stack with the demo static
// policy (same wiring as the banyand demo) so the authn/write-rank gates
// are real.
func newAuthImportServer(t *testing.T) *support.Server {
	t.Helper()
	policy := &auth.StaticPolicy{
		Users:   map[string]string{"alice": "a", "bob": "b"},
		Admins:  []string{"admin"},
	}
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(support.TempDirQuiet(t))
	objects := storage.NewObjectService(ms, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	mux := http.NewServeMux()
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.Auth = policy
	gw.Register(mux)
	api := rpc.NewServer(ms, objects, model.SHA1)
	api.Auth = policy
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &support.Server{Server: srv}
}

func callImport(t *testing.T, s *support.Server, user, pass string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, s.URL+"/api/v1/imports", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestT92ImportAPI(t *testing.T) {
	support.NeedGit(t)
	// The source: a plain unauthenticated banyan serving a seeded repo with
	// a non-main default branch and a tag.
	src, _, _ := support.NewServerPlane(t)
	root := t.TempDir()
	support.Git(t, root, "init", "-q", "-b", "trunk", ".")
	support.Write(t, root+"/readme.md", "import via api\n")
	support.Git(t, root, "add", "-A")
	support.Git(t, root, "commit", "-qm", "c1")
	support.Git(t, root, "tag", "v1")
	support.Git(t, root, "push", "-q", src.RepoURL("t92-src"), "trunk", "--tags")

	dst := newAuthImportServer(t)

	// Auth gates: anonymous 401, authenticated developer passes.
	code, _ := callImport(t, dst, "", "", map[string]any{"url": src.RepoURL("t92-src"), "name": "t92-copy"})
	require.Equal(t, http.StatusUnauthorized, code, "anonymous must be challenged")

	code, body := callImport(t, dst, "alice", "a", map[string]any{"url": src.RepoURL("t92-src")})
	require.Equal(t, http.StatusOK, code, fmt.Sprint(body))
	assert.Equal(t, "t92-src", body["repo"], "name derived from the URL's last segment")
	assert.Equal(t, "trunk", body["defaultBranch"], "remote HEAD adopted")
	assert.InDelta(t, body["refs"], float64(2), 0, "trunk + tag")

	// An explicit name lands the same source under a chosen target.
	code, body = callImport(t, dst, "alice", "a", map[string]any{"url": src.RepoURL("t92-src"), "name": "t92-copy"})
	require.Equal(t, http.StatusOK, code, fmt.Sprint(body))
	assert.Equal(t, "t92-copy", body["repo"])

	// The imported repo clones back as a first-class banyan repo.
	out := t.TempDir()
	support.Git(t, out, "clone", "-q", strings.Replace(dst.RepoURL("t92-copy"), "http://", "http://alice:a@", 1), "c")
	assert.Equal(t, "import via api\n", support.ReadFile(t, out+"/c/readme.md"))
	assert.Equal(t, "trunk", strings.TrimSpace(support.Git(t, out+"/c", "branch", "--show-current")))

	// Imports land on fresh names only: a second run is a 409.
	code, _ = callImport(t, dst, "alice", "a", map[string]any{"url": src.RepoURL("t92-src"), "name": "t92-copy"})
	assert.Equal(t, http.StatusConflict, code)

	// The registered ssh gap and non-http remotes are invalid arguments,
	// not upstream failures.
	code, body = callImport(t, dst, "alice", "a", map[string]any{"url": "git@github.com:example/repo.git", "name": "x"})
	assert.Equal(t, http.StatusBadRequest, code, fmt.Sprint(body))
	code, body = callImport(t, dst, "alice", "a", map[string]any{"url": "http://127.0.0.1:1/r/none.git", "name": "t92-dead"})
	assert.Equal(t, http.StatusBadGateway, code, "unreachable remote is an upstream failure")
	errBody, _ := body["error"].(map[string]any)
	assert.Contains(t, errBody["message"], "import failed")

	// A failed import leaves no half-created repo behind.
	code, _ = callImport(t, dst, "alice", "a", map[string]any{"url": src.RepoURL("t92-src"), "name": "t92-dead"})
	assert.Equal(t, http.StatusOK, code, "the rolled-back name is free again")
}

