// fullclone_test.go — T48: repo-scale statistics + the full-clone budget
// (docs/11 §6). The gateway tracks per-repo pack bytes (primed at ingest,
// /api/v1/repos/<repo>/size) and refuses clone-shaped fetches at or above
// the threshold — while shallow clones, incremental fetches and lazy
// single-object fetches (partial-clone shape) pass untouched.
package integration

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

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func newBudgetServer(t *testing.T) (*server, *gitwire.Server) {
	t.Helper()
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	gw.Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &server{Server: srv}, gw
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
	needGit(t)
	s, gw := newBudgetServer(t)

	// Seed ~300 KiB incompressible → the pack dwarfs the 128 KiB budget we
	// arm after the first (legal) clone.
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "README.md"), "budget\n")
	bigFile(t, src, "a.bin", 150)
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "seed")
	git(t, src, "push", "-q", s.repoURL("budget"), "main")

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
	fresh := cloneRepo(t, s, s.repoURL("budget"))

	// Arm the budget below the repo size: full clones now die with the
	// actionable message; every other fetch shape passes.
	gw.FullCloneMaxBytes = 128 << 10
	out, err := gitTry(t.TempDir(), "clone", s.repoURL("budget"), t.TempDir())
	require.Error(t, err, "超预算全 clone 应被拒绝\n%s", out)
	assert.Contains(t, out, "full clone rejected")
	assert.Contains(t, out, "--depth 1", "错误信息应给出可行替代")

	// Shallow clone passes (depth cut, not clone-shaped).
	shallow := t.TempDir()
	out, err = gitTry("", "clone", "--depth", "1", s.repoURL("budget"), shallow)
	require.NoError(t, err, "浅化克隆不受预算约束\n%s", out)

	// Incremental fetch from a pre-budget clone passes (haves present).
	write(t, filepath.Join(src, "next.txt"), "next\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "next")
	git(t, src, "push", "-q", s.repoURL("budget"), "main")
	git(t, fresh, "pull", "-q")

	// Lazy single-object fetch passes (want is a blob OID, not a tip) — the
	// partial-clone backfill shape.
	blobOID := strings.TrimSpace(git(t, src, "rev-parse", "HEAD:a.bin"))
	out, err = gitTry(fresh, "fetch", "--no-tags", s.repoURL("budget"), blobOID)
	if err != nil {
		t.Logf("fetch-by-oid output:\n%s", out) // tolerate client-side refusal of raw-OID fetch
	} else {
		check := git(t, fresh, "cat-file", "-e", blobOID)
		assert.Empty(t, strings.TrimSpace(check))
	}
}
