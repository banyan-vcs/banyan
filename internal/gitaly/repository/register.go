package repository

import (
	"google.golang.org/grpc"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Register mounts the RepositoryService RPCs (lifecycle, maintenance,
// search, archive, bundles, snapshots, fast-export, fetch-remote).
func Register(s *grpc.Server, b *kit.Base) {
	gitalypb.RegisterRepositoryServiceServer(s, &repositoryService{Base: b})
}
