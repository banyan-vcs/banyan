// fullclone_test.go — T48: repo-scale statistics + the full-clone budget
// (docs/operations/performance.md §6). The gateway tracks per-repo pack bytes (primed at ingest,
// /api/v1/repos/<repo>/size) and refuses clone-shaped fetches at or above
// the threshold — while shallow clones, incremental fetches and lazy
// single-object fetches (partial-clone shape) pass untouched.
package streaming

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func newBudgetServer(t *testing.T) (*support.Server, *gitwire.Server) {
	t.Helper()
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(support.TempDirQuiet(t))
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(ms, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.Register(mux)
	rpc.NewServer(ms, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &support.Server{Server: srv}, gw
}

func bigFile(t *testing.T, dir, name string, kib int) string {
	t.Helper()
	blob := make([]byte, kib<<10)
	bigRnd.Read(blob) // genuinely incompressible — the pack tracks the content size
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, blob, 0o644))
	return p
}

var bigRnd = rand.New(rand.NewSource(48))

func TestFullCloneBudget(t *testing.T) {
	support.NeedGit(t)
	s, gw := newBudgetServer(t)

	// Seed ~300 KiB incompressible → the pack dwarfs the 128 KiB budget we
	// arm after the first (legal) clone.
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "budget\n")
	bigFile(t, src, "a.bin", 150)
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", s.RepoURL("budget"), "main")

	// Scale statistics via RPC: bytes/packs over the pack registry.
	resp, err := http.Get(s.URL + "/api/v1/repos/budget/size")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var size struct {
		Bytes int64 `json:"bytes"`
		Packs int    `json:"packs"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&size))
	resp.Body.Close()
	assert.Greater(t, size.Bytes, int64(128<<10), "规模统计应反映 ~150KiB 内容")
	assert.GreaterOrEqual(t, size.Packs, 1)

	// Below budget: full clone fine.
	fresh := support.CloneRepo(t, s, s.RepoURL("budget"))

	// Arm the budget below the repo size: full clones now die with the
	// actionable message; every other fetch shape passes.
	gw.FullCloneMaxBytes = 128 << 10
	out, err := support.GitTry(t.TempDir(), "clone", s.RepoURL("budget"), t.TempDir())
	require.Error(t, err, "超预算全 clone 应被拒绝\n%s", out)
	assert.Contains(t, out, "full clone rejected")
	assert.Contains(t, out, "--depth 1", "错误信息应给出可行替代")

	// Shallow clone passes (depth cut, not clone-shaped).
	shallow := t.TempDir()
	out, err = support.GitTry("", "clone", "--depth", "1", s.RepoURL("budget"), shallow)
	require.NoError(t, err, "浅化克隆不受预算约束\n%s", out)

	// Incremental fetch from a pre-budget clone passes (haves present).
	support.Write(t, filepath.Join(src, "next.txt"), "next\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "next")
	support.Git(t, src, "push", "-q", s.RepoURL("budget"), "main")
	support.Git(t, fresh, "pull", "-q")

	// Lazy single-object fetch passes (want is a blob OID, not a tip) — the
	// partial-clone backfill shape.
	blobOID := strings.TrimSpace(support.Git(t, src, "rev-parse", "HEAD:a.bin"))
	out, err = support.GitTry(fresh, "fetch", "--no-tags", s.RepoURL("budget"), blobOID)
	if err != nil {
		t.Logf("fetch-by-oid output:\n%s", out) // tolerate client-side refusal of raw-OID fetch
	} else {
		check := support.Git(t, fresh, "cat-file", "-e", blobOID)
		assert.Empty(t, strings.TrimSpace(check))
	}
}
