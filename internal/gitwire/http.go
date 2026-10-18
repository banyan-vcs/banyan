package gitwire

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// VirtualRepo is the (virtual) repo location parsed from the URL (docs/03 §1):
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

// Server mounts the git smart HTTP v1 endpoints.
type Server struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo

	projMu    sync.Mutex
	viewCache map[viewKey]model.OID // subtree timeline head cache (refs.go, v4)
}

func NewServer(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Server {
	return &Server{Meta: meta, Objects: objects, Algo: algo, viewCache: map[viewKey]model.OID{}}
}

// Register registers routes (docs/03 §1 endpoint table).
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
				s.handleUploadPack(w, r, vr)
			case "/git-receive-pack":
				s.handleReceivePack(w, r, vr)
			}
			return
		}
	}
	http.NotFound(w, r)
}

// handleInfoRefs serves ref discovery (mirrors the smart protocol branch of http-backend.c).
// v1: pkt-line sequence "<oid> <refname>\0<caps>\n", terminated by a flush.
func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "dumb http not supported (use smart)", http.StatusBadRequest)
		return
	}
	// Demo-friendly: auto-create the repo on first push to a missing repo (M2 will tighten to ACL policy).
	if _, err := s.Meta.GetRepo(r.Context(), vr.Repo); err != nil {
		if service == "git-receive-pack" {
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

	refs, err := s.ResolveRefs(r.Context(), vr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(refs) > 0 && !vr.Sub.IsRoot() {
		// Debugging: per-request log of subtree advertisements (to locate concurrent advertisement inconsistency).
		state, _ := s.Meta.Timeline(r.Context(), model.Timeline{Repo: vr.Repo, Anchor: model.RootAnchor, Name: s.defaultBranch(r.Context(), vr.Repo)})
		log.Printf("ADVERT %s%s: tip=%s rootHead=%s", vr.Repo, vr.Sub, refs[0].OID, state.Head)
	}

	br := newBufWriter(w)
	// Service header pkt (Smart HTTP v1 requires "# service=..." as the first line + flush).
	_ = WritePacket(br, []byte("# service="+service+"\n"))
	_ = WriteFlush(br)

	caps := s.capabilities(service, s.defaultBranch(r.Context(), vr.Repo))
	if len(refs) == 0 {
		// Empty repo: capabilities hang off the virtual capability^{} header (mirrors upload-pack.c empty-repo handling).
		zero := strings.Repeat("0", 40)
		if s.Algo == model.SHA256 {
			zero = strings.Repeat("0", 64)
		}
		_ = WritePacket(br, []byte(zero+" capabilities^{}\x00"+caps+"\n"))
	} else {
		first := true
		for _, ref := range refs {
			line := string(ref.OID) + " " + ref.Name
			if first {
				line += "\x00" + caps
				first = false
			}
			_ = WritePacket(br, []byte(line+"\n"))
		}
	}
	_ = WriteFlush(br)
	_ = br.Flush()
}

// defaultBranch returns the repo's default branch name (symref target), falling back to main when unknown.
func (s *Server) defaultBranch(ctx context.Context, repo string) string {
	if info, err := s.Meta.GetRepo(ctx, repo); err == nil && info.DefaultBranch != "" {
		return info.DefaultBranch
	}
	return "main"
}

// capabilities returns the capability advertisement string (docs/03 §2 capability table).
// The MVP advertises only fully implemented capabilities: no side-band-64k (response is a bare pack),
// no filter (enable after M3 implements server-side filtering), no no-done.
func (s *Server) capabilities(service, defaultBranch string) string {
	agent := "agent=banyan/0.1"
	objFmt := "object-format=sha1"
	if s.Algo == model.SHA256 {
		objFmt = "object-format=sha256"
	}
	if service == "git-upload-pack" {
		return strings.Join([]string{
			"symref=HEAD:refs/heads/" + defaultBranch,
			"shallow", // deepen N / --unshallow (v0 core shallowing; deepen-since/not not advertised)
			agent, objFmt,
		}, " ")
	}
	return strings.Join([]string{
		agent, objFmt,
		"report-status",
		"delete-refs", // clients silently drop delete commands when this capability is absent (gitprotocol-pack)
		"no-thin", // storage-side packs are self-contained (docs/03 §2)
	}, " ")
}

func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	s.serveUploadPack(w, r, vr)
}

func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	s.serveReceivePack(w, r, vr)
}
