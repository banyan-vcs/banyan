package operations

import (
	"google.golang.org/grpc"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// New builds the OperationService value (family tests reach its
// unexported helpers through it).
func New(b *kit.Base) *operationService { return &operationService{Base: b} }

// Register mounts the OperationService (merge/rebase/cherry-pick/revert,
// patches, submodules) and ConflictsService RPCs.
func Register(s *grpc.Server, b *kit.Base) {
	gitalypb.RegisterOperationServiceServer(s, New(b))
	gitalypb.RegisterConflictsServiceServer(s, &conflictsService{Base: b})
}
