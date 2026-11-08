package commit

import (
	"google.golang.org/grpc"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Register mounts the CommitService RPCs.
func Register(s *grpc.Server, b *kit.Base) {
	gitalypb.RegisterCommitServiceServer(s, &commitService{Base: b})
}
