package repository

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	gittransport "github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitaly/operations"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// Remote-fetch surface. FetchSourceBranch copies a branch inside the banyan
// object plane (same-instance, GitLab's MR precondition probe). FetchRemote
// and CreateRepositoryFromURL speak the real git smart-HTTP protocol through
// go-git's transport client — the only place banyan acts as a git *client*.

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
		current, ok, _ := operations.TimelineHeadOf(ctx, rs.Base, name, operations.FullRefTimeline(name, refName))
		if ok && current.String() != expected {
			return nil, status.Error(codes.FailedPrecondition, "gitaly: unexpected old value at target ref")
		}
	}
	if err := rs.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: refName, New: tip}); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
	}
	return &gitalypb.FetchSourceBranchResponse{}, nil
}

// fetchRemoteRefs pulls every advertised ref (heads, tags) of a smart-HTTP
// remote into the repository: one upload-pack exchange, the pack spooled to
// disk and ingested as one shard, refs force-set afterwards.
func (rs *repositoryService) fetchRemoteRefs(ctx context.Context, repo string, url, authHeader string, includeTags bool) error {
	ep, err := gittransport.NewEndpoint(url)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: invalid remote url: %v", err)
	}
	cl, err := transportclient.NewClient(ep)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: unsupported remote protocol %q", ep.Protocol)
	}
	sess, err := cl.NewUploadPackSession(ep, kit.RawAuth{Value: authHeader})
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: remote session: %v", err)
	}
	defer sess.Close()
	refs, err := sess.AdvertisedReferences()
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: remote refs: %v", err)
	}
	req := packp.NewUploadPackRequest()
	for name, hash := range refs.References {
		if name == "HEAD" {
			continue
		}
		if !includeTags && strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		req.Wants = append(req.Wants, hash)
	}
	if len(req.Wants) == 0 {
		return nil
	}
	reader, err := sess.UploadPack(ctx, req)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: remote upload-pack: %v", err)
	}
	defer reader.Close()
	pack, err := io.ReadAll(reader)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: remote pack: %v", err)
	}
	dir, err := os.MkdirTemp("", "banyan-fetch-*")
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	defer os.RemoveAll(dir)
	packPath := filepath.Join(dir, "remote.pack")
	if err := os.WriteFile(packPath, pack, 0o644); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if _, err := rs.Objects.IngestPackFile(ctx, repo, packPath, int64(len(pack))); err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: remote pack rejected: %v", err)
	}
	for name, hash := range refs.References {
		if name == "HEAD" || !strings.HasPrefix(name, "refs/") {
			continue
		}
		if !includeTags && strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		if err := rs.Svc.UpdateRef(ctx, service.RefUpdate{
			Repo: repo, Name: name, New: model.OID(hash.String())}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	return nil
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
	if strings.HasPrefix(remoteParams.GetUrl(), "ssh://") ||
		strings.HasPrefix(remoteParams.GetUrl(), "git@") {
		return nil, status.Error(codes.Unimplemented, "gitaly: ssh remotes are not served")
	}
	if err := rs.fetchRemoteRefs(ctx, name, remoteParams.GetUrl(),
		remoteParams.GetHttpAuthorizationHeader(), !req.GetNoTags()); err != nil {
		return nil, err
	}
	return &gitalypb.FetchRemoteResponse{}, nil
}

// CreateRepositoryFromURL creates the repository and seeds it from a
// smart-HTTP remote in one call.
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
	if err := rs.fetchRemoteRefs(ctx, name, req.GetUrl(), req.GetHttpAuthorizationHeader(), true); err != nil {
		_ = rs.Objects.DeleteRepo(ctx, name)
		return nil, err
	}
	return &gitalypb.CreateRepositoryFromURLResponse{}, nil
}
