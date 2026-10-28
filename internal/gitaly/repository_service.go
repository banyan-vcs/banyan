package gitaly

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
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

// RepositorySize reports the pack bytes backing the repo (the same number
// the /api/v1/repos/<repo>/size plane serves).
func (rs *repositoryService) RepositorySize(ctx context.Context, req *gitalypb.RepositorySizeRequest) (*gitalypb.RepositorySizeResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	size, _, err := rs.s.Svc.RepoSize(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.RepositorySizeResponse{Size: size}, nil
}

// GetObjectDirectorySize mirrors RepositorySize — banyan has one object
// plane, not split storage.
func (rs *repositoryService) GetObjectDirectorySize(ctx context.Context, req *gitalypb.GetObjectDirectorySizeRequest) (*gitalypb.GetObjectDirectorySizeResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	size, _, err := rs.s.Svc.RepoSize(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.GetObjectDirectorySizeResponse{Size: size}, nil
}

// WriteRef creates or moves a single ref (Rails' housekeeping path).
func (rs *repositoryService) WriteRef(ctx context.Context, req *gitalypb.WriteRefRequest) (*gitalypb.WriteRefResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	refName := string(req.GetRef())
	if !strings.HasPrefix(refName, "refs/") {
		return nil, status.Errorf(codes.InvalidArgument, "gitaly: ref %q must be fully qualified", refName)
	}
	oid, err := rs.s.resolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		return nil, err
	}
	if err := rs.s.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: refName, New: oid}); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
	}
	return &gitalypb.WriteRefResponse{}, nil
}

// FindMergeBase answers the first common ancestor of two revisions in
// commit-time order — the single-base form Rails' merge-request flow uses.
func (rs *repositoryService) FindMergeBase(ctx context.Context, req *gitalypb.FindMergeBaseRequest) (*gitalypb.FindMergeBaseResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	if len(req.GetRevisions()) < 2 {
		return nil, status.Error(codes.InvalidArgument, "gitaly: merge base needs two revisions")
	}
	a, err := rs.s.resolveRevision(ctx, name, string(req.GetRevisions()[0]))
	if err != nil {
		return nil, err
	}
	b, err := rs.s.resolveRevision(ctx, name, string(req.GetRevisions()[1]))
	if err != nil {
		return nil, err
	}
	ancestors := map[model.OID]bool{}
	err = rs.s.walkParents(ctx, name, []model.OID{a}, &walkFilter{}, func(oid model.OID, _ *commitRecord) bool {
		ancestors[oid] = true
		return true
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var base string
	err = rs.s.walkParents(ctx, name, []model.OID{b}, &walkFilter{}, func(oid model.OID, _ *commitRecord) bool {
		if ancestors[oid] {
			base = oid.String()
			return false
		}
		return true
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.FindMergeBaseResponse{Base: base}, nil
}
