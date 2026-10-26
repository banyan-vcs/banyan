package integration

// T53 (docs/13): the clone-shaped serving contract, in two halves that the
// same scenario pins together —
//
//  1. gitlink safety: a tree entry with mode 160000 references a commit in
//     ANOTHER repository (a submodule pin); the pushed pack never contains
//     it, and the walk must not try to fetch it (the old probe-based walk
//     failed the whole enumeration on the missing object and the clone died
//     with "remote did not send all necessary objects").
//  2. zero whole-pack reads: with a repacked (exact-pack-hit) clone, the
//     serving path streams the pack verbatim and the enumeration probe is
//     type-directed — no GetPack (whole pack into memory) ever runs on the
//     local backend. This is the regression guard for the T50 post-mortem
//     (docs/17 §7): a 20 MiB clone used to allocate ~2.5× the repo because
//     the type probe inflated every object through the pack decoder.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/stretchr/testify/require"
)

// countingBackend wraps the all-in-one backend to observe whole-pack reads.
type countingBackend struct {
	*storage.LocalBackend
	getPack atomic.Int64
}

func (c *countingBackend) GetPack(ctx context.Context, repo string, packOID model.OID, holders []string) ([]byte, error) {
	c.getPack.Add(1)
	return c.LocalBackend.GetPack(ctx, repo, packOID, holders)
}

func TestCloneShapedServing(t *testing.T) {
	needGit(t)
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	backend := &countingBackend{LocalBackend: &storage.LocalBackend{Local: local, NodeID: "t1"}}
	objects := storage.NewObjectServiceWithBackend(meta, backend, model.SHA1)

	mux := http.NewServeMux()
	gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// The referenced commit lives only in this scratch repo — never pushed
	// anywhere — so the gitlink target is genuinely absent server-side.
	sub := t.TempDir()
	git(t, sub, "init", "-b", "main", ".")
	require.NoError(t, os.WriteFile(filepath.Join(sub, "s.txt"), []byte("sub"), 0o644))
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-m", "sub")
	subOID := strings.TrimSpace(git(t, sub, "rev-parse", "HEAD"))

	src := t.TempDir()
	require.NoError(t, meta.CreateRepo(context.Background(), "gl", model.SHA1))
	git(t, src, "init", "-b", "main", ".")
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("body"), 0o644))
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "base")
	git(t, src, "update-index", "--add", "--cacheinfo", "160000,"+subOID+",vendor")
	git(t, src, "commit", "-m", "pin a submodule")
	git(t, src, "remote", "add", "origin", srv.URL+"/r/gl.git")
	git(t, src, "push", "origin", "main")

	// Consolidate the first push's fragments into one pack so the clone is
	// clone-shaped (exact-pack hit), same as the T50 flow.
	_, err := http.Post(srv.URL+"/api/v1/repack", "application/json",
		strings.NewReader(`{"repo":"gl"}`))
	require.NoError(t, err)

	// Measurement window: the clone itself must not read any pack whole.
	backend.getPack.Store(0)
	dst := t.TempDir()
	git(t, dst, "clone", srv.URL+"/r/gl.git", "gl")

	require.Zero(t, backend.getPack.Load(),
		"clone-shaped serving streamed without a whole-pack read (enumeration is type-directed, serve is verbatim)")
	ls := git(t, filepath.Join(dst, "gl"), "ls-files", "-s")
	require.Contains(t, ls, "160000 "+subOID+" 0\tvendor", "submodule pin survived the round trip")
	body, err := os.ReadFile(filepath.Join(dst, "gl", "f.txt"))
	require.NoError(t, err)
	require.Equal(t, "body", string(body))
}
