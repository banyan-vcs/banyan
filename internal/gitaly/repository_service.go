package gitaly

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// repositoryService implements the repo lifecycle slice of gitaly.
// RepositoryService: lifecycle (create/exists/delete) + identity probes.
// The maintenance and import surfaces (repack/fsck/bundle/fork…) are later
// slices — see docs/19 for the coverage matrix.
type repositoryService struct {
	gitalypb.UnimplementedRepositoryServiceServer
	s *Server
}

func (rs *repositoryService) RepositoryExists(ctx context.Context, req *gitalypb.RepositoryExistsRequest) (*gitalypb.RepositoryExistsResponse, error) {
	name, err := rs.s.repoName(req.GetRepository())
	if err != nil {
		return nil, err
	}
	_, gerr := rs.s.Meta.GetRepo(ctx, name)
	return &gitalypb.RepositoryExistsResponse{Exists: gerr == nil}, nil
}

func (rs *repositoryService) CreateRepository(ctx context.Context, req *gitalypb.CreateRepositoryRequest) (*gitalypb.CreateRepositoryResponse, error) {
	name, err := rs.s.repoName(req.GetRepository())
	if err != nil {
		return nil, err
	}
	switch of := req.GetObjectFormat(); of {
	case gitalypb.ObjectFormat_OBJECT_FORMAT_UNSPECIFIED, gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1:
	default:
		return nil, status.Error(codes.InvalidArgument, "gitaly: only sha1 object format is served")
	}
	if err := rs.s.Meta.CreateRepo(ctx, name, rs.s.Algo); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil, status.Errorf(codes.AlreadyExists, "gitaly: repository %q already exists", name)
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	if branch := string(req.GetDefaultBranch()); branch != "" && branch != "main" {
		if err := rs.s.Meta.SetDefaultBranch(ctx, name, branch); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	return &gitalypb.CreateRepositoryResponse{}, nil
}

func (rs *repositoryService) ObjectFormat(ctx context.Context, req *gitalypb.ObjectFormatRequest) (*gitalypb.ObjectFormatResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	info, err := rs.s.Meta.GetRepo(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &gitalypb.ObjectFormatResponse{Format: gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1}
	if info.Algo == model.SHA256 {
		out.Format = gitalypb.ObjectFormat_OBJECT_FORMAT_SHA256
	}
	return out, nil
}

func (rs *repositoryService) HasLocalBranches(ctx context.Context, req *gitalypb.HasLocalBranchesRequest) (*gitalypb.HasLocalBranchesResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	refs, err := rs.s.listRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "refs/heads/") {
			return &gitalypb.HasLocalBranchesResponse{Value: true}, nil
		}
	}
	return &gitalypb.HasLocalBranchesResponse{Value: false}, nil
}
