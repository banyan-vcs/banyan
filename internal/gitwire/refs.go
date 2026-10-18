package gitwire

import (
	"bufio"
	"context"
	"errors"
	"net/http"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Ref is one externally exposed ref view (docs/03 §6).
type Ref struct {
	Name string // git-visible name, e.g. refs/heads/main
	OID  model.OID
}

// ResolveRefs generates the ref list for a (virtual) repo (docs/03 §6 ref mapping table):
//
//	Root repo: each root timeline's head exposed as-is (zero-translation projection), HEAD points at the default branch.
//	Subtree repo: the head of the root timeline projected via SplitHistory (docs/01 §5.2).
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

	// Subtree repo: subtree timeline (docs/01 §12, design v4 timeline-first).
	head, _, err := s.subtreeHead(ctx, vr)
	if err != nil {
		if errors.Is(err, model.ErrObjectNotFound) {
			return nil, nil // path absent from the root commit's tree: empty-repo semantics
		}
		return nil, err
	}
	if head.IsZero() {
		return nil, nil // root has no commits or the path never existed in history
	}
	return []Ref{
		{Name: "HEAD", OID: head},
		{Name: "refs/heads/" + defaultBranch, OID: head},
	}, nil
}

// subtreeHead returns the subtree timeline head and the current root head (docs/01 §12).
//
// Head-merge rules (descendant rule):
//   - no local timeline → projected-chain head (split(rootHead, X));
//   - projected-chain head is a descendant of the local head (root side / another side merged X changes, the common case) → projected-chain head
//     — client pull = fast-forward, root-side changes auto-follow downstream;
//   - local head is a descendant of the projected-chain head → local head;
//   - true divergence → local head wins (conservative: the root side waits for an explicit sync; the server does no content merging,
//     conflicts must be resolved explicitly by git three-way merge).
//
// On the projected chain, adoption merges of this path map back to the adopted commit itself
// via SplitHistory, so one's own pushed changes keep their own SHA after flowing back.
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

// subtreeTip computes and caches the timeline head: projected chain + local timeline merged by the descendant rule.
// A pure function of (rootHead, localTip) — deterministic, cacheable, recomputable after restart.
func (s *Server) subtreeTip(vr VirtualRepo, branch string, rootHead, localTip model.OID) (model.OID, error) {
	key := viewKey{repo: vr.Repo, sub: vr.Sub, branch: branch, root: rootHead, local: localTip}
	s.projMu.Lock()
	if oid, ok := s.viewCache[key]; ok {
		s.projMu.Unlock()
		return oid, nil
	}
	s.projMu.Unlock()

	// Projected chain: split(rootHead, X), materializing projection commits into the synthetic object layer.
	// Adoption merges map to the adopted commit itself (docs/01 §12).
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

	// Head merge by the descendant rule.
	var head model.OID
	switch {
	case localTip.IsZero():
		head = projTip
	case projTip.IsZero():
		head = localTip
	case s.descends(vr.Repo, projTip, localTip): // projected head ⊇ local head
		head = projTip
	case s.descends(vr.Repo, localTip, projTip): // local head ⊇ projected head
		head = localTip
	default: // true divergence: local wins (explicit sync semantics)
		head = localTip
	}

	s.projMu.Lock()
	s.viewCache[key] = head
	s.projMu.Unlock()
	return head, nil
}

// descends reports whether target is an ancestor of from (BFS along all parent edges, via the
// CommitGraph index; the descendant rule's head-merge check).
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

// bufWriter batches writes (the pkt stream is not flushed per record).
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
