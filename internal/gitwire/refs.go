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

	// 子树仓库：组合投影 head（docs/01 §5.2 split(根) ⊕ 本地提交）。
	head, _, err := s.subtreeHead(ctx, vr)
	if err != nil {
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

// subtreeHead 返回子树仓库的组合 head 与当前根 head。
// 组合规则：本地 timeline（push 到 /tree/… 锚定而来）有提交时优先，
// 否则为投影 head；本地提交的 parent 链自然挂接投影链（分叉语义
// 由投影纯函数性保证，docs/01 §7）。
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
	head, err = s.projection(ctx, vr, rootHead)
	if err != nil {
		return "", "", err
	}
	if local, lerr := s.Meta.Timeline(ctx, model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: defaultBranch}); lerr == nil && !local.Head.IsZero() {
		head = local.Head
	}
	return head, rootHead, nil
}

// projection 计算并缓存子树投影（docs/01 §5.2）：SplitHistory 得到投影
// commit 链，物化进合成对象存储，返回投影 head OID。投影是纯函数 ⇒
// 以 (repo, sub, rootHead) 为缓存键，push 前进后自然失效。
func (s *Server) projection(_ context.Context, vr VirtualRepo, rootHead model.OID) (model.OID, error) {
	key := projectionKey{repo: vr.Repo, sub: vr.Sub, root: rootHead}
	s.projMu.Lock()
	if oid, ok := s.projCache[key]; ok {
		s.projMu.Unlock()
		return oid, nil
	}
	s.projMu.Unlock()

	res, err := model.SplitHistory(s.Objects.Lookup(vr.Repo), s.Algo, rootHead, vr.Sub)
	if err != nil {
		return "", err
	}
	if len(res.Commits) == 0 {
		s.projMu.Lock()
		s.projCache[key] = ""
		s.projMu.Unlock()
		return "", nil
	}
	for _, pc := range res.Commits {
		s.Objects.PutSynthetic(vr.Repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
	}
	head := res.Head
	s.projMu.Lock()
	s.projCache[key] = head
	s.projMu.Unlock()
	return head, nil
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
