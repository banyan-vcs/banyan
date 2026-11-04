package metrics

import (
	"strings"
	"sync"
	"testing"
)

// TestRenderPrometheusText pins the exposition format: HELP/TYPE headers,
// labeled series, histogram buckets (cumulative + +Inf + sum + count), and
// label-value escaping.
func TestRenderPrometheusText(t *testing.T) {
	reqs := GetCounter("banyan_test_requests_total", "test requests", "transport", "result")
	reqs.With("transport", "http", "result", "ok").Inc()
	reqs.With("transport", "http", "result", "ok").Inc()
	reqs.With("transport", "http", "result", `rej"ected`).Inc()
	reqs.With("transport", "ssh", "result", "ok").Add(7)

	inflight := GetGauge("banyan_test_inflight", "in-flight requests")
	inflight.With().Gauge().Add(3)

	lat := GetHistogram("banyan_test_seconds", "request latency", "op")
	lat.With("op", "fetch").Observe(0.03) // lands in 0.05 bucket
	lat.With("op", "fetch").Observe(7)     // lands in 10 bucket and +Inf

	out := Render()
	for _, want := range []string{
		"# HELP banyan_test_requests_total test requests",
		"# TYPE banyan_test_requests_total counter",
		`banyan_test_requests_total{result="ok",transport="http"} 2`,
		`banyan_test_requests_total{result="rej\"ected",transport="http"} 1`,
		`banyan_test_requests_total{result="ok",transport="ssh"} 7`,
		"# TYPE banyan_test_inflight gauge",
		`banyan_test_inflight 3`,
		"# TYPE banyan_test_seconds histogram",
		`banyan_test_seconds_bucket{le="+Inf",op="fetch"} 2`,
		`banyan_test_seconds_bucket{le="0.05",op="fetch"} 1`,
		`banyan_test_seconds_bucket{le="10",op="fetch"} 2`,
		`banyan_test_seconds_count{op="fetch"} 2`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
	if !strings.Contains(out, `banyan_test_seconds_sum{op="fetch"} 7.03`) {
		t.Errorf("histogram sum wrong:\n%s", out)
	}
	// Buckets must be cumulative: everything ≤10 includes the ≤0.05 one.
	if strings.Index(out, `le="0.05"`) > strings.Index(out, `le="10"`) {
		t.Error("bucket lines must be ordered small → large")
	}
}

// TestConcurrentSeriesSafety hammers one series from many goroutines — the
// registry is process-global, call sites race by design.
func TestConcurrentSeriesSafety(t *testing.T) {
	c := GetCounter("banyan_test_race_total", "race")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.With().Counter().Inc()
			}
		}()
	}
	wg.Wait()
	if got := c.With().Counter().val.Load(); got != 1600 {
		t.Fatalf("counter = %d, want 1600", got)
	}
}

// TestDeclareIdempotent re-declaring must not reset or panic (packages
// declare from init paths that may run in any order).
func TestDeclareIdempotent(t *testing.T) {
	a := GetCounter("banyan_test_redecl", "same metric")
	a.With().Counter().Inc()
	b := GetCounter("banyan_test_redecl", "same metric")
	if b.With().Counter().val.Load() != 1 {
		t.Fatal("redeclaration must reuse the collector, not reset it")
	}
}
