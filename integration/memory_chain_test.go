// memory_chain_test.go — T46: full-chain usability under a small memory
// budget (docs/03 §4 write path, §5 read path). A ~150 MiB incompressible
// repo is pushed and cloned through the real cluster wiring (git CLI →
// gateway → ScatteredStore → NodeClient → storenode → disk) while the test
// process is held to a soft GOMEMLIMIT far below the pack size. Every layer
// must work in a small window: any component that buffers the whole pack
// shows up as a heap peak at or above the pack size and fails the ceiling.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	memLimitBytes = 64 << 20  // soft GOMEMLIMIT for the whole in-process chain
	heapCeiling   = 110 << 20 // streaming peak must stay well under the pack
	bigContentMin = 150 << 20 // repo content floor: ~2.3× the memory limit
	gitBudget     = 8 * time.Minute
)

// heapPeak samples HeapAlloc every 20 ms and keeps the maximum. The peak is
// the evidence: O(window) code stays near the limit's neighborhood, while a
// layer holding the pack reads back at ≥ pack size.
type heapPeak struct {
	v atomic.Int64
}

func (p *heapPeak) sample() int64 { return p.v.Load() }

func startHeapSampler(t *testing.T) (*heapPeak, func()) {
	t.Helper()
	p := &heapPeak{}
	stopc, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var ms runtime.MemStats
		for {
			select {
			case <-stopc:
				return
			case <-tick.C:
				runtime.ReadMemStats(&ms)
				cur := int64(ms.HeapAlloc)
				for {
					old := p.v.Load()
					if cur <= old || p.v.CompareAndSwap(old, cur) {
						break
					}
				}
			}
		}
	}()
	return p, func() { close(stopc); <-done }
}

// gitTimed runs git with a hard kill: under a runaway heap the GC throttles
// everything to a crawl, and the suite needs the failure (with the sampled
// peak) rather than an indefinite hang.
func gitTimed(t *testing.T, timeout time.Duration, dir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"-c", "core.autocrlf=false", "-c", "core.compression=1"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s (memory-limit thrash suspected)\n%s", timeout, buf.String())
	}
	return buf.String(), err
}

// genBigRepo writes ~bigContentMin of incompressible data across three
// commits (60 blobs × 2.5 MiB — pack stays ~content size, no deltas) so the
// pushed pack dwarfs the memory limit.
func genBigRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", ".")
	rnd := rand.New(rand.NewSource(46))
	blob := make([]byte, 2560<<10)
	total := 0
	for c := 1; total < bigContentMin; c++ {
		for f := 0; f < 20 && total < bigContentMin; f++ {
			rnd.Read(blob)
			write(t, filepath.Join(dir, "data", fmt.Sprintf("f%02d.bin", f)), string(blob))
			total += len(blob)
		}
		git(t, dir, "-c", "core.compression=1", "add", "-A")
		git(t, dir, "commit", "-qm", fmt.Sprintf("big %d", c))
	}
	return dir
}

// nodePackBytes sums the pack files on every node's disk.
func nodePackBytes(t *testing.T, c *haCluster) int64 {
	t.Helper()
	var total int64
	for id, root := range c.Roots {
		require.NoError(t, filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && filepath.Ext(d.Name()) == ".pack" {
				info, ierr := d.Info()
				require.NoError(t, ierr, "node %s", id)
				total += info.Size()
			}
			return err
		}))
	}
	return total
}

func TestMemoryChainSmallBudget(t *testing.T) {
	needGit(t)
	if v := os.Getenv("BANYAN_MEMCHAIN"); v == "off" {
		t.Skip("BANYAN_MEMCHAIN=off")
	}

	c := startHACluster(t, 3)

	prev := debug.SetMemoryLimit(-1)
	debug.SetMemoryLimit(memLimitBytes)
	defer debug.SetMemoryLimit(prev)

	if dir := os.Getenv("BANYAN_HEAPPROF"); dir != "" {
		t.Cleanup(func() {
			for _, name := range []string{"heap", "allocs"} {
				f, ferr := os.Create(filepath.Join(dir, "mem-"+name+".prof"))
				if ferr != nil {
					t.Logf("profile %s: %v", name, ferr)
					continue
				}
				if ferr := pprof.Lookup(name).WriteTo(f, 0); ferr != nil {
					t.Logf("profile %s: %v", name, ferr)
				}
				f.Close()
			}
		})
	}

	peak, stopSampler := startHeapSampler(t)
	mib := func(v int64) string { return fmt.Sprintf("%.1f MiB", float64(v)/(1<<20)) }

	// Push: git → gateway spool (64 KiB window) → metadata scan → streaming
	// placement to three nodes. The heap must never approach the pack size.
	src := genBigRepo(t)
	pushStart := time.Now()
	out, err := gitTimed(t, gitBudget, src, "push", "-q", c.GW.URL+"/r/big.git", "main")
	require.NoError(t, err, "push 失败\n%s", out)
	pushPeak := peak.sample()
	t.Logf("T46 push: heap peak %s (limit %s, %s wall)",
		mib(pushPeak), mib(memLimitBytes), time.Since(pushStart).Round(time.Millisecond))

	// The pack really is big (guards the test against silently testing
	// something small) and really replicated to all three nodes.
	packBytes := nodePackBytes(t, c)
	require.Greater(t, packBytes, int64(bigContentMin), "节点落盘包过小")
	t.Logf("T46 storage: %s across 3 nodes", mib(packBytes))

	// Clone (protocol v2): ls-refs weave + enumeration-only closure walk +
	// verbatim pack stream back through the gateway. Reset the peak so the
	// two directions report independently.
	peak.v.Store(0)
	dst := t.TempDir()
	cloneStart := time.Now()
	out, err = gitTimed(t, gitBudget, "", "clone", "-q", c.GW.URL+"/r/big.git", dst)
	require.NoError(t, err, "clone 失败\n%s", out)
	clonePeak := peak.sample()
	t.Logf("T46 clone v2: heap peak %s (%s wall)", mib(clonePeak), time.Since(cloneStart).Round(time.Millisecond))

	// Correctness: same head, same object count, same bytes.
	require.Equal(t, headOf(t, src), headOf(t, dst))
	require.Equal(t, countInPack(t, src), countInPack(t, dst))
	for _, f := range []string{"data/f00.bin", "data/f19.bin"} {
		a, rerr := os.ReadFile(filepath.Join(src, f))
		require.NoError(t, rerr)
		b, rerr := os.ReadFile(filepath.Join(dst, f))
		require.NoError(t, rerr)
		require.True(t, bytes.Equal(a, b), "working tree bytes differ: %s", f)
	}

	// Same clone over protocol v0 (the SSH/legacy framing path).
	peak.v.Store(0)
	dst2 := t.TempDir()
	out, err = gitTimed(t, gitBudget, "", "-c", "protocol.version=0", "clone", "-q", c.GW.URL+"/r/big.git", dst2)
	require.NoError(t, err, "clone v0 失败\n%s", out)
	t.Logf("T46 clone v0: heap peak %s", mib(peak.sample()))
	require.Equal(t, headOf(t, src), headOf(t, dst2))

	require.Less(t, pushPeak, int64(heapCeiling),
		"push 峰值 %s 超过天花板 %s — 链路上有环节整包驻内存", mib(pushPeak), mib(heapCeiling))
	require.Less(t, clonePeak, int64(heapCeiling),
		"clone 峰值 %s 超过天花板 %s — 链路上有环节整包驻内存", mib(clonePeak), mib(heapCeiling))
	stopSampler()
}
