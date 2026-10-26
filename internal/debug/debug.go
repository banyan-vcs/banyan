// Package debug serves banyan's ops plane: the performance-tracing
// endpoints (the pprof family, a runtime snapshot, expvar counters) on a
// dedicated listener. It is deliberately OFF the public git/API port and
// off by default — profiles and serve counters describe repository
// contents and work patterns, so exposure is a deployment decision
// (config debug.pprof_addr, docs/05 §6).
package debug

import (
	"encoding/json"
	"expvar"
	"log"
	"net/http"
	"net/http/pprof"
	"runtime"
	rdebug "runtime/debug"
	"time"
)

var startedAt = time.Now()

// Options mirrors the config debug section.
type Options struct {
	// Addr enables the plane: an extra HTTP listener serving /debug/*.
	// Empty keeps everything off.
	Addr string
	// MutexProfileFraction feeds runtime.SetMutexProfileFraction (0 = off).
	MutexProfileFraction int
	// BlockProfileRateNs feeds runtime.SetBlockProfileRate in nanoseconds
	// (0 = off). Rates below ~10µs measurably slow the runtime — opt in.
	BlockProfileRateNs int
}

// Start brings the ops plane up when configured: the profile rates first
// (they are process-wide), then the listener. A failed listen logs and
// leaves the main role running — the ops plane is auxiliary, never a
// startup dependency.
func Start(o Options) {
	if o.Addr == "" {
		return
	}
	if o.MutexProfileFraction > 0 {
		runtime.SetMutexProfileFraction(o.MutexProfileFraction)
	}
	if o.BlockProfileRateNs > 0 {
		runtime.SetBlockProfileRate(o.BlockProfileRateNs)
	}
	go func() {
		log.Printf("ops: debug plane on http://%s/debug/pprof/ (+ /debug/stats, /debug/vars)", o.Addr)
		if err := http.ListenAndServe(o.Addr, Handler()); err != nil {
			log.Printf("ops: debug plane %s: %v (continuing without it)", o.Addr, err)
		}
	}()
}

// Handler assembles the ops-plane routes: the pprof family (heap, allocs,
// goroutine, block, mutex, threadcreate, profile, trace, cmdline, symbol),
// expvar at /debug/vars (includes the gitwire serve-path counters), and a
// one-shot runtime snapshot at /debug/stats.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/vars", expvar.Handler())
	mux.HandleFunc("/debug/stats", stats)
	return mux
}

// stats writes a point-in-time runtime snapshot: the numbers the
// memory-chain discipline cares about (T46/T50) without pulling a profile.
func stats(w http.ResponseWriter, _ *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	threads, _ := runtime.ThreadCreateProfile(nil)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"uptime_sec":       int(time.Since(startedAt).Seconds()),
		"go_version":       runtime.Version(),
		"gomaxprocs":       runtime.GOMAXPROCS(0),
		"goroutines":       runtime.NumGoroutine(),
		"os_threads":       threads,
		"gomemlimit_bytes": rdebug.SetMemoryLimit(-1), // read, not set
		"mem": map[string]uint64{
			"heap_alloc_bytes":  ms.HeapAlloc,
			"heap_inuse_bytes":  ms.HeapInuse,
			"stack_sys_bytes":   ms.StackSys,
			"sys_bytes":         ms.Sys,
			"total_alloc_bytes": ms.TotalAlloc,
			"mallocs":           ms.Mallocs,
			"frees":             ms.Frees,
			"num_gc":            uint64(ms.NumGC),
			"gc_pause_total_ns": ms.PauseTotalNs,
		},
	})
}
