package gitwire

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/view"
)

// Ref is one externally exposed ref view (docs/gateway/git-compat.md §6).
type Ref struct {
	Name string // git-visible name, e.g. refs/heads/main
	OID  model.OID
}

// ResolveRefs generates the ref list for a (virtual) repo (docs/gateway/git-compat.md §6, docs/design/data-model.md §13):
//
//	Root repo: the woven root view per branch — a pure function of the
//	timeline tips (root-local + all subtree timelines), materialized on read.
//	Subtree repo: the subtree view head (projection + local timeline,
//	descendant rule) via the view engine.
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
		isAux := func(name string) bool {
			return strings.HasPrefix(name, "refs/") // tags/notes/custom: stored under the full ref name
		}
		var refs []Ref
		for _, tl := range tls {
			if isAux(tl.Name) {
				continue // advertised below, verbatim
			}
			tip, terr := s.Views.RootTip(ctx, vr.Repo, tl.Name)
			if terr != nil || tip.IsZero() {
				continue
			}
			if tl.Name == defaultBranch {
				refs = append(refs, Ref{Name: "HEAD", OID: tip})
			}
		}
		for _, tl := range tls {
			if isAux(tl.Name) {
				// Auxiliary refs point at fixed objects (tag objects,
				// commits) — no derived view: the stored head is the ref.
				st, serr := s.Meta.Timeline(ctx, tl)
				if serr != nil || st.Head.IsZero() {
					continue
				}
				refs = append(refs, Ref{Name: tl.Name, OID: st.Head})
				continue
			}
			tip, terr := s.Views.RootTip(ctx, vr.Repo, tl.Name)
			if terr != nil || tip.IsZero() {
				continue
			}
			refs = append(refs, Ref{Name: tl.RefName(), OID: tip})
		}
		// Projection refs join the advertisement unless a verbatim aux
		// ref already owns the name (UpdateReferences could have written
		// one; first-wins keeps the advertisement free of duplicates).
		have := map[string]bool{}
		for _, r := range refs {
			have[r.Name] = true
		}
		for _, r := range s.ProjectionRefs(ctx, vr.Repo, defaultBranch) {
			if !have[r.Name] {
				refs = append(refs, r)
			}
		}
		return refs, nil
	}

	return s.resolveSubtreeRefs(ctx, vr, defaultBranch)
}

// projectionRefs lists the subtree-projection refs (docs/integration/gitlab-integration.md §7): every
// top-level directory of the default branch's root view is addressable as a
// repository of its own inside the parent's ref space — refs/banyan/tree/<dir>
// points at exactly what the /tree/<dir> face serves (the scoped weave:
// projection of root-local history plus any local timeline at or below that
// path). Front ends that only know whole repos — GitLab in gitaly mode —
// get banyan's signature any-subtree-is-a-repo through the ref namespace,
// one URL, one credential, one permission surface.
func (s *Server) ProjectionRefs(ctx context.Context, repo string, defaultBranch string) []Ref {
	tip, err := s.Views.RootTip(ctx, repo, defaultBranch)
	if err != nil || tip.IsZero() {
		return nil // no commits on the default branch: nothing to project
	}
	rec, err := s.Objects.Lookup(repo).Commit(tip)
	if err != nil {
		return nil
	}
	tree, err := s.Objects.Lookup(repo).Tree(rec.TreeOID)
	if err != nil {
		return nil
	}
	// Shared-walk heads (docs/operations/performance.md §3b finding ④): ONE
	// history walk derives every directory's head (cached per tip, nothing
	// persisted — the /tree/<dir> face materializes on demand), instead of a
	// SubtreeHead per directory (O(dirs × history), each materializing its
	// whole projection chain before the advertisement answers).
	state, terr := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: defaultBranch})
	if terr != nil {
		return nil
	}
	heads := s.Views.HeadsFor(repo, defaultBranch, state.Head)
	// Directories with local timeline activity at or below them fold those
	// commits in (a strictly longer chain than the shared derivation) —
	// they keep the full SubtreeHead. Rare: subtree pushes only.
	local := map[string]bool{}
	if all, aerr := s.Meta.ListAllTimelines(ctx, repo); aerr == nil {
		for _, tl := range all {
			if tl.Name != defaultBranch || tl.Anchor == model.RootAnchor || view.IsWovenFamily(tl.Anchor) {
				continue
			}
			if st, serr := s.Meta.Timeline(ctx, tl); serr != nil || st.Head.IsZero() {
				continue
			}
			if seg := strings.SplitN(strings.TrimPrefix(string(tl.Anchor), "/"), "/", 2); len(seg) > 0 && seg[0] != "" {
				local[seg[0]] = true
			}
		}
	}
	var out []Ref
	for _, e := range tree.Entries {
		if !e.IsDir || e.Mode == "160000" {
			continue // blobs and gitlinks have no subtree view
		}
		var head model.OID
		if local[e.Name] {
			if h, _, serr := s.Views.SubtreeHead(ctx, repo, model.AnchorPath("/"+e.Name), defaultBranch); serr == nil {
				head = h
			}
		} else {
			head = heads[e.Name]
		}
		if head.IsZero() {
			continue // path never existed in history: /tree/ answers empty too
		}
		out = append(out, Ref{Name: projectionRefPrefix + e.Name, OID: head})
	}
	return out
}

// projectionRefPrefix is the reserved namespace for subtree projections.
const projectionRefPrefix = "refs/banyan/tree/"

// projectionRefPath maps a ref name onto its projected top-level directory,
// ok only for the single-segment form the advertisement emits.
func projectionRefPath(ref string) (string, bool) {
	rest, ok := strings.CutPrefix(ref, projectionRefPrefix)
	if !ok {
		return "", false
	}
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// subtreeBranchName is the timeline branch a projection push lands on: the
// subtree view's default — the same branch the /tree/<dir> face pushes.
func (s *Server) subtreeBranchName(ctx context.Context, repo string) string {
	info, err := s.Meta.GetRepo(ctx, repo)
	if err != nil || info.DefaultBranch == "" {
		return "main"
	}
	return info.DefaultBranch
}

// ResolveRefs' subtree branch: the subtree timeline view (docs/design/data-model.md §12,
// design v4 timeline-first).
func (s *Server) resolveSubtreeRefs(ctx context.Context, vr VirtualRepo, defaultBranch string) ([]Ref, error) {
	head, _, err := s.Views.SubtreeHead(ctx, vr.Repo, vr.Sub, defaultBranch)
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

// bufWriter batches writes (the pkt stream is not flushed per record).
type bufWriter struct {
	bw *bufio.Writer
}

func newBufWriter(w io.Writer) *bufWriter {
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
