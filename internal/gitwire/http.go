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

// VirtualRepo 是 URL 解析出的（虚拟）仓库定位（docs/03 §1）：
//
//	/r/demo                 → {Repo: demo, Sub: "/"}          根仓库
//	/r/demo/tree/src/lib    → {Repo: demo, Sub: "/src/lib"}   子树仓库
//	/r/demo//src/lib        → 同上（服务端兼容旧 "//" 写法；注意标准
//	                          git 客户端会把 URL 中 "//" 规范化为 "/"，
//	                          因此对外统一使用 /tree/ 形式）
type VirtualRepo struct {
	Repo    string
	Sub     model.AnchorPath
	RepoDir string // 请求路径中仓库段（含子树后缀），供端点匹配
}

// ParseVirtualRepo 解析 /r/... 请求路径（不含端点后缀）。
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

// Server 挂载 git smart HTTP v1 端点。
type Server struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo

	projMu    sync.Mutex
	viewChain map[viewKey]viewChainState // 视图 tip 链状态（refs.go）
}

func NewServer(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Server {
	return &Server{Meta: meta, Objects: objects, Algo: algo, viewChain: map[viewKey]viewChainState{}}
}

// Register 注册路由（docs/03 §1 端点表）。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/r/", s.handleRepo)
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	// 请求路径形如 /r/<repo>[//<sub>]/<endpoint>，先剥端点。
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

// handleInfoRefs 服务引用发现（对照 http-backend.c 的 smart 协议分支）。
// v1： pkt-line 序列 "<oid> <refname>\0<caps>\n"，以 flush 结束。
func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "dumb http not supported (use smart)", http.StatusBadRequest)
		return
	}
	// demo 友好：push 首次打到不存在的仓库时自动建仓（M2 收紧为 ACL 策略）。
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
		// 调试：子树广告的请求级记录（定位并发广告不一致）。
		state, _ := s.Meta.Timeline(r.Context(), model.Timeline{Repo: vr.Repo, Anchor: model.RootAnchor, Name: s.defaultBranch(r.Context(), vr.Repo)})
		log.Printf("ADVERT %s%s: tip=%s rootHead=%s", vr.Repo, vr.Sub, refs[0].OID, state.Head)
	}

	br := newBufWriter(w)
	// 服务头 pkt（Smart HTTP v1 要求第一条为 "# service=..." + flush）。
	_ = WritePacket(br, []byte("# service="+service+"\n"))
	_ = WriteFlush(br)

	caps := s.capabilities(service, s.defaultBranch(r.Context(), vr.Repo))
	if len(refs) == 0 {
		// 空仓库：能力挂在虚拟 capability^{} 头上（对照 upload-pack.c 空仓处理）。
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

// defaultBranch 返回仓库默认分支名（symref 目标），未知时回退 main。
func (s *Server) defaultBranch(ctx context.Context, repo string) string {
	if info, err := s.Meta.GetRepo(ctx, repo); err == nil && info.DefaultBranch != "" {
		return info.DefaultBranch
	}
	return "main"
}

// capabilities 返回能力协商串（docs/03 §2 能力表）。
// MVP 只广告已完整实现的能力：不广告 side-band-64k（应答为裸 pack）、
// 不广告 filter（M3 实现服务端过滤后再开）、不广告 no-done。
func (s *Server) capabilities(service, defaultBranch string) string {
	agent := "agent=banyan/0.1"
	objFmt := "object-format=sha1"
	if s.Algo == model.SHA256 {
		objFmt = "object-format=sha256"
	}
	if service == "git-upload-pack" {
		return strings.Join([]string{
			"symref=HEAD:refs/heads/" + defaultBranch,
			agent, objFmt,
		}, " ")
	}
	return strings.Join([]string{
		agent, objFmt,
		"report-status",
		"delete-refs", // 未广告此能力时客户端会静默丢弃删除命令（gitprotocol-pack）
		"no-thin", // 存储侧 pack 自包含（docs/03 §2）
	}, " ")
}

func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	s.serveUploadPack(w, r, vr)
}

func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	s.serveReceivePack(w, r, vr)
}
