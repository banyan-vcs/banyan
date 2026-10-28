package gitaly

import (
	"context"
	"crypto/ed25519"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// serverService implements gitaly.ServerService: node identity and health.
type serverService struct {
	gitalypb.UnimplementedServerServiceServer
	s *Server
}

func (ss *serverService) ServerInfo(context.Context, *gitalypb.ServerInfoRequest) (*gitalypb.ServerInfoResponse, error) {
	return &gitalypb.ServerInfoResponse{
		ServerVersion: ss.s.Opts.ReportedVersion,
		GitVersion:    GitVersionReported,
		StorageStatuses: []*gitalypb.ServerInfoResponse_StorageStatus{{
			StorageName: ss.s.Opts.StorageName,
			Readable:    true,
			Writeable:   true,
		}},
	}, nil
}

func (ss *serverService) ReadinessCheck(context.Context, *gitalypb.ReadinessCheckRequest) (*gitalypb.ReadinessCheckResponse, error) {
	return &gitalypb.ReadinessCheckResponse{
		Result: &gitalypb.ReadinessCheckResponse_OkResponse{OkResponse: &gitalypb.ReadinessCheckResponse_Ok{}},
	}, nil
}

func (ss *serverService) ServerSignature(context.Context, *gitalypb.ServerSignatureRequest) (*gitalypb.ServerSignatureResponse, error) {
	pubBytes, err := openSSHPublicKey(ss.s.signingKey.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, err
	}
	return &gitalypb.ServerSignatureResponse{PublicKey: pubBytes}, nil
}

func (ss *serverService) DiskStatistics(context.Context, *gitalypb.DiskStatisticsRequest) (*gitalypb.DiskStatisticsResponse, error) {
	// Byte-level filesystem stats are a monitoring nicety; the object plane
	// reports usage through /api/v1/repos/<repo>/size. Wired when the ops
	// plane grows a volume probe (docs/19 roadmap).
	return &gitalypb.DiskStatisticsResponse{
		StorageStatuses: []*gitalypb.DiskStatisticsResponse_StorageStatus{{
			StorageName: ss.s.Opts.StorageName,
		}},
	}, nil
}
