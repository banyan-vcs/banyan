package service

import (
	"context"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// isAuxRefName: any refs/<ns>/<name> outside refs/heads is an auxiliary ref
// (tags, notes, custom namespaces) stored verbatim under the full ref name
// (docs/03 §6).
func isAuxRefName(name string) bool {
	return strings.HasPrefix(name, "refs/")
}

// Ref is one externally visible ref of a (virtual) repo.
type Ref struct {
	Name string    `json:"name"` // HEAD | refs/heads/<b> | refs/tags/<t> | refs/<ns>/<name>
	Head model.OID `json:"head"`
}

// ListRefs mirrors gitwire's advertisement (docs/03 §6) over the JSON API:
// root repo → woven view per branch + verbatim aux refs; subtree repo → the
// subtree view head (heads-only, views are derived).
func (s *Service) ListRefs(ctx context.Context, repo string, sub string) ([]Ref, error) {
	ap, err := parseSub(sub)
	if err != nil {
		return nil, err
	}
	def, err := s.defaultBranchOf(ctx, repo)
	if err != nil {
		return nil, err
	}
	if !ap.IsRoot() {
		head, _, _, err := s.viewHead(ctx, repo, ap, def)
		if err != nil {
			return nil, err
		}
		if head.IsZero() {
			return []Ref{}, nil
		}
		return []Ref{
			{Name: "HEAD", Head: head},
			{Name: "refs/heads/" + def, Head: head},
		}, nil
	}
	tls, err := s.Meta.ListTimelines(ctx, repo, model.RootAnchor)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, tl := range tls {
		if isAuxRefName(tl.Name) {
			continue
		}
		if tl.Name != def {
			continue
		}
		if tip, terr := s.Views.RootTip(ctx, repo, tl.Name); terr == nil && !tip.IsZero() {
			refs = append(refs, Ref{Name: "HEAD", Head: tip})
			break
		}
	}
	for _, tl := range tls {
		if isAuxRefName(tl.Name) {
			st, serr := s.Meta.Timeline(ctx, tl)
			if serr != nil || st.Head.IsZero() {
				continue
			}
			refs = append(refs, Ref{Name: tl.Name, Head: st.Head})
			continue
		}
		tip, terr := s.Views.RootTip(ctx, repo, tl.Name)
		if terr != nil || tip.IsZero() {
			continue
		}
		refs = append(refs, Ref{Name: tl.RefName(), Head: tip})
	}
	return refs, nil
}

// RefUpdate is one CAS on a root-repo ref (docs/10 §3.3): old zero = create,
// new zero = delete. Branches map to root timelines; auxiliary refs (tags
// etc.) to their full-name timelines — the same storage as gitwire's
// receive-pack path, so both frontends stay interchangeable.
type RefUpdate struct {
	Repo string    `json:"repo"`
	Name string    `json:"name"`
	Old  model.OID `json:"old"`
	New  model.OID `json:"new"`
}

func (s *Service) UpdateRef(ctx context.Context, req RefUpdate) error {
	if _, err := s.Meta.GetRepo(ctx, req.Repo); err != nil {
		return err
	}
	if !strings.HasPrefix(req.Name, "refs/") || !strings.Contains(strings.TrimPrefix(req.Name, "refs/"), "/") {
		return invalid("ref name %q: want refs/<ns>/<name>", req.Name)
	}
	if !req.New.IsZero() {
		if _, _, err := s.Objects.Raw(ctx, req.Repo, req.New); err != nil {
			return invalid("target object %s missing", req.New)
		}
	}
	tl := model.Timeline{Repo: req.Repo, Anchor: model.RootAnchor, Name: req.Name}
	if strings.HasPrefix(req.Name, "refs/heads/") {
		name := strings.TrimPrefix(req.Name, "refs/heads/")
		if name == "" || strings.Contains(name, "/") {
			return invalid("branch name %q invalid in this context", req.Name)
		}
		tl.Name = name
	}
	expected := req.Old
	if st, err := s.Meta.Timeline(ctx, tl); err == nil {
		expected = st.Head // the advertised value maps to the stored head
	}
	if err := s.Meta.CASHead(ctx, tl, expected, req.New); err != nil {
		return err
	}
	return nil
}
