package integration

// T55 (docs/13): the multi-pack clone-shaped streaming rung — the "way in
// is the way out" fix for streamed imports (docs/03 §3). A 72 MiB
// incompressible repo fragmented across three packs (three pushes, no
// repack) clones COMPLETE under a 64 MiB soft heap limit: the closure maps
// alone would exceed the limit, so completion is itself the proof that
// nothing O(closure) is resident. The streamed rung is asserted through
// the ops-plane counter, and the cloned bytes are verified against the
// source.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	rdebug "runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/banyan-vcs/banyan/internal/debug"
	"github.com/stretchr/testify/require"
)

func TestStreamingCloneMultiPack(t *testing.T) {
	if os.Getenv("BANYAN_MEMCHAIN") == "off" {
		t.Skip("BANYAN_MEMCHAIN=off")
	}
	needGit(t)
	rdebug.SetMemoryLimit(64 << 20)

	var peak atomic.Int64
	stop := make(chan struct{})
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
	defer close(stop)

	srv := newServer(t)
	ops := httptest.NewServer(debug.Handler())
	t.Cleanup(ops.Close)
	counter := func(name string) int64 {
		resp, err := http.Get(ops.URL + "/debug/vars")
		require.NoError(t, err)
		defer resp.Body.Close()
		var vars map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&vars))
		v, ok := vars[name].(float64)
		require.True(t, ok, "counter %s missing: %v", name, vars)
		return int64(v)
	}

	// Three pushes × 12 incompressible 2 MiB blobs = 72 MiB across three
	// packs (disjoint files per round keep every push off-pack). No repack:
	// the fragments stay, so the clone's closure spans all three packs and
	// the exact-pack fast path misses by construction.
	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	git(t, src, "remote", "add", "origin", srv.repoURL("t55"))
	for round := 0; round < 3; round++ {
		for i := 0; i < 12; i++ {
			blob := make([]byte, 2<<20)
			_, _ = rand.Read(blob)
			require.NoError(t, os.WriteFile(
				filepath.Join(src, fmt.Sprintf("r%d-b%02d.bin", round, i)), blob, 0o644))
		}
		git(t, src, "add", ".")
		git(t, src, "commit", "-m", fmt.Sprintf("round %d", round))
		git(t, src, "push", "origin", "main")
	}

	streamed0 := counter("banyan_fetch_streamed")
	dst := t.TempDir()
	git(t, dst, "clone", srv.repoURL("t55"), "r")
	require.Equal(t, streamed0+1, counter("banyan_fetch_streamed"),
		"multi-pack clone took the streaming rung")

	// Byte-level verification against the source: every round's first blob
	// plus the full file listing.
	for round := 0; round < 3; round++ {
		want, err := os.ReadFile(filepath.Join(src, fmt.Sprintf("r%d-b00.bin", round)))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dst, "r", fmt.Sprintf("r%d-b00.bin", round)))
		require.NoError(t, err)
		require.Len(t, got, len(want))
		require.Equal(t, want, got, "round %d blob round-trips byte-for-byte", round)
	}
	files := strings.Fields(git(t, filepath.Join(dst, "r"), "ls-files"))
	require.Len(t, files, 36, "all three rounds' blobs present: %v", files)

	t.Logf("streaming clone: 72 MiB across 3 packs under the 64 MiB soft limit, peak heap %.1f MiB",
		float64(peak.Load())/(1<<20))
}
