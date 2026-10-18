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

	// 子树仓库：子树时间线（docs/01 §12，设计 v4 时间线优先）。
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

// subtreeHead 返回子树时间线 head 与当前根 head（docs/01 §12）。
//
// 合头规则（后裔规则）：
//   - 无本地时间线 → 投影链头（split(rootHead, X)）；
//   - 投影链头是本地头的后裔（根侧/他侧合入了 X 变更，常态）→ 投影链头
//     ——客户端 pull = fast-forward，根侧变更 auto-follow 流下；
//   - 本地头是投影链头的后裔 → 本地头；
//   - 真分叉 → 本地头优先（保守：根侧等待显式 sync；服务端不做内容合并，
//     冲突必须由 git 三方合并显式解决）。
//
// 投影链上，本路径的收编 merge 经身份映射还原为被收编提交本尊
//（SplitHistory），故自己 push 的变更回流后仍是自己的 SHA。
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

	localTL := model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: defaultBranch}
	var localTip model.OID
	if st, lerr := s.Meta.Timeline(ctx, localTL); lerr == nil {
		localTip = st.Head
	}

	head, err = s.subtreeTip(vr, defaultBranch, rootHead, localTip)
	if err != nil {
		return "", "", err
	}
	return head, rootHead, nil
}

// subtreeTip 计算并缓存时间线 head：投影链 + 本地时间线按后裔规则合头。
// 纯函数于 (rootHead, localTip)——确定性、可缓存、重启可重算。
func (s *Server) subtreeTip(vr VirtualRepo, branch string, rootHead, localTip model.OID) (model.OID, error) {
	key := viewKey{repo: vr.Repo, sub: vr.Sub, branch: branch, root: rootHead, local: localTip}
	s.projMu.Lock()
	if oid, ok := s.viewCache[key]; ok {
		s.projMu.Unlock()
		return oid, nil
	}
	s.projMu.Unlock()

	// 投影链：split(rootHead, X)，物化投影提交进合成对象层。
	// 收编 merge 身份映射为被收编提交本尊（docs/01 §12）。
	var projTip model.OID
	if !rootHead.IsZero() {
		res, err := model.SplitHistory(s.Objects.Lookup(vr.Repo), s.Algo, rootHead, vr.Sub)
		if err != nil {
			return "", err
		}
		for _, pc := range res.Commits {
			s.Objects.PutSynthetic(vr.Repo, pc.OID(s.Algo), model.CommitType, pc.Encode())
		}
		projTip = res.Head
	}

	// 后裔规则合头。
	var head model.OID
	switch {
	case localTip.IsZero():
		head = projTip
	case projTip.IsZero():
		head = localTip
	case s.descends(vr.Repo, projTip, localTip): // 投影头 ⊇ 本地头
		head = projTip
	case s.descends(vr.Repo, localTip, projTip): // 本地头 ⊇ 投影头
		head = localTip
	default: // 真分叉：本地优先（显式 sync 语义）
		head = localTip
	}

	s.projMu.Lock()
	s.viewCache[key] = head
	s.projMu.Unlock()
	return head, nil
}

// descends 判定 target 是否为 from 的祖先（沿全部父边 BFS，走 CommitGraph
// 索引；后裔规则的合头判定）。
func (s *Server) descends(repo string, from, target model.OID) bool {
	if from.IsZero() || target.IsZero() || from == target {
		return from == target
	}
	visited := map[model.OID]bool{}
	queue := []model.OID{from}
	for len(queue) > 0 && len(visited) < 100000 {
		oid := queue[0]
		queue = queue[1:]
		if visited[oid] {
			continue
		}
		visited[oid] = true
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			continue
		}
		for _, p := range ci.Parents {
			if p == target {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

type viewKey struct {
	repo   string
	sub    model.AnchorPath
	branch string
	root   model.OID
	local  model.OID
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

func firstLineOf(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
