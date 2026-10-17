package gitwire

import (
	"bufio"
	"context"
	"errors"
	"net/http"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Ref 是对外暴露的一条 ref 视图（docs/03 §6）。
type Ref struct {
	Name string // git 可见名，如 refs/heads/main
	OID  model.OID
}

// ResolveRefs 为（虚拟）仓库生成 ref 列表（docs/03 §6 ref 映射表）：
//
//	根仓库：各根时间线 head 原样暴露（投影零翻译），HEAD 指向默认分支。
//	子树仓库：根时间线经 SplitHistory 投影后的 head（docs/01 §5.2）。
func (s *Server) ResolveRefs(ctx context.Context, vr VirtualRepo) ([]Ref, error) {
	info, err := s.Meta.GetRepo(ctx, vr.Repo)
	if err != nil {
		return nil, err
	}
	defaultBranch := info.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	if vr.Sub.IsRoot() {
		tls, err := s.Meta.ListTimelines(ctx, vr.Repo, model.RootAnchor)
		if err != nil {
			return nil, err
		}
		var refs []Ref
		for _, tl := range tls {
			state, err := s.Meta.Timeline(ctx, tl)
			if err != nil {
				continue
			}
			if tl.Name == defaultBranch {
				refs = append(refs, Ref{Name: "HEAD", OID: state.Head})
			}
		}
		for _, tl := range tls {
			state, err := s.Meta.Timeline(ctx, tl)
			if err != nil {
				continue
			}
			refs = append(refs, Ref{Name: tl.RefName(), OID: state.Head})
		}
		return refs, nil
	}

	// 子树仓库：视图 tip（统一 DAG，docs/01 §10）。
	head, _, err := s.subtreeHead(ctx, vr)
	if err != nil {
		if errors.Is(err, model.ErrObjectNotFound) {
			return nil, nil // 该路径在根提交树中不存在：空仓库语义
		}
		return nil, err
	}
	if head.IsZero() {
		return nil, nil // 根无提交或该路径在历史中从未存在
	}
	return []Ref{
		{Name: "HEAD", OID: head},
		{Name: "refs/heads/" + defaultBranch, OID: head},
	}, nil
}

// subtreeHead 返回子树视图 tip 与当前根 head（统一 DAG 设计）：
// tip = SubtreeTip(根 head, path)——tree=子树内容、parent=根提交，
// 祖先即统一 DAG，父树历史以原始 hash 进入子树视图（展示过滤交给
// git 原生 `git log -- .`）。子树 push 经 AdoptSubtreePush 收编后根
// head 前进，tip 随之推进——无独立子树 timeline。
func (s *Server) subtreeHead(ctx context.Context, vr VirtualRepo) (head, rootHead model.OID, err error) {
	info, err := s.Meta.GetRepo(ctx, vr.Repo)
	if err != nil {
		return "", "", err
	}
	defaultBranch := info.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	rootTL := model.Timeline{Repo: vr.Repo, Anchor: model.RootAnchor, Name: defaultBranch}
	state, err := s.Meta.Timeline(ctx, rootTL)
	if errors.Is(err, storage.ErrTimelineUnknown) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	rootHead = state.Head
	head, err = s.subtreeTip(vr, rootHead)
	if err != nil {
		return "", "", err
	}
	return head, rootHead, nil
}

// subtreeTip 计算并缓存视图 tip：SubtreeTip 是 (rootOID, path) 的纯
// 函数（确定性 hash），物化进合成对象层后可被 fetch 的闭包遍历命中。
func (s *Server) subtreeTip(vr VirtualRepo, rootHead model.OID) (model.OID, error) {
	key := projectionKey{repo: vr.Repo, sub: vr.Sub, root: rootHead}
	s.projMu.Lock()
	if oid, ok := s.projCache[key]; ok {
		s.projMu.Unlock()
		return oid, nil
	}
	s.projMu.Unlock()

	tip, err := model.SubtreeTip(s.Objects.Lookup(vr.Repo), s.Algo, rootHead, vr.Sub)
	if err != nil {
		return "", err
	}
	oid := tip.OID(s.Algo)
	s.Objects.PutSynthetic(vr.Repo, oid, model.CommitType, tip.Encode())
	s.projMu.Lock()
	s.projCache[key] = oid
	s.projMu.Unlock()
	return oid, nil
}

type projectionKey struct {
	repo string
	sub  model.AnchorPath
	root model.OID
}

// bufWriter 聚合写（pkt 流不逐条 flush）。
type bufWriter struct {
	bw *bufio.Writer
}

func newBufWriter(w http.ResponseWriter) *bufWriter {
	return &bufWriter{bw: bufio.NewWriterSize(w, 64*1024)}
}

func (b *bufWriter) Write(p []byte) (int, error) { return b.bw.Write(p) }
func (b *bufWriter) Flush() error                { return b.bw.Flush() }
