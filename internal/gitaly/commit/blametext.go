package commit

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
)

// BlameText renders `git blame --line-porcelain` text for one path at one
// revision: recursive first-parent walk with per-step diff attribution.
func BlameText(ctx context.Context, b *kit.Base, repo, rev, path string) (string, error) {
	cs := &commitService{Base: b}
	tip, err := b.ResolveRevision(ctx, repo, rev)
	if err != nil {
		return "", err
	}
	lines, chain, owner, origNo, err := cs.blameLines(ctx, repo, tip, path)
	if err != nil {
		return "", err
	}
	return blamePorcelain(lines, chain, owner, origNo, path), nil
}
