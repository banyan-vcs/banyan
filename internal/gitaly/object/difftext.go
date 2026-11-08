package object

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
)

// DiffText renders the git-format unified diff between two revisions
// (branch names, tags, or OIDs — the same resolution the gitaly RPCs use).
// path filters the output: an exact file path, or a directory prefix when
// it carries a trailing slash; "" = the whole tree diff.
func DiffText(ctx context.Context, b *kit.Base, repo, from, to, path string) (string, error) {
	ds := &diffService{Base: b}
	changes, err := ds.diffBetween(ctx, repo, from, to)
	if err != nil {
		return "", err
	}
	var out []byte
	for _, c := range changes {
		if !pathMatch(path, c.Path) {
			continue
		}
		out = append(out, ds.gitFormatPatch(ctx, repo, c)...)
	}
	return string(out), nil
}

// pathMatch: "" matches everything; a trailing slash matches the directory
// prefix; otherwise the exact path.
func pathMatch(filter, path string) bool {
	if filter == "" || filter == path {
		return true
	}
	if len(filter) > 1 && filter[len(filter)-1] == '/' {
		return len(path) > len(filter) && path[:len(filter)] == filter
	}
	return false
}
