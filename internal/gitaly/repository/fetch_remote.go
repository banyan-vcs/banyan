package repository

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/operations"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/importer"
	"github.com/banyan-vcs/banyan/internal/service"
)

// Remote-fetch surface. FetchSourceBranch copies a branch inside the banyan
// object plane (same-instance, GitLab's MR precondition probe). FetchRemote
// and CreateRepositoryFromURL speak the real git smart-HTTP protocol through
// internal/importer — the only place banyan acts as a git *client*.

// FetchSourceBranch copies source_branch from source_repository onto
// target_ref of repository, closing the source's object closure into the
// target's object plane first.
func (rs *repositoryService) FetchSourceBranch(ctx context.Context, req *gitalypb.FetchSourceBranchRequest) (*gitalypb.FetchSourceBranchResponse, error) {
	name, err := rs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	source, err := rs.ExistingRepo(ctx, req.GetSourceRepository())
	if err != nil {
		return nil, err
	}
	branch := strings.TrimPrefix(string(req.GetSourceBranch()), "refs/heads/")
	tip, err := rs.ResolveRevision(ctx, source, branch)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "gitaly: source branch %q not found", string(req.GetSourceBranch()))
	}
	if err := rs.CopyClosure(ctx, source, name, tip); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	refName := string(req.GetTargetRef())
	if !strings.HasPrefix(refName, "refs/") {
		return nil, status.Errorf(codes.InvalidArgument, "gitaly: target ref %q must be fully qualified", refName)
	}
	if expected := req.GetExpectedTargetOldOid(); expected != "" {
		if current, ok, _ := operations.TimelineHeadOf(ctx, rs.Base, name, operations.FullRefTimeline(name, refName)); ok && current.String() != expected {
			return nil, status.Error(codes.FailedPrecondition, "gitaly: unexpected old value at target ref")
		}
	}
	if err := rs.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: refName, New: tip}); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
	}
	return &gitalypb.FetchSourceBranchResponse{}, nil
}

// importStatus preserves the wire surface's code mapping around the shared
// engine (InvalidArgument before the wire, FailedPrecondition on it,
// Unimplemented for the registered ssh gap).
func importStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, importer.ErrInvalidURL):
		return status.Error(codes.InvalidArgument, "gitaly: "+err.Error())
	case errors.Is(err, importer.ErrSSHRemote):
		return status.Error(codes.Unimplemented, "gitaly: "+err.Error())
	default:
		return status.Error(codes.FailedPrecondition, "gitaly: "+err.Error())
	}
}

// FetchRemote pulls a smart-HTTP remote into an existing repository.
// SSH remotes (ssh_key/known_hosts) are not served — registered in docs/19.
func (rs *repositoryService) FetchRemote(ctx context.Context, req *gitalypb.FetchRemoteRequest) (*gitalypb.FetchRemoteResponse, error) {
	name, err := rs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	remoteParams := req.GetRemoteParams()
	if remoteParams == nil || remoteParams.GetUrl() == "" {
		return nil, status.Error(codes.InvalidArgument, "gitaly: remote_params.url is required")
	}
	if _, err := importer.Fetch(ctx, rs.Objects, rs.Svc, name, remoteParams.GetUrl(),
		remoteParams.GetHttpAuthorizationHeader(), !req.GetNoTags()); err != nil {
		return nil, importStatus(err)
	}
	return &gitalypb.FetchRemoteResponse{}, nil
}

// CreateRepositoryFromURL creates the repository and seeds it from a
// smart-HTTP remote in one call, adopting the remote's HEAD as the default
// branch when it is not banyan's own default.
func (rs *repositoryService) CreateRepositoryFromURL(ctx context.Context, req *gitalypb.CreateRepositoryFromURLRequest) (*gitalypb.CreateRepositoryFromURLResponse, error) {
	if req.GetUrl() == "" {
		return nil, status.Error(codes.InvalidArgument, "gitaly: url is required")
	}
	if err := rs.createRepositoryChecked(ctx, req.GetRepository()); err != nil {
		return nil, err
	}
	name, err := rs.RepoName(req.GetRepository())
	if err != nil {
		return nil, err
	}
	res, err := importer.Fetch(ctx, rs.Objects, rs.Svc, name, req.GetUrl(), req.GetHttpAuthorizationHeader(), true)
	if err != nil {
		_ = rs.Objects.DeleteRepo(ctx, name)
		return nil, importStatus(err)
	}
	if res.DefaultBranch != "" && res.DefaultBranch != "main" {
		_ = rs.Meta.SetDefaultBranch(ctx, name, res.DefaultBranch)
	}
	return &gitalypb.CreateRepositoryFromURLResponse{}, nil
}
