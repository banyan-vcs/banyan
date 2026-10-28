package gitaly

import "github.com/banyan-vcs/banyan/internal/gitalypb"

// Pending services. Every gitaly service is registered from day one; the
// structs below answer codes.Unimplemented for RPCs not yet built, so a
// frontend sees a stable contract (clean per-RPC Unimplemented, never a
// connection failure). Coverage lands slice by slice — docs/19 tracks it.

type diffService struct {
	gitalypb.UnimplementedDiffServiceServer
	s *Server
}

type operationService struct {
	gitalypb.UnimplementedOperationServiceServer
	s *Server
}

type hookService struct {
	gitalypb.UnimplementedHookServiceServer
	s *Server
}

type conflictsService struct {
	gitalypb.UnimplementedConflictsServiceServer
	s *Server
}

type remoteService struct {
	gitalypb.UnimplementedRemoteServiceServer
	s *Server
}

type cleanupService struct {
	gitalypb.UnimplementedCleanupServiceServer
	s *Server
}

type objectPoolService struct {
	gitalypb.UnimplementedObjectPoolServiceServer
	s *Server
}

type internalGitalyService struct {
	gitalypb.UnimplementedInternalGitalyServer
	s *Server
}

type backupService struct {
	gitalypb.UnimplementedBackupServiceServer
	s *Server
}

type partitionService struct {
	gitalypb.UnimplementedPartitionServiceServer
	s *Server
}

type analysisService struct {
	gitalypb.UnimplementedAnalysisServiceServer
	s *Server
}

type praefectInfoService struct {
	gitalypb.UnimplementedPraefectInfoServiceServer
	s *Server
}

type refTransactionService struct {
	gitalypb.UnimplementedRefTransactionServer
	s *Server
}
