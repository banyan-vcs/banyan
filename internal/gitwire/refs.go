package gitwire

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Ref is one externally exposed ref view (docs/04 §6).
type Ref struct {
	Name string // git-visible name, e.g. refs/heads/main
	OID  model.OID
}

// ResolveRefs generates the ref list for a (virtual) repo (docs/04 §6, docs/02 §13):
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
		return refs, nil
	}

	// Subtree repo: subtree timeline view (docs/02 §12, design v4 timeline-first).
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
