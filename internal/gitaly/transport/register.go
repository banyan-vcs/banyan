package transport

import (
	"google.golang.org/grpc"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Register mounts the SmartHTTPService and SSHService RPCs (the wire
// faces bridging onto the internal gitwire instance).
func Register(s *grpc.Server, b *kit.Base) {
	gitalypb.RegisterSmartHTTPServiceServer(s, &smartHTTPService{Base: b})
	gitalypb.RegisterSSHServiceServer(s, &sshService{Base: b})
}
