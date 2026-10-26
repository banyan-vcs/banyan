package gitwire

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/view"
)

// VirtualRepo is the (virtual) repo location parsed from the URL (docs/04 §1):
//
//	/r/demo                 → {Repo: demo, Sub: "/"}          root repo
//	/r/demo/tree/src/lib    → {Repo: demo, Sub: "/src/lib"}   subtree repo
//	/r/demo//src/lib        → same as above (server tolerates the legacy "//" form; note standard
//	                          git clients normalize "//" in URLs to "/",
//	                          so /tree/ is the canonical external form)
type VirtualRepo struct {
	Repo    string
	Sub     model.AnchorPath
	RepoDir string // repo segment of the request path (incl. subtree suffix), for endpoint matching
}

// ParseVirtualRepo parses a /r/... request path (without the endpoint suffix).
func ParseVirtualRepo(p string) (VirtualRepo, error) {
	if !strings.HasPrefix(p, "/r/") {
		return VirtualRepo{}, fmt.Errorf("not a repo path: %q", p)
	}
	rest := strings.TrimPrefix(p, "/r/")
	vr := VirtualRepo{}
	var subRaw string
	switch {
	case strings.Contains(rest, "//"):
		i := strings.Index(rest, "//")
		vr.Repo = strings.TrimSuffix(rest[:i], ".git")
		subRaw = "/" + rest[i+2:]
	case strings.Contains(rest, "/tree/"):
		i := strings.Index(rest, "/tree/")
		vr.Repo = strings.TrimSuffix(rest[:i], ".git")
		subRaw = "/" + strings.TrimPrefix(rest[i+len("/tree/"):], "/")
	default:
		vr.Repo = strings.TrimSuffix(rest, ".git")
		subRaw = "/"
	}
	vr.RepoDir = rest
	sub, err := model.ParseAnchorPath(subRaw)
	if err != nil {
		return VirtualRepo{}, err
	}
	vr.Sub = sub
	if vr.Repo == "" || strings.Contains(vr.Repo, "/") {
		return VirtualRepo{}, fmt.Errorf("bad repo name in %q", p)
	}
	return vr, nil
}

// Server mounts the git smart HTTP endpoints.
type Server struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo
	Views   *view.Service // derived-view engine (docs/02 §13 assembly-on-read)
	// Auth is the optional authorization service (docs/07, M5): nil keeps
	// the pre-M5 open behavior; set, every advertisement, fetch and push
	// passes through it. Wired by cmd/banyand per role.
	Auth auth.Service
	// SSHHostKey signs the git-over-SSH transport's server key exchange
	// (docs/04 §8); wired by cmd/banyand (generated/persisted host key).
	SSHHostKey ssh.Signer

	// Convergence enables the M4 workflow semantics (docs/15): same-
	// directory pushes serialize on a per-timeline queue and divergent
	// chains converge server-side (pure-Go merge-ort subset, conflict =
	// rejected ticket). Off (the default at this layer) keeps the pre-M4
	// reject-and-rebase behavior; cmd/banyand turns it on via config.
	Convergence bool

	// FullCloneMaxBytes is the repo-size budget for clone-shaped fetches
	// (docs/11 §6): at or above it the gateway rejects full clones and
	// points the client at partial/shallow clones. 0 = the 100 GiB default;
	// negative disables the gate.
	FullCloneMaxBytes int64

	wf     *workflow
	wfOnce sync.Once
}

func NewServer(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Server {
	return &Server{Meta: meta, Objects: objects, Algo: algo, Views: view.New(meta, objects, algo)}
}

// requireAuth resolves the request identity: 401 (with the Basic challenge
// git clients retry against) for anonymous callers when authorization is
// enabled. The bool reports whether to proceed.
func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	if s.Auth == nil {
		return auth.Identity{}, true
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() {
		w.Header().Set("WWW-Authenticate", `Basic realm="banyan"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return auth.Identity{}, false
	}
	return id, true
}

// visibleRefs applies the policy's branch-visibility filter (no-op without
// an Auth service).
func (s *Server) visibleRefs(id auth.Identity, repo string, refs []Ref) []Ref {
	if s.Auth == nil {
		return refs
	}
	out := make([]auth.Ref, len(refs))
	for i, r := range refs {
		out[i] = auth.Ref{Name: r.Name, OID: string(r.OID)}
	}
	filtered := s.Auth.VisibleRefs(id, repo, out)
	back := make([]Ref, len(filtered))
	for i, r := range filtered {
		back[i] = Ref{Name: r.Name, OID: model.OID(r.OID)}
	}
	return back
}

// Register registers routes (docs/04 §1 endpoint table).
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/r/", s.handleRepo)
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	// Request path looks like /r/<repo>[//<sub>]/<endpoint>; strip the endpoint first.
	path := r.URL.Path
	for _, ep := range []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"} {
		if strings.HasSuffix(path, ep) {
			vr, err := ParseVirtualRepo(strings.TrimSuffix(path, ep))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			switch ep {
			case "/info/refs":
				s.handleInfoRefs(w, r, vr)
			case "/git-upload-pack":
				// v2 carries the command in the POST body; remote-curl also sends the
				// Git-Protocol header on the POST (http-backend.c checks the header
				// there too — a v1 body never starts with "command=").
				if wantsV2(r) {
					s.serveUploadPackV2(w, r, vr)
				} else {
					s.serveUploadPack(w, r, vr)
				}
			case "/git-receive-pack":
				s.serveReceivePack(w, r, vr) // receive-pack has no v2 — v0 forever
			}
			return
		}
	}
	http.NotFound(w, r)
}

// handleInfoRefs serves ref discovery (mirrors the smart protocol branch of http-backend.c).
// v1: pkt-line sequence "<oid> <refname>\0<caps>\n", terminated by a flush.
// With Auth set the advertisement is the identity's VISIBLE view: personal
// branches of other users never leave the server (docs/07 §3).
func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "dumb http not supported (use smart)", http.StatusBadRequest)
		return
	}
	id, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	// Demo-friendly: auto-create the repo on first push to a missing repo —
	// gated by the push permission when authorization is on.
	if _, err := s.Meta.GetRepo(r.Context(), vr.Repo); err != nil {
		if service == "git-receive-pack" {
			if s.Auth != nil && !s.Auth.CanPush(id, vr.Repo, string(vr.Sub), "") {
				http.Error(w, "permission denied", http.StatusForbidden)
				return
			}
			if err := s.Meta.CreateRepo(r.Context(), vr.Repo, s.Algo); err != nil {
				http.Error(w, "create repo: "+err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("info/refs: auto-created repo %q", vr.Repo)
		} else {
			http.Error(w, "repository not found: "+vr.Repo, http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")

	// v2 is upload-pack only (receive-pack has no v2; mirroring http-backend.c
	// the header on a receive-pack discovery is ignored and v1 is served).
	if service == "git-upload-pack" && wantsV2(r) {
		s.advertiseV2(w)
		return
	}

	br := newBufWriter(w)
	// Service header pkt (Smart HTTP v1 requires "# service=..." as the first line + flush).
	_ = WritePacket(br, []byte("# service="+service+"\n"))
	_ = WriteFlush(br)
	_ = s.writeRefAdvertisement(r.Context(), br, vr, id, service)
	_ = br.Flush()
}

// writeRefAdvertisement writes the v0 ref advertisement shared by smart HTTP
// and the SSH transport (docs/04 §2): "<oid> <ref>" lines with the
// capability block trailing the first, terminated by a flush; an empty repo
// advertises the virtual capabilities^{} header (mirrors upload-pack.c
// empty-repo handling). The advertisement is the identity's VISIBLE view.
func (s *Server) writeRefAdvertisement(ctx context.Context, out *bufWriter, vr VirtualRepo, id auth.Identity, service string) error {
	refs, err := s.ResolveRefs(ctx, vr)
	if err != nil {
		return err
	}
	refs = s.visibleRefs(id, vr.Repo, refs)
	if len(refs) > 0 && !vr.Sub.IsRoot() {
		// Debugging: per-request log of subtree advertisements (to locate concurrent advertisement inconsistency).
		state, _ := s.Meta.Timeline(ctx, model.Timeline{Repo: vr.Repo, Anchor: model.RootAnchor, Name: s.defaultBranch(ctx, vr.Repo)})
		log.Printf("ADVERT %s%s: tip=%s rootHead=%s", vr.Repo, vr.Sub, refs[0].OID, state.Head)
	}

	caps := s.capabilities(service, s.defaultBranch(ctx, vr.Repo))
	if len(refs) == 0 {
		zero := strings.Repeat("0", 40)
		if s.Algo == model.SHA256 {
			zero = strings.Repeat("0", 64)
		}
		_ = WritePacket(out, []byte(zero+" capabilities^{}\x00"+caps+"\n"))
	} else {
		first := true
		for _, ref := range refs {
			line := string(ref.OID) + " " + ref.Name
			if first {
				line += "\x00" + caps
				first = false
			}
			_ = WritePacket(out, []byte(line+"\n"))
		}
	}
	return WriteFlush(out)
}

// defaultBranch returns the repo's default branch name (symref target), falling back to main when unknown.
func (s *Server) defaultBranch(ctx context.Context, repo string) string {
	if info, err := s.Meta.GetRepo(ctx, repo); err == nil && info.DefaultBranch != "" {
		return info.DefaultBranch
	}
	return "main"
}

// capabilities returns the capability advertisement string (docs/04 §2 capability table).
// Only fully implemented capabilities are advertised: no side-band-64k (response is a bare
// pack), no no-done, no multi_ack (a single terminal ACK suffices), no thin-pack
// (deltas only ever reference in-pack bases).
func (s *Server) capabilities(service, defaultBranch string) string {
	agent := "agent=banyan/0.1"
	objFmt := "object-format=sha1"
	if s.Algo == model.SHA256 {
		objFmt = "object-format=sha256"
	}
	if service == "git-upload-pack" {
		return strings.Join([]string{
			"symref=HEAD:refs/heads/" + defaultBranch,
			"shallow",   // deepen N / --unshallow (v0 core shallowing; deepen-since/not not advertised)
			"filter",    // partial clone: blob:none / tree:0 served (docs/04 §4); unsupported specs answered with ERR
			"ofs-delta", // serve-path delta packs use OFS_DELTA (docs/11 P1-4); REF_DELTA otherwise
			"thin-pack", // have-subtracted fetches delta against client-held bases (docs/11 P2-6)
			// Unadvertised wants (partial-clone lazy fetches name missing blobs by
			// OID) pass a reachability check from the advertised tips — the exact
			// semantic this capability promises (protocol-capabilities.txt).
			"allow-reachable-sha1-in-want",
			agent, objFmt,
		}, " ")
	}
	return strings.Join([]string{
		agent, objFmt,
		"report-status",
		"delete-refs", // clients silently drop delete commands when this capability is absent (gitprotocol-pack)
		"no-thin",     // storage-side packs are self-contained (docs/04 §2)
	}, " ")
}

// handleUploadPack / handleReceivePack dispatch happens in handleRepo
// (v2 negotiation check inline).
