package gitwire

import (
	"expvar"
	"net/http"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/metrics"
)

// Process-wide serve-path counters, published via expvar (ops plane
// /debug/vars, docs/gateway/service-api.md §6 / docs/operations/performance.md). Package-level on purpose: expvar
// panics on duplicate names and one process — tests included — may host
// many Server instances; they share the counters, which is the honest
// accounting anyway (a process serves what it serves).
//
// The fetch counters answer the first performance question of any serving
// incident, "which rung is actually serving": the clone-shaped fast path
// streams a stored pack verbatim without computing the closure; after a
// computed closure the ladder retries the exact-pack hit, then the
// clone-shaped response cache, then online encoding; thin packs serve
// incremental fetches against client-held bases.
var (
	fetchExactPackStream = expvar.NewInt("banyan_fetch_exact_pack_stream") // clone-shaped fast path (closure skipped)
	fetchStreamed        = expvar.NewInt("banyan_fetch_streamed")          // multi-pack closure, streaming encode (closure skipped)
	fetchVerbatimPack    = expvar.NewInt("banyan_fetch_verbatim_pack")     // post-closure exact-pack hit
	fetchThin            = expvar.NewInt("banyan_fetch_thin")
	fetchClosureCache    = expvar.NewInt("banyan_fetch_closure_cache")
	fetchEncoded         = expvar.NewInt("banyan_fetch_encoded") // online encode (± response-cache store)
	pushAccepted         = expvar.NewInt("banyan_push_accepted") // pushes with ≥1 applied ref
)

// The Prometheus-facing plane (internal/metrics, served at /metrics): the
// three numbers an on-call slices first — request rate by operation and
// result, latency, and how much is in flight right now.
var (
	mGitRequests = metrics.GetCounter("banyan_git_requests_total",
		"git protocol requests by transport, operation and result",
		"transport", "op", "result")
	mGitLatency = metrics.GetHistogram("banyan_git_request_seconds",
		"git protocol request latency", "transport", "op")
	mGitInflight = metrics.GetGauge("banyan_git_inflight",
		"in-flight git protocol requests")
)

// resultClass maps the handler's response status onto the counter's result
// dimension: ok, rejected (authn/authz/guards) or error.
func resultClass(status int) string {
	switch {
	case status < 400:
		return "ok"
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusConflict:
		return "rejected"
	default:
		return "error"
	}
}

// statusWriter records the status code while passing everything through —
// including Flush, the streaming pack paths depend on it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// observeGit wraps one git-protocol endpoint dispatch: inflight gauge up
// for the duration, latency histogram, and the request counter classified
// by response status.
func observeGit(op string, handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		mGitInflight.With().Gauge().Add(1)
		start := time.Now()
		handler(sw, r)
		mGitLatency.With("transport", "http", "op", op).Observe(time.Since(start).Seconds())
		mGitRequests.With("transport", "http", "op", op, "result", resultClass(sw.status)).Inc()
		mGitInflight.With().Gauge().Add(-1)
	}
}

// observeSSHSession counts one git-over-SSH session (the transport the
// HTTP wrapper never sees): op from the exec'd service name, result ok on
// exit 0, rejected on guard denials (exit 1 paths the session core maps
// from policy), error otherwise.
func observeSSHSession(service string, code int, d time.Duration) {
	op := map[string]string{
		"git-upload-pack": "upload_pack", "git-receive-pack": "receive_pack",
	}[service]
	if op == "" {
		op = service
	}
	result := "ok"
	if code != 0 {
		result = "rejected"
	}
	mGitLatency.With("transport", "ssh", "op", op).Observe(d.Seconds())
	mGitRequests.With("transport", "ssh", "op", op, "result", result).Inc()
}

// countServeRung maps a serveClosurePack rung string onto the counters.
// Unknown rungs count as encoded — the ladder's end state.
func countServeRung(rung string) {
	switch {
	case rung == "thin" || rung == "thin-error":
		fetchThin.Add(1)
	case rung == "closure-cache":
		fetchClosureCache.Add(1)
	case strings.HasPrefix(rung, "exact-pack-stream"):
		fetchVerbatimPack.Add(1)
	default:
		fetchEncoded.Add(1)
	}
}
