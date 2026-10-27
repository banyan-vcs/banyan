package integration

// T58 (docs/13): the deepen-shaped streaming rung (docs/03 §3) — unshallow
// of a large multi-pack repository. The ladder's closure materializes every
// object body of the deepened history; the rung carries the same
// depth/graft/have pruning typed and streams entry by entry. 54 MiB across
// three packs: a depth-1 clone (bounded set, ladder), then
// `git fetch --unshallow` under a 64 MiB soft heap limit must complete with
// the full history intact, take the streamed rung, and hold the peak below
// any ladder outcome (the ladder's resident bodies alone exceed 32 MiB).

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

func TestStreamingUnshallowMultiPack(t *testing.T) {
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

	// Three pushes × 9 incompressible 2 MiB blobs = 54 MiB across three
	// packs; the unshallow closure spans all of them, so the ladder would
	// hold ≥ 36 MiB of object bodies no matter what the client reports.
	src := t.TempDir()
	git(t, src, "init", "-b", "main", ".")
	git(t, src, "remote", "add", "origin", srv.repoURL("t58"))
	for round := 0; round < 3; round++ {
		for i := 0; i < 9; i++ {
			blob := make([]byte, 2<<20)
			_, _ = rand.Read(blob)
			require.NoError(t, os.WriteFile(
				filepath.Join(src, fmt.Sprintf("r%d-b%02d.bin", round, i)), blob, 0o644))
		}
		git(t, src, "add", ".")
		git(t, src, "commit", "-q", "-m", fmt.Sprintf("round %d", round))
		git(t, src, "push", "-q", "origin", "main")
	}

	streamed0 := counter("banyan_fetch_streamed")

	// Shallow clone, depth 1: the tip's snapshot is one commit + one tree,
	// but that tree carries every round's blobs (git add . accumulates), so
	// the group spans all three packs — past the cutoff, the rung streams a
	// depth-bounded set that is nonetheless whole-tree complete.
	dst := t.TempDir()
	git(t, dst, "clone", "-q", "--depth", "1", srv.repoURL("t58"), "r")
	c := filepath.Join(dst, "r")
	require.Equal(t, "1", strings.TrimSpace(git(t, c, "rev-list", "--count", "HEAD")),
		"the shallow clone carries exactly one commit")
	git(t, c, "fsck", "--no-dangling")
	require.Equal(t, streamed0+1, counter("banyan_fetch_streamed"),
		"the depth-1 clone streamed (its whole-tree snapshot spans all three packs)")

	// Unshallow: the remaining history (two commits + two trees, the held
	// snapshot is excluded, the want itself resent — closure() parity),
	// streamed under the same window.
	git(t, c, "fetch", "-q", "--unshallow", "origin")
	require.Equal(t, streamed0+2, counter("banyan_fetch_streamed"),
		"unshallow took the deepen streaming rung")
	require.Equal(t, "3", strings.TrimSpace(git(t, c, "rev-list", "--count", "--all")),
		"the whole history arrived")
	git(t, c, "fsck", "--no-dangling")

	// Byte-level verification against the source: one blob per round.
	for round := 0; round < 3; round++ {
		want, err := os.ReadFile(filepath.Join(src, fmt.Sprintf("r%d-b00.bin", round)))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(c, fmt.Sprintf("r%d-b00.bin", round)))
		require.NoError(t, err)
		require.Equal(t, want, got, "round %d blob round-trips byte-for-byte", round)
	}

	require.Less(t, peak.Load(), int64(32<<20),
		"peak heap must stay below any ladder outcome (resident bodies alone exceed 32 MiB)")
	t.Logf("streaming unshallow: 54 MiB across 3 packs under the 64 MiB soft limit, peak heap %.1f MiB",
		float64(peak.Load())/(1<<20))
}
