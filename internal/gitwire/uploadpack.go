package gitwire

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// serveUploadPack serves the fetch/clone data phase (docs/03 §3).
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

	br := bufio.NewReaderSize(r.Body, 64*1024)
	out := newBufWriter(w)

	var wants []model.OID
	var haves []model.OID
	var clientShallow []model.OID // client's current shallow boundary (reported by deepen/unshallow requests)
	var filter *FilterSpec        // active partial-clone filter (blob:none / tree:0, docs/03 §4)
	clientOfsDelta := false       // client accepted OFS_DELTA (v1 want-line capability)
	depth := 0                    // deepen N; 0 = no deepen
	done := false
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
	for !done {
		line, _, err := ReadPacketText(br)
		if err != nil {
			if len(wants) == 0 && len(haves) == 0 {
				http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
				return
			}
			if depth > 0 {
				s.writeShallowSection(out, vr.Repo, wants, clientShallow, depth)
				_ = out.Flush()
				return
			}
			// Negotiation round (request body ended, no done) — mirrors the basic
			// mode of upload-pack.c get_common_commits: with a common commit the ACK is terminal,
			// so the pack must go in the same response (the client reads the pack right after a bare ACK);
			// with no common commit, NAK + flush-pkt ends this round and the client continues with the next POST.
			if common := s.firstKnown(r.Context(), vr.Repo, haves); !common.IsZero() {
				_ = WritePacket(out, []byte("ACK "+string(common)+"\n"))
				break // terminal ACK, go straight to the pack phase
			}
			_ = WritePacket(out, []byte("NAK\n"))
			_ = WriteFlush(out)
			_ = out.Flush()
			return
		}
		switch {
		case strings.HasPrefix(line, "want "):
			fields := strings.Fields(strings.TrimPrefix(line, "want "))
			if len(fields) == 0 {
				http.Error(w, "malformed want", http.StatusBadRequest)
				return
			}
			wants = append(wants, model.OID(strings.SplitN(fields[0], "\x00", 2)[0]))
			// Capabilities trail the first want (gitprotocol-pack); OFS_DELTA
			// packs may only go to clients that asked for it.
			if strings.Contains(line, " ofs-delta") {
				clientOfsDelta = true
			}
		case strings.HasPrefix(line, "have "):
			haves = append(haves, model.OID(strings.TrimPrefix(line, "have ")))
		case strings.HasPrefix(line, "shallow "):
			clientShallow = append(clientShallow, model.OID(strings.TrimSpace(strings.TrimPrefix(line, "shallow "))))
		case strings.HasPrefix(line, "deepen "):
			if n, derr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "deepen "))); derr == nil && n > 0 {
				depth = n // ultra-deep values like 2147483647 (--unshallow) naturally cover full history
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
				_ = WritePacket(out, []byte("ERR filter: "+why.Error()+"\n"))
				_ = out.Flush()
				return
			}
			filter = fs
		case line == "done":
			done = true
		}
	}

	if !done && len(wants) > 0 && len(haves) > 0 {
		// The terminal ACK was already written in the negotiation round (break path); only the done-round terminal line is added here.
	}

	if done {
		if depth > 0 {
			s.writeShallowSection(out, vr.Repo, wants, clientShallow, depth)
		}
		if common := s.firstKnown(r.Context(), vr.Repo, haves); !common.IsZero() {
			_ = WritePacket(out, []byte("ACK "+string(common)+"\n"))
		} else {
			_ = WritePacket(out, []byte("NAK\n"))
		}
	}

	if len(wants) == 0 {
		_ = out.Flush()
		return
	}

	// allow-reachable-sha1-in-want (advertised): wants that are not themselves
	// advertised refs must be reachable from the advertised tips — partial-clone
	// lazy fetches name missing blobs/trees by OID and rely on exactly this
	// (upload-pack.c "not our ref" is the rejection git clients expect).
	if bad := s.unadvertisedWant(r.Context(), vr, wants); !bad.IsZero() {
		_ = WritePacket(out, []byte("ERR upload-pack: not our ref "+string(bad)+"\n"))
		_ = out.Flush()
		return
	}

	closureStart := time.Now()
	blobs, trees, commits, tags, excluded, err := s.closure(r.Context(), vr, wants, depth, clientShallow, haves, filter)
	closureDur := time.Since(closureStart)
	if err != nil {
		log.Printf("upload-pack %s%s: closure: %v", vr.Repo, vr.Sub, err)
		// Headers are already written; finish with an empty response. The client will report missing objects.
		_ = out.Flush()
		return
	}
	encodeStart := time.Now()
	// Hot-path whole-pack direct serve (docs/02 §5): when the closure exactly equals a stored pack,
	// stream the pack's raw bytes back, skipping re-hash + re-compression.
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
	served := "encode"
	if packOID, ok := s.Objects.ExactPackHit(vr.Repo, wanted); ok {
		if raw, err := s.Objects.PackBytes(r.Context(), vr.Repo, packOID); err == nil {
			if _, werr := out.Write(raw); werr == nil {
				served = "exact-pack " + string(packOID)
			}
		}
	}
	if served == "encode" {
		if err := serveObjectPack(out, blobs, trees, commits, tags, clientOfsDelta); err != nil {
			log.Printf("upload-pack %s%s: encode: %v", vr.Repo, vr.Sub, err)
		}
	}
	_ = out.Flush()
	log.Printf("upload-pack %s%s: %d wants → %d commits, %d trees, %d blobs (have-subtracted %d, filter %s, closure %s, serve %s/%s)",
		vr.Repo, vr.Sub, len(wants), len(commits), len(trees), len(blobs), excluded, filterSpecName(filter),
		closureDur.Round(time.Millisecond), served, time.Since(encodeStart).Round(time.Millisecond))
}

// serveDeltaMinBytes is the raw-payload threshold at which the online serve
// path switches from delta-free to delta encoding (docs/12 P1-4/B5): below
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
// expand along all entries, blobs are just collected (docs/01 §5 — root repo and subtree repo share one
// traversal path: a projection commit's tree is the subtree, parents already rewritten at projection time).
// Pruning: depth > 0 prunes parent chains at the shallowing boundary; with no deepen but a client-reported
// shallow boundary (plain fetch from a shallow clone) the client boundary is the prune point — matching the
// reference server's have-subtracted pack shape (objects below the graft layer stay out of the pack).
//
// Partial-clone filtering (docs/03 §4): blob:none drops blobs, tree:0 drops
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
	blobs map[model.OID][]byte, trees map[model.OID][]byte, commits map[model.OID][]byte, tags map[model.OID][]byte, excludedObjects int, err error) {
	blobs = map[model.OID][]byte{}
	trees = map[model.OID][]byte{}
	commits = map[model.OID][]byte{}
	tags = map[model.OID][]byte{}
	exclude := s.haveClosure(ctx, vr.Repo, haves, clientShallow)
	wantSet := map[model.OID]bool{}
	for _, w := range wants {
		delete(exclude, w)
		wantSet[w] = true
	}
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
			return nil, nil, nil, nil, 0, fmt.Errorf("closure: %w (object %s, referred by %s [%s])", err, oid, referrer[oid], refDump)
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
			// Go through the commit-graph index: commits are no longer decoded one by one (docs/04).
			ci, err := s.Objects.CommitInfo(vr.Repo, oid)
			if err != nil {
				return nil, nil, nil, nil, 0, err
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
				return nil, nil, nil, nil, 0, fmt.Errorf("closure: tree %s: %w", oid, err)
			}
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
				return nil, nil, nil, nil, 0, terr
			}
			enq(tg.Object)
		default:
			return nil, nil, nil, nil, 0, fmt.Errorf("closure: unsupported object type %q for %s", t, oid)
		}
	}
	return blobs, trees, commits, tags, len(exclude), nil
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
// zero value when every have in the batch is unknown.
func (s *Server) firstKnown(ctx context.Context, repo string, haves []model.OID) model.OID {
	for _, oid := range haves {
		if _, _, err := s.Objects.Raw(ctx, repo, oid); err == nil {
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
func (s *Server) unadvertisedWant(ctx context.Context, vr VirtualRepo, wants []model.OID) model.OID {
	refs, err := s.ResolveRefs(ctx, vr)
	if err != nil {
		return "" // cannot advertise → cannot judge; the closure will fail anyway
	}
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
