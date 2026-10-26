package gitwire

import (
	"expvar"
	"strings"
)

// Process-wide serve-path counters, published via expvar (ops plane
// /debug/vars, docs/05 §6 / docs/11). Package-level on purpose: expvar
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
	fetchVerbatimPack    = expvar.NewInt("banyan_fetch_verbatim_pack")     // post-closure exact-pack hit
	fetchThin            = expvar.NewInt("banyan_fetch_thin")
	fetchClosureCache    = expvar.NewInt("banyan_fetch_closure_cache")
	fetchEncoded         = expvar.NewInt("banyan_fetch_encoded") // online encode (± response-cache store)
	pushAccepted         = expvar.NewInt("banyan_push_accepted") // pushes with ≥1 applied ref
)

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
