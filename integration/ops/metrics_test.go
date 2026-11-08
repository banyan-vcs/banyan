// metrics_test.go — the observability plane's integration pin: real git
// traffic lands in the process registry, and the /metrics endpoint serves
// it in the Prometheus text format. The registry is process-global (tests
// share it), so assertions are on deltas, never absolute values.
package ops

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/metrics"
	"github.com/banyan-vcs/banyan/internal/model"
)

// metricValue extracts one series value from a Render() snapshot; a series
// that has never been touched renders nothing and counts as 0.
func metricValue(t *testing.T, render, linePrefix string) int64 {
	t.Helper()
	for _, line := range strings.Split(render, "\n") {
		if strings.HasPrefix(line, linePrefix) {
			v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, linePrefix)), 10, 64)
			if err != nil {
				t.Fatalf("series %q: %v", line, err)
			}
			return v
		}
	}
	return 0
}

// TestMetricsObserveGitTraffic: a push and a clone through the real git
// CLI must move the request counters and the ingest-bytes gauge — the
// numbers an on-call's first dashboard panel is made of — and /metrics
// serves the exposition format.
func TestMetricsObserveGitTraffic(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	if h, ok := s.Server.Config.Handler.(*http.ServeMux); ok {
		h.Handle("/metrics", metrics.Handler()) // cmd wiring, reproduced here
	}

	pushLine := "banyan_git_requests_total{op=\"receive_pack\",result=\"ok\",transport=\"http\"} "
	fetchLine := "banyan_git_requests_total{op=\"upload_pack\",result=\"ok\",transport=\"http\"} "
	ingestLine := "banyan_ingest_bytes_total "
	before := metrics.Render()

	support.SeedRepo(t, s, "t45")
	support.CloneRepo(t, s, s.RepoURL("t45"))

	after := metrics.Render()
	if got := metricValue(t, after, pushLine) - metricValue(t, before, pushLine); got != 1 {
		t.Errorf("receive_pack delta = %d, want 1", got)
	}
	// Protocol v2 posts twice to /git-upload-pack per clone (command=ls-refs,
	// then command=fetch) — the counter is endpoint-grained, so one clone
	// moves it by exactly 2.
	if got := metricValue(t, after, fetchLine) - metricValue(t, before, fetchLine); got != 2 {
		t.Errorf("upload_pack delta = %d, want 2 (ls-refs + fetch)", got)
	}
	if got := metricValue(t, after, ingestLine) - metricValue(t, before, ingestLine); got <= 0 {
		t.Errorf("ingest bytes did not grow (delta %d)", got)
	}

	// The endpoint answers in the exposition format with our series.
	resp, err := http.Get(s.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content type %q, want text/plain exposition", ct)
	}
	for _, want := range []string{
		"# TYPE banyan_git_requests_total counter",
		"# TYPE banyan_git_request_seconds histogram",
		"banyan_git_inflight 0",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// TestMetricsCASConflictCounter: a stale-expected CAS counts as contention
// — the single-writer's contention gauge.
func TestMetricsCASConflictCounter(t *testing.T) {
	_, meta, _ := support.NewServerPlane(t)
	tl := model.Timeline{Repo: "t45cas", Anchor: model.RootAnchor, Name: "main"}
	conflictLine := "banyan_cas_conflicts_total "
	before := metricValue(t, metrics.Render(), conflictLine)

	if err := meta.CASHead(t.Context(), tl, model.OID(""), model.OID("0123456789012345678901234567890123456789")); err != nil {
		t.Fatalf("first CAS: %v", err)
	}
	// Stale expectation (head already moved): the contention shape.
	err := meta.CASHead(t.Context(), tl, model.OID(""), model.OID("1111111111111111111111111111111111111111"))
	if err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
		t.Fatalf("second CAS = %v, want non-fast-forward", err)
	}
	after := metricValue(t, metrics.Render(), conflictLine)
	if after-before != 1 {
		t.Errorf("cas conflict delta = %d, want 1", after-before)
	}
	_ = fmt.Sprint()
}
