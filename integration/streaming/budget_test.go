package streaming

// T96 (docs/operations/testing.md): the per-request fetch memory budget
// (fetch_mem_budget, docs/operations/performance.md §8.2), verified with
// real git clients. Two proofs:
//
//  1. ROUTING — a 28 MiB two-pack repo sits between the two cutoffs: under
//     the 200 MiB default the closure ladder serves it (28 < 32 MiB), and
//     squeezing the budget to 100 MiB drops the cutoff to 25 MiB so the
//     SAME repo shape takes the streaming rung instead. The ops counters
//     pin both sides; the bytes round-trip either way.
//  2. THE EXTREME — 120 MiB across three packs (above the 100 MiB budget
//     itself) served with the process held to a 100 MiB soft heap limit:
//     any O(closure) residency would blow the limit. The clone completes,
//     fsck --strict is clean, every round's blob round-trips, and the
//     sampled heap peak stays inside the budget's window.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
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

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/debug"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestFetchMemBudget(t *testing.T) {
	if os.Getenv("BANYAN_MEMCHAIN") == "off" {
		t.Skip("BANYAN_MEMCHAIN=off")
	}
	support.NeedGit(t)

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

	// seedRepo pushes rounds×per×size MiB of incompressible blobs, one
	// disjoint file set per round so every push lands off-pack — the repo
	// stays multi-pack by construction and the exact-pack rungs miss.
	seedRepo := func(t *testing.T, srv *support.Server, repo string, rounds, per, sizeMiB int) string {
		src := t.TempDir()
		support.Git(t, src, "init", "-b", "main", ".")
		support.Git(t, src, "remote", "add", "origin", srv.RepoURL(repo))
		for round := 0; round < rounds; round++ {
			for i := 0; i < per; i++ {
				blob := make([]byte, sizeMiB<<20)
				_, _ = rand.Read(blob)
				require.NoError(t, os.WriteFile(
					filepath.Join(src, fmt.Sprintf("r%d-b%02d.bin", round, i)), blob, 0o644))
			}
			support.Git(t, src, "add", ".")
			support.Git(t, src, "commit", "-m", fmt.Sprintf("round %d", round))
			support.Git(t, src, "push", "origin", "main")
		}
		return src
	}
	verifyClone := func(t *testing.T, src, clone string, rounds, per int) {
		for round := 0; round < rounds; round++ {
			want, err := os.ReadFile(filepath.Join(src, fmt.Sprintf("r%d-b00.bin", round)))
			require.NoError(t, err)
			got, err := os.ReadFile(filepath.Join(clone, fmt.Sprintf("r%d-b00.bin", round)))
			require.NoError(t, err)
			require.Equal(t, want, got, "round %d blob round-trips byte-for-byte", round)
		}
		files := strings.Fields(support.Git(t, clone, "ls-files"))
		require.Len(t, files, rounds*per, "every round's blobs present: %v", files)
		support.Git(t, clone, "fsck", "--strict")
	}

	t.Run("routing", func(t *testing.T) {
		// Default budget (200 MiB → 32 MiB cutoff): 28 MiB of packs takes
		// the closure ladder — its maps fit the budget, and its response
		// cache keeps repeat clones cheap.
		def := support.NewServer(t)
		src := seedRepo(t, def, "t96a", 2, 7, 2) // 2 packs × 14 MiB = 28 MiB
		encoded0, streamed0 := counter("banyan_fetch_encoded"), counter("banyan_fetch_streamed")
		dst := t.TempDir()
		support.Git(t, dst, "clone", def.RepoURL("t96a"), "r")
		verifyClone(t, src, filepath.Join(dst, "r"), 2, 7)
		require.Equal(t, encoded0+1, counter("banyan_fetch_encoded"),
			"default budget: 28 MiB < 32 MiB cutoff, the closure ladder serves")
		require.Equal(t, streamed0, counter("banyan_fetch_streamed"))

		// Squeezed to 100 MiB the cutoff drops to 25 MiB: the same shape
		// now STREAMS — nothing O(closure) resident, per-request memory
		// inside the tighter window.
		sqz := support.NewServerTuned(t, func(gw *gitwire.Server, objects *storage.ObjectService) {
			gw.FetchMemBudget = 100 << 20
			objects.FetchMemBudget = 100 << 20
		})
		src2 := seedRepo(t, sqz, "t96a", 2, 7, 2)
		streamed1 := counter("banyan_fetch_streamed")
		dst2 := t.TempDir()
		support.Git(t, dst2, "clone", sqz.RepoURL("t96a"), "r")
		verifyClone(t, src2, filepath.Join(dst2, "r"), 2, 7)
		require.Equal(t, streamed1+1, counter("banyan_fetch_streamed"),
			"100 MiB budget: 28 MiB ≥ 25 MiB cutoff, the streaming rung serves")
	})

	t.Run("extreme-100MiB", func(t *testing.T) {
		const budget = 100 << 20
		prev := rdebug.SetMemoryLimit(budget)
		t.Cleanup(func() { rdebug.SetMemoryLimit(prev) })

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

		// 3 packs × 40 MiB = 120 MiB of packs served under a 100 MiB
		// budget with a 100 MiB soft heap limit: completion itself proves
		// nothing O(closure) is resident — the closure maps alone would
		// exceed the limit and GC-thrash to death.
		srv := support.NewServerTuned(t, func(gw *gitwire.Server, objects *storage.ObjectService) {
			gw.FetchMemBudget = budget
			objects.FetchMemBudget = budget
		})
		src := seedRepo(t, srv, "t96b", 3, 10, 4)
		streamed0 := counter("banyan_fetch_streamed")
		dst := t.TempDir()
		support.Git(t, dst, "clone", srv.RepoURL("t96b"), "r")
		require.Equal(t, streamed0+1, counter("banyan_fetch_streamed"),
			"120 MiB multi-pack clone took the streaming rung")
		verifyClone(t, src, filepath.Join(dst, "r"), 3, 10)

		require.Less(t, peak.Load(), int64(math.Round(float64(budget)*1.10)),
			"heap peak stayed inside the budget window (+10%% GC slack)")
		t.Logf("fetch mem budget: 120 MiB across 3 packs under a 100 MiB budget/soft limit, peak heap %.1f MiB",
			float64(peak.Load())/(1<<20))
	})
}
