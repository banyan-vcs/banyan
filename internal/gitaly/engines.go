// Engine exports for non-gRPC consumers (docs/gateway/service-api.md §6, docs/project/roadmap.md M2/M3): the
// JSON RPC surface serves path diff and blame from the same engines the
// gitaly RPCs ride — unifiedPatch is byte-pinned against `git diff` and
// blamePorcelain against `git blame` (T63), so the wire format a client
// gets over /api/v1 is the same text git itself produces.
package gitaly

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/gitaly/commit"
	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitaly/object"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/meta"
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
func Engines(ms meta.MetaStore, objects *storage.ObjectService, algo model.HashAlgo, svc *service.Service) *Server {
	b := &kit.Base{Meta: ms, Objects: objects, Algo: algo, Svc: svc}
	s := &Server{Base: b}
	s.git = gitwire.NewServer(ms, objects, algo)
	b.Git = s.git
	return s
}

// DiffText renders the git-format unified diff between two revisions
// (branch names, tags, or OIDs — the same resolution the gitaly RPCs use).
// path filters the output: an exact file path, or a directory prefix when
// it carries a trailing slash; "" = the whole tree diff.
func (s *Server) DiffText(ctx context.Context, repo, from, to, path string) (string, error) {
	return object.DiffText(ctx, s.Base, repo, from, to, path)
}

// BlameText renders `git blame --line-porcelain` text for one path at one
// revision: recursive first-parent walk with per-step diff attribution.
func (s *Server) BlameText(ctx context.Context, repo, rev, path string) (string, error) {
	return commit.BlameText(ctx, s.Base, repo, rev, path)
}
