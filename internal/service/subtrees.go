// Subtree listing (docs/gateway/authorization.md §8, docs/integration/gitlab-integration.md §"子树管理控制面"): the management
// view of a repo's directory-anchored timelines — which subtrees exist,
// their branch heads and last commits, and which carry a projection ref
// (refs/banyan/tree/<dir>, the whole-repo-frontend recipe). The raw
// ListAllTimelines rows also contain the internal /.woven materialization
// family and verbatim aux refs; those are bookkeeping, not subtrees.
package service

import (
	"context"
	"sort"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/view"
)

// SubtreeBranch is one branch head of a directory timeline.
type SubtreeBranch struct {
	Name    string `json:"name"`
	Head    string `json:"head"`
	Subject string `json:"subject,omitempty"`
	Author  string `json:"author,omitempty"`
	When    int64  `json:"when,omitempty"`
}

// SubtreeSummary aggregates one subtree anchor. Projected marks the
// top-level directories the ref-space projection serves (refs/banyan/tree/).
type SubtreeSummary struct {
	Path      string           `json:"path"`
	Branches  []SubtreeBranch  `json:"branches"`
	Projected bool             `json:"projected"`
}

// Subtrees answers the repo's directory timelines grouped by anchor, sorted
// by path. The root anchor (the repo itself), the /.woven internals and
// verbatim aux refs are excluded — this lists SUBTREES, not refs.
func (s *Service) Subtrees(ctx context.Context, repo string) ([]SubtreeSummary, error) {
	if _, err := s.Meta.GetRepo(ctx, repo); err != nil {
		return nil, err
	}
	branch, err := s.defaultBranchOf(ctx, repo)
	if err != nil {
		return nil, err
	}
	tls, err := s.Meta.ListAllTimelines(ctx, repo)
	if err != nil {
		return nil, err
	}
	lookup := s.Objects.Lookup(repo)
	grouped := map[model.AnchorPath][]SubtreeBranch{}
	for _, tl := range tls {
		if tl.Anchor.IsRoot() || isAuxRefName(tl.Name) || view.IsWovenFamily(tl.Anchor) {
			continue
		}
		st, err := s.Meta.Timeline(ctx, tl)
		if err != nil {
			continue
		}
		b := SubtreeBranch{Name: tl.Name, Head: string(st.Head)}
		if c, cerr := lookup.Commit(st.Head); cerr == nil {
			b.Subject = firstLine(c.Message)
			b.Author = c.Author.Name + " <" + c.Author.Email + ">"
			b.When = c.Committer.When.Unix()
		}
		grouped[tl.Anchor] = append(grouped[tl.Anchor], b)
	}

	projected := s.projectedDirs(ctx, repo, branch)
	paths := make([]model.AnchorPath, 0, len(grouped))
	for a := range grouped {
		paths = append(paths, a)
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })
	out := make([]SubtreeSummary, 0, len(paths))
	for _, a := range paths {
		branches := grouped[a]
		sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
		out = append(out, SubtreeSummary{Path: string(a), Branches: branches, Projected: projected[a]})
	}
	return out, nil
}

// DefaultBranch answers the repo's advertised default branch — the subtree
// listing's projection set is derived against it.
func (s *Service) DefaultBranch(ctx context.Context, repo string) (string, error) {
	if _, err := s.Meta.GetRepo(ctx, repo); err != nil {
		return "", err
	}
	return s.defaultBranchOf(ctx, repo)
}

// projectedDirs mirrors gitwire.ProjectionRefs' derivation — the top-level
// directories of the default branch's woven root tree with a non-zero
// subtree head — so the management listing marks exactly the subtrees the
// ref-space projection serves.
func (s *Service) projectedDirs(ctx context.Context, repo, branch string) map[model.AnchorPath]bool {
	out := map[model.AnchorPath]bool{}
	tip, err := s.Views.RootTip(ctx, repo, branch)
	if err != nil || tip.IsZero() {
		return out
	}
	lookup := s.Objects.Lookup(repo)
	rec, err := lookup.Commit(tip)
	if err != nil {
		return out
	}
	tree, err := lookup.Tree(rec.TreeOID)
	if err != nil {
		return out
	}
	for _, e := range tree.Entries {
		if !e.IsDir || e.Mode == "160000" {
			continue // blobs and gitlinks have no subtree view
		}
		head, _, serr := s.Views.SubtreeHead(ctx, repo, model.AnchorPath("/"+e.Name), branch)
		if serr != nil || head.IsZero() {
			continue
		}
		out[model.AnchorPath("/"+e.Name)] = true
	}
	return out
}
