package node

import (
	"google.golang.org/grpc"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Register mounts the node-plane services: server info, hooks, cleanup,
// object pools, praefect/partition/transactions, analysis.
func Register(s *grpc.Server, b *kit.Base) {
	gitalypb.RegisterServerServiceServer(s, &serverService{Base: b})
	gitalypb.RegisterHookServiceServer(s, &hookService{Base: b})
	gitalypb.RegisterCleanupServiceServer(s, &cleanupService{Base: b})
	gitalypb.RegisterObjectPoolServiceServer(s, &objectPoolService{Base: b})
	gitalypb.RegisterInternalGitalyServer(s, &internalGitalyService{Base: b})
	gitalypb.RegisterPartitionServiceServer(s, &partitionService{Base: b})
	gitalypb.RegisterAnalysisServiceServer(s, &analysisService{Base: b})
	gitalypb.RegisterPraefectInfoServiceServer(s, &praefectInfoService{Base: b})
	gitalypb.RegisterRefTransactionServer(s, &refTransactionService{Base: b})
}
