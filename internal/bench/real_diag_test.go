package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"
)

// TestRealSampleDiag reproduces real-sample stalls inside the test process
// and dumps every goroutine's stack when one appears (runtime.Stack, not a
// test-timeout panic — the hung connection used to swallow the deferred
// cleanup and the dump with it). Gated by BENCH_DIAG_REPO (a local repo
// path): never runs in CI.
//
//	BENCH_DIAG_REPO=F:\Workspace\git go test ./internal/bench -run TestRealSampleDiag -timeout 30m -v
//
// The hang point also writes a heap profile (heap.pprof next to the test's
// workdir log) and a per-minute MemStats heartbeat — a stall's memory owner
// should be named, not guessed (发现⑤'s 9GB took three kills to characterize).
func TestRealSampleDiag(t *testing.T) {
	src := os.Getenv("BENCH_DIAG_REPO")
	if src == "" {
		t.Skip("set BENCH_DIAG_REPO to a local repository to run")
	}
	work := t.TempDir()
	rr, err := materialize(src, work)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sample: %s branch=%s sub=%s", rr.bare, rr.branch, rr.subDir)
	srv, err := StartBanyan(filepath.Join(work, "shards"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { go srv.Stop() }() // fire-and-forget: a hung connection never blocks the dump
	url := srv.URL + "/r/seed.git"
	t.Log("pushing sample into banyan (unmeasured)…")
	start := time.Now()
	if _, err := git(rr.bare, "push", "-q", url, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"); err != nil {
		t.Fatal(err)
	}
	t.Logf("push done in %s", time.Since(start))
	if err := srv.SetDefaultBranch("seed", rr.branch); err != nil {
		t.Fatal(err)
	}
	// MemStats heartbeat: the growth curve during the stall is half the
	// diagnosis (constant vs accelerating tells leak from working set).
	stopTick := make(chan struct{})
	defer close(stopTick)
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-stopTick:
				return
			case <-tick.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				t.Logf("memstats: HeapAlloc=%dMB HeapObjects=%d Sys=%dMB NumGC=%d GCCPU=%.1f%%",
					ms.HeapAlloc>>20, ms.HeapObjects, ms.Sys>>20, ms.NumGC,
					100*float64(ms.GCCPUFraction))
			}
		}
	}()
	// The stall under study: the FIRST full clone after the push (the cold
	// lane). On a hang, dump every goroutine — the in-process server's
	// blocker and waiters are all in here.
	cmd := exec.Command("git", "-c", "core.autocrlf=false", "clone", "-q", url, filepath.Join(work, "clone"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	const hangBudget = 6 * time.Minute
	select {
	case err := <-done:
		t.Logf("clone finished in %s (err=%v) — stall not reproduced", time.Since(start), err)
	case <-time.After(hangBudget):
		_ = cmd.Process.Kill()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		t.Logf("memstats at hang: HeapAlloc=%dMB HeapObjects=%d Sys=%dMB NumGC=%d",
			ms.HeapAlloc>>20, ms.HeapObjects, ms.Sys>>20, ms.NumGC)
		if f, ferr := os.Create(filepath.Join(os.TempDir(), "banyan-diag-heap.pprof")); ferr == nil {
			if herr := pprof.Lookup("heap").WriteTo(f, 0); herr != nil {
				t.Logf("heap profile: %v", herr)
			} else {
				t.Logf("heap profile: %s", f.Name())
			}
			f.Close()
		}
		buf := make([]byte, 4<<20)
		n := runtime.Stack(buf, true)
		t.Logf("clone hung past %s — goroutine dump:\n%s", hangBudget, buf[:n])
		t.Fatal("cold clone hung")
	}
}
