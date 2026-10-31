package gitaly

import "github.com/banyan-vcs/banyan/internal/gitalypb"

// Service structs. Every gitaly service is registered from day one; the
// UnimplementedXServer embeds are the compile-time completeness net — an
// RPC added by a future protocol bump surfaces as a clean per-RPC
// Unimplemented instead of a connection failure. The implementations live
// in the sibling files; docs/19 tracks the coverage ledger (163/163 live
// against vendored v19.5.2).

type conflictsService struct {
	gitalypb.UnimplementedConflictsServiceServer
	s *Server
}

type remoteService struct {
	gitalypb.UnimplementedRemoteServiceServer
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

type partitionService struct {
	gitalypb.UnimplementedPartitionServiceServer
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
