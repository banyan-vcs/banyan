// Protocol v2 (docs/03 §9b). Normative reference: Documentation/technical/
// protocol-v2.txt; behavioral baseline: upload-pack.c/serve.c. v2 only
// defines ls-refs and fetch — receive-pack stays on v0 forever, so the
// version=2 header is honored for upload-pack only, exactly like
// http-backend.c. Value over v1 (docs/12 P1-5): on-demand ref advertisement
// (ref-prefixes) and the sectioned fetch response; negotiation semantics are
// the same terminal-ACK-in-same-response model the v1 path implements.
package gitwire

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/model"
)

// wantsV2 reports a Git-Protocol: version=2 negotiation request. remote-curl
// sends the header on the advertisement GET and on every command POST once it
// has seen a v2 advertisement (protocol.version defaults to 2 for http since
// git 2.26).
func wantsV2(r *http.Request) bool {
	for _, tok := range strings.Fields(r.Header.Get("Git-Protocol")) {
		if tok == "version=2" {
			return true
		}
	}
	return false
}

// advertiseV2 serves the v2 capability advertisement: pkt-lines "version 2"
// then capability key[=value] lines, terminated by a flush. No "# service"
// prelude — that is v1-only and would break the v2 client's parser.
func (s *Server) advertiseV2(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	out := newBufWriter(w)
	_ = WritePacket(out, []byte("version 2\n"))
	_ = WritePacket(out, []byte("agent=banyan/0.1\n"))
	_ = WritePacket(out, []byte("ls-refs=unborn\n")) // unborn HEAD of an empty repo
	// Optional fetch capabilities we implement; the base args (thin-pack,
	// no-progress, include-tag, ofs-delta) are part of the v2 protocol itself
	// and are accepted without advertisement.
	_ = WritePacket(out, []byte("fetch=shallow filter\n"))
	objFmt := "object-format=sha1\n"
	if s.Algo == model.SHA256 {
		objFmt = "object-format=sha256\n"
	}
	_ = WritePacket(out, []byte(objFmt))
	_ = WriteFlush(out)
	_ = out.Flush()
}

// serveUploadPackV2 handles one v2 command request (stateless: one command
// per POST):
//
//	POST body:  command=<name>\n [capability lines] delim <args...> flush
//	Responses are pkt-line sections separated by delim-pkts; the packfile
//	section's payload is the RAW pack to the end of the body (spec grammar:
//	output ends at the packdata — no trailing flush after it).
func (s *Server) serveUploadPackV2(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := s.Meta.GetRepo(r.Context(), vr.Repo); err != nil {
		http.Error(w, "repository not found: "+vr.Repo, http.StatusNotFound)
		return
	}
	// One identity per command request (docs/13); every v2 command path
	// receives it.
	id, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	br := bufio.NewReaderSize(r.Body, 64*1024)
	out := newBufWriter(w)

	line, _, err := ReadPacketText(br)
	if err != nil || !strings.HasPrefix(line, "command=") {
		http.Error(w, "v2: expected command= pkt", http.StatusBadRequest)
		return
	}
	command := strings.TrimPrefix(line, "command=")

	// Capability lines until the delim (agent / object-format echoes carry no
	// new information — we advertised them), then command args until flush.
	var args []string
	inArgs := false
	for {
		payload, kind, err := ReadPacketKind(br)
		if err != nil {
			break // body ended without flush: the args read so far stand
		}
		if kind == PktDelim {
			inArgs = true
			continue
		}
		if kind == PktFlush {
			break
		}
		if inArgs {
			args = append(args, strings.TrimSuffix(string(payload), "\n"))
		}
	}

	switch command {
	case "ls-refs":
		s.lsRefsV2(out, r, vr, args, id)
	case "fetch":
		s.fetchV2(out, r, vr, args, id)
	default:
		_ = WritePacket(out, []byte("ERR unknown command "+command+"\n"))
		_ = WriteFlush(out)
	}
	_ = out.Flush()
}

// lsRefsV2 serves command=ls-refs: pkt-lines "<oid> <refname>
// [ symref-target:<target>][ peeled:<oid>]" (SP-separated attributes, per
// the spec grammar obj-id SP refname *(SP ref-attribute) LF) then a flush.
// Args: peel, symrefs, unborn, ref-prefix <p> (prefix match on the full
// refname — HEAD goes through the same filter, so `git fetch origin
// <branch>` traffic stays minimal).
func (s *Server) lsRefsV2(out *bufWriter, r *http.Request, vr VirtualRepo, args []string, id auth.Identity) {
	ctx := r.Context()
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimSpace(strings.TrimPrefix(a, "ref-prefix ")))
		}
	}
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}

	refs, err := s.ResolveRefs(ctx, vr)
	if err != nil {
		_ = WritePacket(out, []byte("ERR "+err.Error()+"\n"))
		_ = WriteFlush(out)
		return
	}
	refs = s.visibleRefs(id, vr.Repo, refs)
	if unborn && len(refs) == 0 && match("HEAD") {
		// Unborn HEAD of an empty repo (ls-refs=unborn advertisement): a
		// fresh v2 clone can configure its default branch with zero refs.
		_ = WritePacket(out, []byte("unborn HEAD symref-target:refs/heads/"+
			s.defaultBranch(ctx, vr.Repo)+"\n"))
	}
	for _, ref := range refs {
		if !match(ref.Name) {
			continue
		}
		line := string(ref.OID) + " " + ref.Name
		if symrefs && ref.Name == "HEAD" {
			line += " symref-target:refs/heads/" + s.defaultBranch(ctx, vr.Repo)
		}
		if peel {
			// peeled:<target-commit> on annotated-tag refs (ls-refs.c).
			if t, _, err := s.Objects.Raw(ctx, vr.Repo, ref.OID); err == nil && t == model.TagType {
				if c := s.peelToCommit(vr.Repo, ref.OID); !c.IsZero() && c != ref.OID {
					line += " peeled:" + string(c)
				}
			}
		}
		_ = WritePacket(out, []byte(line+"\n"))
	}
	_ = WriteFlush(out)
}

// v2FetchArgs is one parsed command=fetch request.
type v2FetchArgs struct {
	wants, haves, clientShallow []model.OID
	depth                       int
	done                        bool
	ofsDelta                    bool // client accepted OFS_DELTA (base v2 arg, not advertised)
	thinPack                    bool // client asked for a thin pack (base v2 arg)
	filter                      *FilterSpec
}

func parseFetchV2(args []string) (*v2FetchArgs, error) {
	fa := &v2FetchArgs{}
	oidAfter := func(line, prefix string) (model.OID, bool) {
		rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		oid := model.OID(strings.Fields(rest)[0]) // anything trailing is not ours to read
		return oid, !oid.IsZero()
	}
	for _, a := range args {
		var oid model.OID
		var ok bool
		switch {
		case strings.HasPrefix(a, "want "):
			if oid, ok = oidAfter(a, "want "); ok {
				fa.wants = append(fa.wants, oid)
			}
		case strings.HasPrefix(a, "have "):
			if oid, ok = oidAfter(a, "have "); ok {
				fa.haves = append(fa.haves, oid)
			}
		case strings.HasPrefix(a, "shallow "):
			if oid, ok = oidAfter(a, "shallow "); ok {
				fa.clientShallow = append(fa.clientShallow, oid)
			}
		case strings.HasPrefix(a, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(a, "deepen "))); err == nil && n > 0 {
				fa.depth = n
			}
		case strings.HasPrefix(a, "filter "):
			spec := strings.TrimSpace(strings.TrimPrefix(a, "filter "))
			fs, err := ParseFilterSpec(spec)
			if err != nil {
				return nil, fmt.Errorf("filter: %s", strings.TrimPrefix(err.Error(), "filter: "))
			}
			if !fs.ServeSupported() {
				return nil, fmt.Errorf("unsupported filter spec %q", spec)
			}
			fa.filter = fs
		case a == "done":
			fa.done = true
		case a == "ofs-delta":
			fa.ofsDelta = true
		case a == "deepen-relative":
			// Counted from the client's shallow boundary; banyan computes
			// depth from the wants (v1 semantics) — deepen-relative is only
			// sent together with deepen-since/not, which are not advertised.
		case a == "thin-pack":
			fa.thinPack = true
		case a == "no-progress", a == "include-tag",
			strings.HasPrefix(a, "agent="), strings.HasPrefix(a, "object-format="):
			// Accepted and ignored: thin deltas against haves are not served
			// (bases must be in-pack); include-tag is best-effort (tags are
			// wanted explicitly or fetched as refs).
		}
	}
	return fa, nil
}

// fetchV2 serves command=fetch. Section order per the spec: acknowledgments,
// shallow-info, packfile; delim-pkt between sections, raw packdata to the
// end of the response (no trailing flush).
//
// Stateless negotiation mirrors the v1 terminal-ACK model: a round with a
// known have gets ACKs + "ready" + the pack in the SAME response (the
// client would read the pack right after "ready" anyway); a round with no
// known have and no done answers acknowledgments/NAK + flush and ends — the
// client POSTs again with more haves or done.
func (s *Server) fetchV2(out *bufWriter, r *http.Request, vr VirtualRepo, args []string, id auth.Identity) {
	ctx := r.Context()
	fa, err := parseFetchV2(args)
	if err != nil {
		_ = WritePacket(out, []byte("ERR "+err.Error()+"\n"))
		_ = WriteFlush(out)
		return
	}
	if len(fa.wants) == 0 {
		_ = WriteFlush(out)
		return
	}
	// Authorization gates (docs/13): repository/directory fetch
	// permission, then want coverage against the identity's VISIBLE tips
	// (with auth off this is the plain allow-reachable-sha1-in-want
	// reachability contract — partial-clone lazy fetches rely on it).
	if s.Auth != nil && !s.Auth.CanFetch(id, vr.Repo, string(vr.Sub)) {
		_ = WritePacket(out, []byte("ERR upload-pack: permission denied\n"))
		_ = WriteFlush(out)
		return
	}
	if bad := s.wantsBeyondTips(ctx, vr, fa.wants, id); !bad.IsZero() {
		_ = WritePacket(out, []byte("ERR upload-pack: not our ref "+string(bad)+"\n"))
		_ = WriteFlush(out)
		return
	}

	// One batched locate for the whole have list — the ACK check never
	// touches object bytes (docs/12 P0-1).
	knownHaves := s.Objects.Known(ctx, vr.Repo, fa.haves)
	var acks []model.OID
	covered := map[model.OID]bool{}
	for _, h := range fa.haves {
		if !knownHaves[h] || covered[h] {
			continue
		}
		acks = append(acks, h)
		covered[h] = true
		// Ancestry pruning, mirroring got_oid's THEY_HAVE flag propagation:
		// an ACKed have covers every later have reachable from it. Clients
		// send haves newest-first, so a linear history ACKs exactly once —
		// ACKing every known have costs ~45 bytes each and nothing needs
		// them (the client proceeds on any single common base + ready).
		if ci, err := s.Objects.CommitInfo(vr.Repo, h); err == nil {
			queue := append([]model.OID(nil), ci.Parents...)
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				if covered[p] {
					continue
				}
				covered[p] = true
				if pci, err := s.Objects.CommitInfo(vr.Repo, p); err == nil {
					queue = append(queue, pci.Parents...)
				}
			}
		}
	}
	// Section plan (mirrors upload-pack.c v2):
	//   - no done, no known have → acknowledgments/NAK + flush: negotiation
	//     round, the client POSTs again with more haves or done;
	//   - no done, known have   → acknowledgments (ACKs + ready) + delim:
	//     terminal round, the pack follows in the same response;
	//   - done                  → NO acknowledgments section at all (the
	//     client reads sections expecting packfile directly — upload-pack
	//     skips acks for done requests).
	if !fa.done && len(acks) == 0 {
		_ = WritePacket(out, []byte("acknowledgments\n"))
		_ = WritePacket(out, []byte("NAK\n"))
		_ = WriteFlush(out)
		return // negotiation round: the client continues with another POST
	}
	if !fa.done {
		_ = WritePacket(out, []byte("acknowledgments\n"))
		for _, oid := range acks {
			_ = WritePacket(out, []byte("ACK "+string(oid)+"\n"))
		}
		_ = WritePacket(out, []byte("ready\n"))
		_ = WriteDelim(out)
	}

	if fa.depth > 0 {
		_ = WritePacket(out, []byte("shallow-info\n"))
		for _, line := range s.shallowSectionLines(vr.Repo, fa.wants, fa.clientShallow, fa.depth) {
			_ = WritePacket(out, []byte(line+"\n"))
		}
		_ = WriteDelim(out)
	}

	closureStart := time.Now()
	blobs, trees, commits, tags, excluded, cerr := s.closure(ctx, vr, fa.wants, fa.depth, fa.clientShallow, fa.haves, fa.filter)
	if cerr != nil {
		// Section headers are already on the wire; finish with an empty pack
		// (valid: header + trailer) so the client fails cleanly at index-pack.
		log.Printf("v2 fetch %s%s: closure: %v", vr.Repo, vr.Sub, cerr)
		blobs, trees, commits, tags = nil, nil, nil, nil
	}
	var plan *thinPackPlan
	if fa.thinPack && len(fa.haves) > 0 {
		plan = &thinPackPlan{
			bases:    s.thinBases(ctx, vr, commits, excluded),
			loadBase: func(oid model.OID) (model.ObjectType, []byte, error) { return s.Objects.Raw(ctx, vr.Repo, oid) },
		}
	}
	encodeStart := time.Now()
	served := "encode"
	_ = WritePacket(out, []byte("packfile\n")) // section header
	// Section content is sideband pkt-lines (spec: *PKT-LINE(%x01-03
	// *%x00-ff)): each chunk carries band 1 (pack data) — this framing is
	// unconditional in v2, independent of the sideband-all capability.
	sb := sidebandWriter{w: out}
	// Serve ladder shared with v1 (docs/12 P2-2): stored-pack verbatim →
	// clone-shaped response cache → online encoding. Thin packs (haves
	// present) are client-specific and never cached.
	cacheable := len(fa.haves) == 0 && fa.depth == 0 && fa.filter == nil
	served = s.serveClosurePack(ctx, sb, vr, fa.wants, blobs, trees, commits, tags, cacheable, fa.ofsDelta, plan)
	_ = WriteFlush(out)
	log.Printf("v2 fetch %s%s: %d wants → %d commits, %d trees, %d blobs (have-subtracted %d, filter %s, closure %s, serve %s/%s)",
		vr.Repo, vr.Sub, len(fa.wants), len(commits), len(trees), len(blobs), len(excluded), filterSpecName(fa.filter),
		time.Since(closureStart).Round(time.Millisecond), served, time.Since(encodeStart).Round(time.Millisecond))
}

// sidebandWriter wraps a writer into v2 packfile-section framing: chunks of
// at most 65515 payload bytes (pkt cap minus the band byte), each prefixed
// with band 1 (pack data).
type sidebandWriter struct {
	w *bufWriter
}

func (s sidebandWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxPktLen-5 {
			chunk = chunk[:maxPktLen-5]
		}
		if err := WritePacket(s.w, append([]byte{1}, chunk...)); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// shallowSectionLines computes the shallow/unshallow section content shared
// by the v1 shallow section (no-LF pkt lines + flush) and the v2
// shallow-info section (LF pkt lines inside the section):
//   - shallow lines: the new boundary at this depth;
//   - unshallow lines: client-reported boundaries now completed.
func (s *Server) shallowSectionLines(repo string, wants, clientShallow []model.OID, depth int) []string {
	included, boundary := s.shallowCut(repo, wants, depth)
	var lines []string
	for _, oid := range sortedOIDs(boundary) {
		lines = append(lines, "shallow "+string(oid))
	}
	cs := map[model.OID]bool{}
	for _, oid := range clientShallow {
		cs[oid] = true
	}
	for _, oid := range sortedOIDs(cs) {
		if included[oid] && !boundary[oid] {
			lines = append(lines, "unshallow "+string(oid))
		}
	}
	return lines
}
