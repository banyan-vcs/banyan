package gitwire

import (
	"fmt"
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

// subtreeHead 返回子树视图 tip 与当前根 head（统一 DAG 设计）。
//
// tip 解析优先级（resolveViewTip）：
//  1. 根 head 是本路径的收编 merge（第二父的树 == 根树在本路径处的子树）
//     → tip = 被收编的 C 本身——hash 与客户端推送的一致，连续 push /
//     pull 的 fast-forward 语义完全对齐 git；
//  2. 否则生成确定性合成薄 tip（SubtreeTip：tree=子树、parent=根提交），
//     祖先即统一 DAG，父树历史以原始 hash 进入子树视图。
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
	head, err = s.subtreeTip(vr, defaultBranch, rootHead)
	if err != nil {
		return "", "", err
	}
	return head, rootHead, nil
}

// subtreeTip 计算视图 tip（链式缓存 + resolveViewTip）。
// 状态按 (repo, X, branch) 记忆：root 未变返回上次 tip；root 前进则
// 以 [旧tip, 新根] 双亲建链式新 tip——保证客户端相邻两次 fetch 的
// merge-base 是 X 作用域提交（详见 resolveViewTip 注释）。
func (s *Server) subtreeTip(vr VirtualRepo, branch string, rootHead model.OID) (model.OID, error) {
	key := viewKey{repo: vr.Repo, sub: vr.Sub, branch: branch}
	s.projMu.Lock()
	if st, ok := s.viewChain[key]; ok && st.root == rootHead {
		oid := st.tip
		s.projMu.Unlock()
		return oid, nil
	}
	s.projMu.Unlock()

	oid, err := s.resolveViewTip(vr, branch, rootHead)
	if err != nil {
		return "", err
	}
	if oid.IsZero() {
		return "", model.ErrObjectNotFound
	}
	s.projMu.Lock()
	s.viewChain[key] = viewChainState{root: rootHead, tip: oid}
	s.projMu.Unlock()
	return oid, nil
}

// resolveViewTip 解析视图 tip：
//
//  1. 根 head 是本路径的收编 merge（第二父的树 == 根树在本路径处的
//     子树）→ tip = 被收编的提交本身（hash 与客户端推送一致）；
//  2. 否则生成链式合成 tip：parents = [上一 tip, 根 head]。
//     链式化的必要性：若 tip.parent = 根提交（全仓作用域树），客户端
//     相邻两次 fetch 的 merge-base 落在全仓提交上——X 路径在 base 中
//     不存在，任何跨视图同文件的串行编辑都会被 git 判为 add/add 伪
//     冲突（实测：bob 在父级 pull 后追加一行即触发）。链式化后
//     base = 旧 tip（X 作用域），pull 退化为 fast-forward 或规范的
//     modify/modify 三方合并。
//     第一父 = 旧 tip（客户端 log 的主脉络），第二父 = 根提交
//     （统一 DAG 可见性）。
//
// 已知限制：链状态在内存（M2 随 MetaStore 持久化）；进程重启后第一
// 个 tip 退化为单亲 [根 head]，重启窗口内未 pull 的客户端可能遇到
// 一次伪冲突。
func (s *Server) resolveViewTip(vr VirtualRepo, branch string, rootHead model.OID) (model.OID, error) {
	lookup := s.Objects.Lookup(vr.Repo)
	rootInfo, err := s.Objects.CommitInfo(vr.Repo, rootHead)
	if err != nil {
		return "", err
	}
	if len(rootInfo.Parents) == 2 {
		if c2, err := s.Objects.CommitInfo(vr.Repo, rootInfo.Parents[1]); err == nil {
			want := model.SubtreeAt(lookup, rootInfo.Tree, vr.Sub)
			if !want.IsZero() && c2.Tree == want {
				return rootInfo.Parents[1], nil
			}
		}
	}
	sub := model.SubtreeAt(lookup, rootInfo.Tree, vr.Sub)
	if sub.IsZero() {
		return "", model.ErrObjectNotFound
	}
	// 确定性身份：签名固定、时间取根提交 committer（SubtreeTip 同款）。
	rootCommit, err := lookup.Commit(rootHead)
	if err != nil {
		return "", err
	}
	viewID := model.Identity{
		Name:  "banyan-view",
		Email: "view@banyan.local",
		When:  rootCommit.Committer.When,
	}
	tip := &model.AnchoredCommit{
		Anchor:    model.RootAnchor,
		TreeOID:   sub,
		Parents:   []model.OID{rootHead},
		Author:    viewID,
		Committer: viewID,
		Message:   fmt.Sprintf("banyan: subtree view %s @ %s\n", vr.Sub, rootHead),
	}
	// 链式：有上一 tip 则作为第一父。
	key := viewKey{repo: vr.Repo, sub: vr.Sub, branch: branch}
	if prev := s.chainTip(key); !prev.IsZero() {
		tip.Parents = []model.OID{prev, rootHead}
	}
	oid := tip.OID(s.Algo)
	s.Objects.PutSynthetic(vr.Repo, oid, model.CommitType, tip.Encode())
	return oid, nil
}

type viewKey struct {
	repo   string
	sub    model.AnchorPath
	branch string
}

type viewChainState struct {
	root model.OID
	tip  model.OID
}

// chainTip 读取视图链的上一 tip（无状态返回零值）。
func (s *Server) chainTip(key viewKey) model.OID {
	s.projMu.Lock()
	defer s.projMu.Unlock()
	if st, ok := s.viewChain[key]; ok {
		return st.tip
	}
	return ""
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
