package object

import (
	"google.golang.org/grpc"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Register mounts the BlobService and DiffService RPCs.
func Register(s *grpc.Server, b *kit.Base) {
	gitalypb.RegisterBlobServiceServer(s, &blobService{Base: b})
	gitalypb.RegisterDiffServiceServer(s, &diffService{Base: b})
}
