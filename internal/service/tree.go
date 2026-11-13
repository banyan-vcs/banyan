package service

import (
	"context"
	"sort"

	"github.com/banyan-vcs/banyan/internal/model"
)

// TreeEntryItem is one directory entry of the structured tree listing —
// the JSON face the web code browser rides (the same semantics gitaly
// GetTreeEntries serves over gRPC).
type TreeEntryItem struct {
	Name string `json:"name"`
	Type string `json:"type"` // tree | blob | commit (gitlink)
	Mode string `json:"mode"`
	OID  string `json:"oid"`
	Size int    `json:"size,omitempty"` // blobs only, bytes
}

// ListTreeEntries lists one directory level at rev:path. rev "" = the
// repo's default branch; path "" or "/" = the view root. Directories sort
// before files, then by name — the tree-table order browsers expect.
func (s *Service) ListTreeEntries(ctx context.Context, req ResolveRequest) ([]TreeEntryItem, error) {
	res, err := s.Resolve(ctx, ResolveRequest{Repo: req.Repo, Sub: req.Sub, Rev: req.Rev, Path: req.Path})
	if err != nil {
		return nil, err
	}
	if res.Type != "tree" {
		return nil, invalid("%q is not a directory", req.Path)
	}
	t, err := s.Objects.Lookup(req.Repo).Tree(res.OID)
	if err != nil {
		return nil, err
	}
	items := make([]TreeEntryItem, 0, len(t.Entries))
	for _, e := range t.Entries {
		item := TreeEntryItem{Name: e.Name, Mode: e.Mode, OID: string(e.Target), Type: "blob"}
		switch {
		case e.IsDir:
			item.Type = "tree"
		case e.Mode == "160000":
			item.Type = "commit" // gitlink/submodule
		default:
			if typ, data, rerr := s.Objects.Raw(ctx, req.Repo, e.Target); rerr == nil && typ == model.BlobType {
				item.Size = len(data)
			}
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].dirFirst() != items[j].dirFirst() {
			return items[i].dirFirst()
		}
		return items[i].Name < items[j].Name
	})
	return items, nil
}

// dirFirst reports whether the entry sorts as a directory (dirs first).
func (t TreeEntryItem) dirFirst() bool { return t.Type == "tree" }
