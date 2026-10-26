package service

import (
	"context"
	"sort"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Diff is the path-level reference diff (docs/05 M2 item, serving the code
// review surface): two refs of one (subtree) view compared by walking both
// root trees recursively — added / deleted / modified files with their blob
// OIDs. Line-level rendering is the caller's concern; this stays the single
// authority for "what changed, where".
type DiffRequest struct {
	Repo string `json:"repo"`
	Sub  string `json:"sub,omitempty"` // subtree view (review anchor)
	Base string `json:"base"`          // branch name (the review's base)
	Head string `json:"head"`          // branch name (the review's source)
}

type FileDiff struct {
	Path    string    `json:"path"`
	Status  string    `json:"status"` // added | deleted | modified
	BaseOID model.OID `json:"base_oid,omitempty"`
	HeadOID model.OID `json:"head_oid,omitempty"`
}

type DiffResult struct {
	Base     string     `json:"base"`
	Head     string     `json:"head"`
	BaseHead model.OID  `json:"base_head"` // the base view's commit (zero = unborn)
	HeadHead model.OID  `json:"head_head"` // the source view's commit (zero = unborn)
	Files    []FileDiff `json:"files"`
}

// maxDiffEntries bounds the walk: a review diff beyond this is a "use the
// CLI" case, not a JSON payload.
const maxDiffEntries = 10000

func (s *Service) Diff(ctx context.Context, req DiffRequest) (*DiffResult, error) {
	sub, err := parseSub(req.Sub)
	if err != nil {
		return nil, err
	}
	if req.Base == "" || req.Head == "" {
		return nil, invalid("base and head refs required")
	}
	baseHead, baseTree, _, err := s.viewHead(ctx, req.Repo, sub, req.Base)
	if err != nil {
		return nil, err
	}
	headHead, headTree, _, err := s.viewHead(ctx, req.Repo, sub, req.Head)
	if err != nil {
		return nil, err
	}

	baseFiles, err := s.flattenTree(ctx, req.Repo, baseTree)
	if err != nil {
		return nil, err
	}
	headFiles, err := s.flattenTree(ctx, req.Repo, headTree)
	if err != nil {
		return nil, err
	}

	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	if len(paths) > maxDiffEntries {
		return nil, invalid("diff spans %d files (cap %d)", len(paths), maxDiffEntries)
	}
	// Unborn sides follow git's diff-against-empty semantics (an unborn head
	// reads as all-added, an unborn base as all-deleted); callers that need
	// "the ref must exist" check the *_head OIDs.
	out := &DiffResult{Base: req.Base, Head: req.Head, BaseHead: baseHead, HeadHead: headHead, Files: []FileDiff{}}
	for p := range paths {
		b, h := baseFiles[p], headFiles[p]
		switch {
		case b == "":
			out.Files = append(out.Files, FileDiff{Path: p, Status: "added", HeadOID: model.OID(h)})
		case h == "":
			out.Files = append(out.Files, FileDiff{Path: p, Status: "deleted", BaseOID: model.OID(b)})
		case b != h:
			out.Files = append(out.Files, FileDiff{Path: p, Status: "modified", BaseOID: model.OID(b), HeadOID: model.OID(h)})
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, nil
}

// flattenTree walks a root tree into path → blob-OID (submodules are
// recorded by their gitlink OID; they are content for diff purposes too).
func (s *Service) flattenTree(ctx context.Context, repo string, root model.OID) (map[string]string, error) {
	out := map[string]string{}
	if root.IsZero() {
		return out, nil // empty side (e.g. unborn base): everything is added
	}
	lookup := s.Objects.Lookup(repo)
	type frame struct {
		tree   model.OID
		prefix string
	}
	stack := []frame{{tree: root}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		t, err := lookup.Tree(f.tree)
		if err != nil {
			return nil, err
		}
		for _, e := range t.Entries {
			path := e.Name
			if f.prefix != "" {
				path = f.prefix + "/" + e.Name
			}
			if e.IsDir {
				stack = append(stack, frame{tree: e.Target, prefix: path})
				continue
			}
			out[path] = string(e.Target)
		}
	}
	return out, nil
}
