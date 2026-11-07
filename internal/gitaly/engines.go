// Engine exports for non-gRPC consumers (docs/05 §6, docs/14 M2/M3): the
// JSON RPC surface serves path diff and blame from the same engines the
// gitaly RPCs ride — unifiedPatch is byte-pinned against `git diff` and
// blamePorcelain against `git blame` (T63), so the wire format a client
// gets over /api/v1 is the same text git itself produces.
package gitaly

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Engines answers a listener-less Server value carrying just the metadata,
// object plane and service layer — enough to run the diff/blame engines.
// It registers nothing, listens nowhere, and generates no keys. The
// internal gitwire instance comes along for the ride: revision resolution
// consults its advertisement (projection refs included), so engine answers
// agree with every other face.
func Engines(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo, svc *service.Service) *Server {
	return &Server{Meta: meta, Objects: objects, Algo: algo, Svc: svc,
		git: gitwire.NewServer(meta, objects, algo)}
}

// DiffText renders the git-format unified diff between two revisions
// (branch names, tags, or OIDs — the same resolution the gitaly RPCs use).
// path filters the output: an exact file path, or a directory prefix when
// it carries a trailing slash; "" = the whole tree diff.
func (s *Server) DiffText(ctx context.Context, repo, from, to, path string) (string, error) {
	ds := &diffService{s: s}
	changes, err := ds.diffBetween(ctx, repo, from, to)
	if err != nil {
		return "", err
	}
	var b []byte
	for _, c := range changes {
		if !pathMatch(path, c.path) {
			continue
		}
		b = append(b, ds.gitFormatPatch(ctx, repo, c)...)
	}
	return string(b), nil
}

// BlameText renders `git blame --line-porcelain` text for one path at one
// revision: recursive first-parent walk with per-step diff attribution.
func (s *Server) BlameText(ctx context.Context, repo, rev, path string) (string, error) {
	cs := &commitService{s: s}
	tip, err := s.resolveRevision(ctx, repo, rev)
	if err != nil {
		return "", err
	}
	lines, chain, owner, origNo, err := cs.blameLines(ctx, repo, tip, path)
	if err != nil {
		return "", err
	}
	return blamePorcelain(lines, chain, owner, origNo, path), nil
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
