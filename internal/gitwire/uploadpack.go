package gitwire

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// fetchReq accumulates one upload-pack request (shared by the stateless
// HTTP loop and the stateful SSH loop).
type fetchReq struct {
	wants          []model.OID
	haves          []model.OID
	clientShallow  []model.OID // client's current shallow boundary (reported by deepen/unshallow requests)
	filter         *FilterSpec // active partial-clone filter (blob:none / tree:0, docs/04 §4)
	clientOfsDelta bool        // client accepted OFS_DELTA (v1 want-line capability)
	clientThin     bool        // client asked for a thin pack (v1 cap / v2 arg)
	depth          int         // deepen N; 0 = no deepen
	done           bool
}

// applyFetchLine folds one request pkt-line into st. Returns a wire error
// message ("" = ok) and whether the transport should surface it as an ERR
// pkt (true — the filter contract, docs/04 §4) or as a protocol error
// (HTTP 400 / session failure).
func applyFetchLine(st *fetchReq, line string) (string, bool) {
	switch {
	case strings.HasPrefix(line, "want "):
		fields := strings.Fields(strings.TrimPrefix(line, "want "))
		if len(fields) == 0 {
			return "malformed want", false
		}
		st.wants = append(st.wants, model.OID(strings.SplitN(fields[0], "\x00", 2)[0]))
		// Capabilities trail the first want (gitprotocol-pack); OFS_DELTA
		// packs may only go to clients that asked for it, thin packs only
		// to clients that promised to --fix-thin.
		if strings.Contains(line, " ofs-delta") {
			st.clientOfsDelta = true
		}
		if strings.Contains(line, " thin-pack") {
			st.clientThin = true
		}
	case strings.HasPrefix(line, "have "):
		st.haves = append(st.haves, model.OID(strings.TrimPrefix(line, "have ")))
	case strings.HasPrefix(line, "shallow "):
		st.clientShallow = append(st.clientShallow, model.OID(strings.TrimSpace(strings.TrimPrefix(line, "shallow "))))
	case strings.HasPrefix(line, "deepen "):
		if n, derr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "deepen "))); derr == nil && n > 0 {
			st.depth = n // ultra-deep values like 2147483647 (--unshallow) naturally cover full history
		}
	case strings.HasPrefix(line, "filter "):
		spec := strings.TrimSpace(strings.TrimPrefix(line, "filter "))
		fs, ferr := ParseFilterSpec(spec)
		if ferr != nil || !fs.ServeSupported() {
			// Mirrors upload-pack.c: an unsupported filter is answered with
			// an ERR pkt the client surfaces as "remote error: ...".
			why := ferr
			if why == nil {
				why = fmt.Errorf("unsupported filter %q", spec)
			}
			return "filter: " + why.Error(), true
		}
		st.filter = fs
	case line == "done":
		st.done = true
	}
	return "", false
}

// serveUploadPack serves the fetch/clone data phase (docs/04 §3).
//
// v1 session (mirrors the upload-pack.c state machine):
//
//	client POST body (pkt-line stream):
//	  <want OID> [caps]           ← capabilities trail the first want
//	  ...
//	  [<have OID>...]             ← negotiation: commits the client has (MVP ignores, returns full closure)
//	  flush / done
//	server response:
//	  NAK pkt (no common commit negotiated)
//	  packfile raw bytes (side-band not advertised ⇒ client reads a bare pack)
func (s *Server) serveUploadPack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	if _, err := s.Meta.GetRepo(r.Context(), vr.Repo); err != nil {
		http.Error(w, "repository not found: "+vr.Repo, http.StatusNotFound)
		return
	}

	// Identity resolved up front (the gate itself runs in finishFetch, after
	// the request body — same wire behavior: the 401 either way makes the
	// client retry with Basic credentials).
	id, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	br := bufio.NewReaderSize(r.Body, 64*1024)
	out := newBufWriter(w)

	st := &fetchReq{}
	// Stateless HTTP negotiation (mirrors remote-curl.c / upload-pack.c get_common_commits):
	// each POST = the client writes everything once, then reads (want [+haves] [+flush] [done]).
	// The server writes only **one** response line at POST end — writing mid-stream makes the done
	// round emit a double ACK, and the client's do{ACK}while(ACK) parses PACK bytes as an ACK line
	// (bad pack header). done round = terminal ACK/NAK + pack; negotiation round =
	// ACK/NAK + flush-pkt.
	// shallow (deepen) requests are the exception (mirrors git 2.50 http-backend observed reference traffic):
	// every response starts with a shallow section — shallow lines (new boundary) + unshallow lines
	// (client-reported boundaries now completed) + flush-pkt; a negotiation round sends **only** the shallow section
	// (no NAK), and the done round follows with ACK/NAK + pack pruned at the boundary.
	for !st.done {
		line, _, err := ReadPacketText(br)
		if err != nil {
			if len(st.wants) == 0 && len(st.haves) == 0 {
				http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
				return
			}
			if st.depth > 0 {
				s.writeShallowSection(out, vr.Repo, st.wants, st.clientShallow, st.depth)
				_ = out.Flush()
				return
			}
			// Negotiation round (request body ended, no done) — mirrors the basic
			// mode of upload-pack.c get_common_commits: with a common commit the ACK is terminal,
			// so the pack must go in the same response (the client reads the pack right after a bare ACK);
			// with no common commit, NAK + flush-pkt ends this round and the client continues with the next POST.
			if common := s.firstKnown(r.Context(), vr.Repo, st.haves); !common.IsZero() {
				_ = WritePacket(out, []byte("ACK "+string(common)+"\n"))
				break // terminal ACK, go straight to the pack phase
			}
			_ = WritePacket(out, []byte("NAK\n"))
			_ = WriteFlush(out)
			_ = out.Flush()
			return
		}
		if msg, errPkt := applyFetchLine(st, line); msg != "" {
			if errPkt {
				_ = WritePacket(out, []byte("ERR "+msg+"\n"))
				_ = out.Flush()
				return
			}
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	}

	if st.done {
		if st.depth > 0 {
			s.writeShallowSection(out, vr.Repo, st.wants, st.clientShallow, st.depth)
		}
		if common := s.firstKnown(r.Context(), vr.Repo, st.haves); !common.IsZero() {
			_ = WritePacket(out, []byte("ACK "+string(common)+"\n"))
		} else {
			_ = WritePacket(out, []byte("NAK\n"))
		}
	}

	if len(st.wants) == 0 {
		_ = out.Flush()
		return
	}
	s.finishFetch(r.Context(), out, vr, id, st)
}

// finishFetch serves the pack for an accumulated request: authorization
// gates, closure, thin-pack plan and the serve ladder (docs/11 P2-2).
// Shared by the HTTP and SSH transports.
func (s *Server) finishFetch(ctx context.Context, out *bufWriter, vr VirtualRepo, id auth.Identity, st *fetchReq) {
	// Authorization gates (docs/07): identity + repository/directory fetch
	// permission, then the want-coverage check against the identity's
	// VISIBLE tips — with auth on this replaces (and strengthens) the
	// reachability gate: an oid reachable only through someone else's
	// personal branch is not fetchable even by raw OID.
	if s.Auth != nil {
		if !s.Auth.CanFetch(id, vr.Repo, string(vr.Sub)) {
			_ = WritePacket(out, []byte("ERR upload-pack: permission denied\n"))
			_ = out.Flush()
			return
		}
		if bad := s.wantsBeyondTips(ctx, vr, st.wants, id); !bad.IsZero() {
			_ = WritePacket(out, []byte("ERR upload-pack: not our ref "+string(bad)+"\n"))
			_ = out.Flush()
			return
		}
	} else if bad := s.unadvertisedWant(ctx, vr, st.wants); !bad.IsZero() {
		// allow-reachable-sha1-in-want (advertised): wants that are not
		// themselves advertised refs must be reachable from the advertised
		// tips — partial-clone lazy fetches name missing blobs/trees by OID
		// and rely on exactly this ("not our ref" is what git expects).
		_ = WritePacket(out, []byte("ERR upload-pack: not our ref "+string(bad)+"\n"))
		_ = out.Flush()
		return
	}

	// Clone-shaped fast path (docs/03 §5): no haves, depth, filter or
	// shallow — the client wants a whole history. When one stored pack
	// holds exactly that object set, stream it verbatim without building
	// the closure: multi-hundred-MB repos serve in a small memory window.
	if len(st.haves) == 0 && st.depth == 0 && st.filter == nil && len(st.clientShallow) == 0 {
		if served, ok := s.tryExactPackStream(ctx, out, vr, st.wants); ok {
			_ = out.Flush()
			log.Printf("upload-pack %s%s: %d wants → %s (closure skipped)", vr.Repo, vr.Sub, len(st.wants), served)
			return
		}
	}

	closureStart := time.Now()
	blobs, trees, commits, tags, exclude, err := s.closure(ctx, vr, st.wants, st.depth, st.clientShallow, st.haves, st.filter)
	closureDur := time.Since(closureStart)
	if err != nil {
		log.Printf("upload-pack %s%s: closure: %v", vr.Repo, vr.Sub, err)
		// The stream is live; finish with an empty response. The client will report missing objects.
		_ = out.Flush()
		return
	}
	// Thin pack (docs/11 P2-6): with haves and a thin-pack client, delta the
	// served objects against same-path predecessors the client holds.
	var plan *thinPackPlan
	if st.clientThin && len(st.haves) > 0 {
		plan = &thinPackPlan{
			bases:    s.thinBases(ctx, vr, commits, exclude),
			loadBase: func(oid model.OID) (model.ObjectType, []byte, error) { return s.Objects.Raw(ctx, vr.Repo, oid) },
		}
	}
	encodeStart := time.Now()
	// Hot-path serve ladder (docs/03 §5, docs/11 P2-2): stored-pack verbatim
	// → clone-shaped response cache → online encoding (delta past threshold).
	cacheable := len(st.haves) == 0 && st.depth == 0 && st.filter == nil
	served := s.serveClosurePack(ctx, out, vr, st.wants, blobs, trees, commits, tags, cacheable, st.clientOfsDelta, plan)
	_ = out.Flush()
	log.Printf("upload-pack %s%s: %d wants → %d commits, %d trees, %d blobs (have-subtracted %d, filter %s, closure %s, serve %s/%s)",
		vr.Repo, vr.Sub, len(st.wants), len(commits), len(trees), len(blobs), len(exclude), filterSpecName(st.filter),
		closureDur.Round(time.Millisecond), served, time.Since(encodeStart).Round(time.Millisecond))
}

// serveDeltaMinBytes is the raw-payload threshold at which the online serve
// path switches from delta-free to delta encoding (docs/11 P1-4/B5): below
// it the pack-objects window cost buys almost nothing, above it similar
// blobs/trees compress 2-5×.
const serveDeltaMinBytes = 64 << 10

// serveObjectPack writes the response pack: delta-free below the threshold,
// OFS_DELTA above it when the client accepted ofs-delta (v1 advertisement /
// v2 arg), REF_DELTA otherwise.
func serveObjectPack(w io.Writer, blobs, trees, commits, tags map[model.OID][]byte, ofsDelta bool) error {
	total := 0
	for _, m := range []map[model.OID][]byte{blobs, trees, commits, tags} {
		for _, b := range m {
			total += len(b)
		}
	}
	if total < serveDeltaMinBytes {
		return storage.EncodePack(w, blobs, trees, commits, tags)
	}
	if ofsDelta {
		return storage.EncodePackDelta(w, blobs, trees, commits, tags)
	}
	return storage.EncodePackDeltaRef(w, blobs, trees, commits, tags)
}

func filterSpecName(f *FilterSpec) string {
	if f == nil {
		return "off"
	}
	return f.Spec()
}

// enumerateClosure walks the want closure collecting OIDs only (docs/03 §5,
// the streaming-serve probe): commits and trees are read through the
// metadata indexes (transient bytes, nothing retained beyond per-object
// metadata), blobs are resolved for type but their bytes are never kept.
// The resulting set equals what closure() yields for the same clone-shaped
// request (no haves/depth/filter/shallow) — at a fraction of the memory.
func (s *Server) enumerateClosure(ctx context.Context, vr VirtualRepo, wants []model.OID) (map[model.OID]bool, error) {
	s.Objects.Prefetch(ctx, vr.Repo, wants)
	set := make(map[model.OID]bool, 1024)
	queue := append([]model.OID(nil), wants...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if set[oid] {
			continue
		}
		set[oid] = true
		t, raw, err := s.Objects.Raw(ctx, vr.Repo, oid)
		if err != nil {
			return nil, err
		}
		switch t {
		case model.CommitType:
			ci, err := s.Objects.CommitInfo(vr.Repo, oid)
			if err != nil {
				return nil, err
			}
			queue = append(queue, ci.Tree)
			queue = append(queue, ci.Parents...)
		case model.TreeType:
			entries, err := s.Objects.TreeEntries(vr.Repo, oid)
			if err != nil {
				return nil, err
			}
			children := make([]model.OID, 0, len(entries))
			for _, e := range entries {
				children = append(children, e.Target)
			}
			s.Objects.Prefetch(ctx, vr.Repo, children)
			queue = append(queue, children...)
		case model.TagType:
			tg, terr := model.DecodeTag(raw)
			if terr != nil {
				return nil, terr
			}
			queue = append(queue, tg.Object)
		case model.BlobType: // leaf: bytes discarded, only the OID matters
		}
	}
	return set, nil
}

// tryExactPackStream is the clone-shaped fast path (docs/03 §5): enumerate
// the wanted set (OIDs only), probe for a stored pack with exactly that
// object set, and stream the pack verbatim — node disk → network →
// response in one small window, the closure's object bytes never resident
// in gateway memory. Any miss before the first byte falls back to the full
// closure path; a break MID-copy is terminal (bytes are already on the
// wire, same as any dropped response).
func (s *Server) tryExactPackStream(ctx context.Context, out io.Writer, vr VirtualRepo, wants []model.OID) (string, bool) {
	oids, err := s.enumerateClosure(ctx, vr, wants)
	if err != nil {
		return "", false
	}
	packOID, hit := s.Objects.ExactPackHit(vr.Repo, oids)
	if !hit {
		return "", false
	}
	rc, _, oerr := s.Objects.OpenPackStream(ctx, vr.Repo, packOID)
	if oerr != nil {
		return "", false // before the first byte: the closure path still can
	}
	defer rc.Close()
	buf := make([]byte, 256<<10)
	if _, cerr := io.CopyBuffer(out, rc, buf); cerr != nil {
		log.Printf("upload-pack %s%s: exact-pack stream %s broke mid-copy: %v", vr.Repo, vr.Sub, packOID, cerr)
		return "exact-pack-stream " + string(packOID) + " (broken)", true
	}
	return "exact-pack-stream " + string(packOID), true
}

// serveClosurePack writes the response pack for a computed closure:
//
//	thin (client-held bases, only when requested) → whole-pack verbatim →
//	clone-shaped response cache (docs/11 P2-2) → online encoding.
//
// Thin outranks the verbatim serve deliberately: a stored pack is
// self-contained (receive advertises no-thin), so its objects are whole —
// an incremental fetch's changed file carries its full bytes even though
// the client holds the previous version at the same path. The client asked
// for minimal bytes and promised to --fix-thin; give it the delta.
func (s *Server) serveClosurePack(ctx context.Context, out io.Writer, vr VirtualRepo, wants []model.OID,
	blobs, trees, commits, tags map[model.OID][]byte, cacheable, ofsDelta bool, thin *thinPackPlan) string {

	if thin != nil && len(thin.bases) > 0 {
		if err := storage.EncodePackThin(out, s.Objects.Algo, blobs, trees, commits, tags, thin.bases, thin.loadBase); err != nil {
			log.Printf("upload-pack %s%s: thin encode: %v", vr.Repo, vr.Sub, err)
			return "thin-error"
		}
		return "thin"
	}
	wanted := make(map[model.OID]bool, len(commits)+len(trees)+len(blobs)+len(tags))
	for oid := range commits {
		wanted[oid] = true
	}
	for oid := range trees {
		wanted[oid] = true
	}
	for oid := range blobs {
		wanted[oid] = true
	}
	for oid := range tags {
		wanted[oid] = true
	}
	if packOID, ok := s.Objects.ExactPackHit(vr.Repo, wanted); ok {
		if rc, _, oerr := s.Objects.OpenPackStream(ctx, vr.Repo, packOID); oerr == nil {
			buf := make([]byte, 256<<10)
			n, cerr := io.CopyBuffer(out, rc, buf)
			rc.Close()
			if cerr == nil {
				return "exact-pack-stream " + string(packOID)
			}
			if n > 0 {
				// Bytes are already on the wire: terminal, like any dropped
				// response — falling through would append a second pack.
				log.Printf("upload-pack %s%s: exact-pack %s broke mid-copy: %v", vr.Repo, vr.Sub, packOID, cerr)
				return "exact-pack-stream " + string(packOID) + " (broken)"
			}
			// Nothing written yet: the ladder below still can serve.
		}
	}
	if cacheable {
		if raw, ok := s.Objects.CachedResponsePack(vr.Repo, vr.Sub, wants, ofsDelta); ok {
			if _, werr := out.Write(raw); werr == nil {
				return "closure-cache"
			}
		}
		// Oversized closures skip the cache by construction — serve them
		// straight off the encoder instead of parking a whole-response
		// buffer that would only be thrown away.
		total := 0
		for _, m := range []map[model.OID][]byte{blobs, trees, commits, tags} {
			for _, b := range m {
				total += len(b)
			}
		}
		if total > storage.ResponsePackCacheMax {
			if err := serveObjectPack(out, blobs, trees, commits, tags, ofsDelta); err != nil {
				log.Printf("upload-pack %s%s: encode: %v", vr.Repo, vr.Sub, err)
			}
			return "encode"
		}
		var buf bytes.Buffer
		if err := serveObjectPack(&buf, blobs, trees, commits, tags, ofsDelta); err != nil {
			log.Printf("upload-pack %s%s: encode: %v", vr.Repo, vr.Sub, err)
		}
		s.Objects.StoreResponsePack(vr.Repo, vr.Sub, wants, ofsDelta, buf.Bytes())
		_, _ = out.Write(buf.Bytes())
		return "encode+cache"
	}
	if err := serveObjectPack(out, blobs, trees, commits, tags, ofsDelta); err != nil {
		log.Printf("upload-pack %s%s: encode: %v", vr.Repo, vr.Sub, err)
	}
	return "encode"
}

// writeShallowSection writes the shallow section at the start of the response (deepen requests only):
// the shallowSectionLines content as no-LF pkt lines (v1 reference-traffic
// bytes) + flush-pkt terminator (empty boundary = bare flush, per reference traffic d99).
func (s *Server) writeShallowSection(out io.Writer, repo string, wants, clientShallow []model.OID, depth int) {
	for _, line := range s.shallowSectionLines(repo, wants, clientShallow, depth) {
		_ = WritePacket(out, []byte(line))
	}
	_ = WriteFlush(out)
}

func sortedOIDs(set map[model.OID]bool) []model.OID {
	out := make([]model.OID, 0, len(set))
	for oid := range set {
		out = append(out, oid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// shallowCut BFS from wants along all parent edges, including depth levels 0..depth-1
// (2147483647 for --unshallow, or depths beyond history, naturally converge to full).
// Returns (included set, boundary set); boundary = commits included that have an unincluded parent —
// those commits' parent chains stay out of the pack, and the client grafts via .git/shallow.
func (s *Server) shallowCut(repo string, wants []model.OID, depth int) (included, boundary map[model.OID]bool) {
	included = map[model.OID]bool{}
	boundary = map[model.OID]bool{}
	parents := map[model.OID][]model.OID{}
	type node struct {
		oid model.OID
		d   int
	}
	queue := make([]node, 0, len(wants))
	for _, w := range wants {
		queue = append(queue, node{s.peelToCommit(repo, w), 0})
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.d >= depth || included[n.oid] {
			continue
		}
		included[n.oid] = true
		ci, err := s.Objects.CommitInfo(repo, n.oid)
		if err != nil {
			continue // non-commit object (defensive): does not affect the remaining levels
		}
		parents[n.oid] = ci.Parents
		for _, p := range ci.Parents {
			queue = append(queue, node{p, n.d + 1})
		}
	}
	for oid := range included {
		for _, p := range parents[oid] {
			if !included[p] {
				boundary[oid] = true
				break
			}
		}
	}
	return included, boundary
}

// closure collects the object closure from wants: commits expand along all parents, trees
// expand along all entries, blobs are just collected (docs/02 §5 — root repo and subtree repo share one
// traversal path: a projection commit's tree is the subtree, parents already rewritten at projection time).
// Pruning: depth > 0 prunes parent chains at the shallowing boundary; with no deepen but a client-reported
// shallow boundary (plain fetch from a shallow clone) the client boundary is the prune point — matching the
// reference server's have-subtracted pack shape (objects below the graft layer stay out of the pack).
//
// Partial-clone filtering (docs/04 §4): blob:none drops blobs, tree:0 drops
// trees and blobs; dropped trees are not expanded (their contents cannot
// survive anyway). Explicit wants are exempt — lazy-fetch requests name the
// missing object directly and must receive it.
//
// Have subtraction (git upload-pack.c mark-uninteresting semantics, docs/09 §3):
// the client's haves bound the set of objects it provably already has (a git
// repo holding commit X holds X's full closure); every object reachable from
// the known haves' commits is therefore excluded from the pack, and the main
// walk never visits an excluded object. Excluded objects are not expanded —
// their children are reachable only through excluded paths unless some other
// included path reaches them, in which case that path enqueues them. The
// wants themselves are never excluded (a degenerate client that wants
// something it also has must still receive it). Server cost: one extra
// traversal of the have side per fetch — the same trade git makes without
// reachability bitmaps (docs/09 §3.3).
func (s *Server) closure(ctx context.Context, vr VirtualRepo, wants []model.OID, depth int, clientShallow, haves []model.OID, filter *FilterSpec) (
	blobs map[model.OID][]byte, trees map[model.OID][]byte, commits map[model.OID][]byte, tags map[model.OID][]byte, exclude map[model.OID]bool, err error) {
	blobs = map[model.OID][]byte{}
	trees = map[model.OID][]byte{}
	commits = map[model.OID][]byte{}
	tags = map[model.OID][]byte{}
	exclude = s.haveClosure(ctx, vr.Repo, haves, clientShallow)
	wantSet := map[model.OID]bool{}
	for _, w := range wants {
		delete(exclude, w)
		wantSet[w] = true
	}
	// Batch-locate the walk's known inputs up front (docs/11 P0-1): wants +
	// haves in one Meta round trip instead of one per object.
	s.Objects.Prefetch(ctx, vr.Repo, append(append([]model.OID{}, wants...), haves...))
	visited := map[model.OID]bool{}
	referrer := map[model.OID]model.OID{} // debugging: oid ← who enqueued it
	var boundary map[model.OID]bool
	if depth > 0 {
		_, boundary = s.shallowCut(vr.Repo, wants, depth)
	} else if len(clientShallow) > 0 {
		boundary = map[model.OID]bool{}
		for _, oid := range clientShallow {
			boundary[oid] = true
		}
	}

	queue := append([]model.OID(nil), wants...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if visited[oid] {
			continue
		}
		visited[oid] = true

		if exclude[oid] {
			// The client provably has this object (reachable from its haves,
			// bounded by its reported shallow boundaries). A non-shallow
			// client also has its whole ancestry, so the walk prunes here;
			// a shallow client's history is grafted at the boundary — what
			// sits BELOW an excluded commit may still be wanted (deepen /
			// unshallow), so the walk traverses THROUGH it, emitting nothing
			// and respecting the depth boundary cut.
			if len(clientShallow) > 0 && !boundary[oid] {
				if ci, err := s.Objects.CommitInfo(vr.Repo, oid); err == nil {
					queue = append(queue, ci.Parents...)
				}
			}
			continue
		}

		t, raw, err := s.Objects.Raw(ctx, vr.Repo, oid)
		if err != nil {
			// Debugging: dump the referrer context (type + first 256 bytes of raw content).
			refType, refRaw, rerr := s.Objects.Raw(ctx, vr.Repo, referrer[oid])
			refDump := fmt.Sprintf("type=%s rerr=%v raw=%.256x", refType, rerr, refRaw)
			return nil, nil, nil, nil, nil, fmt.Errorf("closure: %w (object %s, referred by %s [%s])", err, oid, referrer[oid], refDump)
		}
		enq := func(child model.OID) {
			if _, seen := referrer[child]; !seen {
				referrer[child] = oid
			}
			queue = append(queue, child)
		}
		switch t {
		case model.CommitType:
			commits[oid] = raw
			// Go through the commit-graph index: commits are no longer decoded one by one (docs/08).
			ci, err := s.Objects.CommitInfo(vr.Repo, oid)
			if err != nil {
				return nil, nil, nil, nil, nil, err
			}
			enq(ci.Tree)
			if !boundary[oid] {
				for _, p := range ci.Parents {
					enq(p)
				}
			}
		case model.TreeType:
			if filterDrop(filter, wantSet, oid, model.TreeType) {
				continue // tree:0 — trees are dropped unexpanded
			}
			trees[oid] = raw
			// Go through the tree-entry index: path/child expansion no longer re-parses tree bytes.
			entries, err := s.Objects.TreeEntries(vr.Repo, oid)
			if err != nil {
				return nil, nil, nil, nil, nil, fmt.Errorf("closure: tree %s: %w", oid, err)
			}
			// Tree expansion is the natural batching point (docs/11 P0-1):
			// all children locate in one Meta round trip, so cold-walk RPC
			// count falls from O(objects) to O(trees).
			children := make([]model.OID, 0, len(entries))
			for _, e := range entries {
				children = append(children, e.Target)
			}
			s.Objects.Prefetch(ctx, vr.Repo, children)
			for _, e := range entries {
				enq(e.Target)
			}
		case model.BlobType:
			if filterDrop(filter, wantSet, oid, model.BlobType) {
				continue // blob:none — omitted objects are the promisor's to lazy-fetch
			}
			blobs[oid] = raw
		case model.TagType:
			// Annotated tag: serve the tag object and walk into its target
			// (git peels tags when computing the closure).
			tags[oid] = raw
			tg, terr := model.DecodeTag(raw)
			if terr != nil {
				return nil, nil, nil, nil, nil, terr
			}
			enq(tg.Object)
		default:
			return nil, nil, nil, nil, nil, fmt.Errorf("closure: unsupported object type %q for %s", t, oid)
		}
	}
	return blobs, trees, commits, tags, exclude, nil
}

// haveClosure computes the object set provably present on a client holding
// the given haves: the commit closure of the server-known haves, expanded
// through their trees. Unknown haves are skipped.
//
// Shallow caveat: a client reporting shallow boundaries does NOT hold those
// commits' parent closures (grafted at .git/shallow), so the commit walk
// stops AT each reported boundary — the boundary commit and its tree closure
// are excluded, everything below them is not. Without this bound the
// unshallow/deepen path would exclude objects the client is precisely trying
// to receive ("remote did not send all necessary objects").
func (s *Server) haveClosure(ctx context.Context, repo string, haves, clientShallow []model.OID) map[model.OID]bool {
	exclude := map[model.OID]bool{}
	if len(haves) == 0 {
		return exclude
	}
	s.Objects.Prefetch(ctx, repo, haves) // one batched locate for the whole have list
	shallowStop := map[model.OID]bool{}
	for _, oid := range clientShallow {
		shallowStop[oid] = true
	}
	commitSeen := map[model.OID]bool{}
	queue := append([]model.OID(nil), haves...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if commitSeen[oid] {
			continue
		}
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			continue // unknown to the server: cannot anchor an exclusion
		}
		commitSeen[oid] = true
		exclude[oid] = true
		if !shallowStop[oid] {
			queue = append(queue, ci.Parents...)
		}
	}
	for oid := range commitSeen {
		ci, _ := s.Objects.CommitInfo(repo, oid)
		if ci == nil {
			continue
		}
		var walk func(tree model.OID)
		walk = func(tree model.OID) {
			if exclude[tree] {
				return
			}
			exclude[tree] = true
			entries, err := s.Objects.TreeEntries(repo, tree)
			if err != nil {
				return
			}
			for _, e := range entries {
				if e.IsDir {
					walk(e.Target)
				} else {
					exclude[e.Target] = true
				}
			}
		}
		walk(ci.Tree)
	}
	return exclude
}

// firstKnown returns the first object in haves that the server already has (the v1 negotiation
// common-commit check; a single basic ACK suffices — multi_ack not advertised). Returns the
// zero value when every have in the batch is unknown. One batched locate, no object bytes.
func (s *Server) firstKnown(ctx context.Context, repo string, haves []model.OID) model.OID {
	known := s.Objects.Known(ctx, repo, haves)
	for _, oid := range haves {
		if known[oid] {
			return oid
		}
	}
	return ""
}

// unadvertisedWant validates wants against the advertised refs (the
// allow-reachable-sha1-in-want contract): an oid that is neither an
// advertised ref tip nor reachable from one is rejected. Reachability is the
// full closure walk from the tips — the same trade git's upload-pack makes
// without reachability bitmaps.
// unadvertisedWant validates wants against all advertised refs (auth off).
func (s *Server) unadvertisedWant(ctx context.Context, vr VirtualRepo, wants []model.OID) model.OID {
	return s.wantsBeyondTips(ctx, vr, wants, auth.Identity{})
}

// wantsBeyondTips validates wants against the identity's VISIBLE view of the
// advertised refs (docs/07): an oid that is neither a visible tip nor
// reachable from one is rejected — the allow-reachable-sha1-in-want walk,
// scoped to what the caller may actually see. With auth off the filter is a
// no-op and this is the plain reachability contract; with auth on it makes
// objects reachable only through another user's personal branch
// unfetchable even by raw OID.
func (s *Server) wantsBeyondTips(ctx context.Context, vr VirtualRepo, wants []model.OID, id auth.Identity) model.OID {
	refs, err := s.ResolveRefs(ctx, vr)
	if err != nil {
		return "" // cannot advertise → cannot judge; the closure will fail anyway
	}
	refs = s.visibleRefs(id, vr.Repo, refs)
	advertised := map[model.OID]bool{}
	var tips []model.OID
	for _, ref := range refs {
		advertised[ref.OID] = true
		tips = append(tips, ref.OID)
		if peeled := s.peelToCommit(vr.Repo, ref.OID); !peeled.IsZero() && peeled != ref.OID {
			tips = append(tips, peeled) // annotated tag: its target is reachable
		}
	}
	var unknown []model.OID
	for _, w := range wants {
		if !advertised[w] {
			unknown = append(unknown, w)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	reachable := s.haveClosure(ctx, vr.Repo, tips, nil)
	for _, w := range unknown {
		if !reachable[w] {
			return w
		}
	}
	return ""
}

// peelToCommit follows annotated-tag chains (want <tag-oid> must behave like
// wanting the tagged commit for depth computation; the closure walk serves
// the tag objects themselves). Bounded against malformed tag loops.
func (s *Server) peelToCommit(repo string, oid model.OID) model.OID {
	for i := 0; i < 16; i++ {
		t, raw, err := s.Objects.Raw(context.Background(), repo, oid)
		if err != nil || t != model.TagType {
			return oid
		}
		tg, terr := model.DecodeTag(raw)
		if terr != nil {
			return oid
		}
		oid = tg.Object
	}
	return oid
}
