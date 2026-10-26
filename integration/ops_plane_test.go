package integration

// T54 (docs/13): the ops plane observes real git traffic end to end — the
// expvar serve-path counters (which rung served each fetch, pushes applied)
// move as real clients push/clone/repack/clone, and the pprof family
// answers on the same plane. Package-level counters are process-wide;
// tests in this package run serially, so deltas are deterministic.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banyan-vcs/banyan/internal/debug"
	"github.com/stretchr/testify/require"
)

func TestOpsPlaneObservesServing(t *testing.T) {
	needGit(t)
	srv := newServer(t)
	ops := httptest.NewServer(debug.Handler())
	t.Cleanup(ops.Close)

	counter := func(t *testing.T, name string) int64 {
		t.Helper()
		resp, err := http.Get(ops.URL + "/debug/vars")
		require.NoError(t, err)
		defer resp.Body.Close()
		var vars map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&vars))
		v, ok := vars[name].(float64)
		require.True(t, ok, "counter %s missing from /debug/vars: %v", name, vars)
		return int64(v)
	}

	push0 := counter(t, "banyan_push_accepted")
	encode0 := counter(t, "banyan_fetch_encoded")
	exact0 := counter(t, "banyan_fetch_exact_pack_stream")
	cache0 := counter(t, "banyan_fetch_closure_cache")

	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("ops"), 0o644))
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "c1")
	git(t, src, "remote", "add", "origin", srv.repoURL("t54"))
	git(t, src, "push", "origin", "main")
	require.Equal(t, push0+1, counter(t, "banyan_push_accepted"), "push counted")

	// Clone #1: the one-push closure equals the pushed pack's object set, so
	// the clone-shaped fast path streams it verbatim.
	dst1 := t.TempDir()
	git(t, dst1, "clone", srv.repoURL("t54"), "r")
	require.GreaterOrEqual(t, counter(t, "banyan_fetch_exact_pack_stream"), exact0+1, "single-pack clone served by the exact-pack stream")

	// A second push fragments the closure across two packs: no exact hit,
	// the response cache is cold for the new wants — the online encoder
	// serves (and warms the cache).
	require.NoError(t, os.WriteFile(filepath.Join(src, "b.txt"), []byte("more"), 0o644))
	git(t, src, "add", ".")
	git(t, src, "commit", "-m", "c2")
	git(t, src, "push", "origin", "main")
	dst2 := t.TempDir()
	git(t, dst2, "clone", srv.repoURL("t54"), "r")
	require.GreaterOrEqual(t, counter(t, "banyan_fetch_encoded"), encode0+1, "fragmented closure served by the encoder")

	// Clone #3 of the same tip: served straight from the response cache.
	dst3 := t.TempDir()
	git(t, dst3, "clone", srv.repoURL("t54"), "r")
	require.GreaterOrEqual(t, counter(t, "banyan_fetch_closure_cache"), cache0+1, "repeat clone served from the response cache")

	// Repack consolidates the fragments; the next clone matches the stored
	// pack exactly — the streaming fast path again.
	resp, err := http.Post(srv.URL+"/api/v1/repack", "application/json",
		strings.NewReader(`{"repo":"t54"}`))
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	dst4 := t.TempDir()
	git(t, dst4, "clone", srv.repoURL("t54"), "r")
	require.GreaterOrEqual(t, counter(t, "banyan_fetch_exact_pack_stream"), exact0+2, "post-repack clone served by the exact-pack stream")

	// The profiling family answers on the same plane with consumable bytes.
	presp, perr := http.Get(ops.URL + "/debug/pprof/heap")
	require.NoError(t, perr)
	defer presp.Body.Close()
	require.Equal(t, 200, presp.StatusCode)
	magic := make([]byte, 2)
	_, _ = io.ReadFull(presp.Body, magic)
	require.Equal(t, []byte{0x1f, 0x8b}, magic, "heap profile must be gzip'd protobuf")

	sresp, serr := http.Get(ops.URL + "/debug/stats")
	require.NoError(t, serr)
	defer sresp.Body.Close()
	var st map[string]any
	require.NoError(t, json.NewDecoder(sresp.Body).Decode(&st))
	require.NotEmpty(t, st["go_version"])
}
