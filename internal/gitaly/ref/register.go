package ref

import (
	"google.golang.org/grpc"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Register mounts the RefService and RemoteService RPCs.
func Register(s *grpc.Server, b *kit.Base) {
	gitalypb.RegisterRefServiceServer(s, &refService{Base: b})
	gitalypb.RegisterRemoteServiceServer(s, &remoteService{Base: b})
}
