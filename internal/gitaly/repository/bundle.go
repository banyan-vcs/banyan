package repository

import (
	"context"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Bundle surface: v2 git bundles (header refs + one pack of the closure).
// CreateBundle/FromRefList write them, FetchBundle/FromBundle ingest them —
// the interchange format for backup, seed and offline transfer flows.

// CreateBundle streams the whole-repository bundle (all heads and tags).
func (rs *repositoryService) CreateBundle(req *gitalypb.CreateBundleRequest, stream gitalypb.RepositoryService_CreateBundleServer) error {
	ctx := stream.Context()
	name, err := rs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	refs, err := rs.BundleRefsOf(ctx, name)
	if err != nil {
		return err
	}
	w := kit.StreamWriter(func(p []byte) error {
		return stream.Send(&gitalypb.CreateBundleResponse{Data: p})
	})
	return rs.WriteBundle(ctx, name, refs, w)
}

// CreateBundleFromRefList streams a bundle restricted to refs matching the
// client-streamed patterns (git for-each-ref globs).
func (rs *repositoryService) CreateBundleFromRefList(stream gitalypb.RepositoryService_CreateBundleFromRefListServer) error {
	ctx := stream.Context()
	var name string
	var patterns []string
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = rs.ExistingRepo(ctx, req.GetRepository()); err != nil {
				return err
			}
		}
		for _, p := range req.GetPatterns() {
			patterns = append(patterns, strings.TrimSpace(string(p)))
		}
	}
	if len(patterns) == 0 {
		return status.Error(codes.InvalidArgument, "gitaly: no patterns given")
	}
	all, err := rs.BundleRefsOf(ctx, name)
	if err != nil {
		return err
	}
	var matched []kit.BundleRef
	for _, r := range all {
		for _, p := range patterns {
			if kit.GlobMatch(p, r.Ref) {
				matched = append(matched, r)
				break
			}
		}
	}
	w := kit.StreamWriter(func(p []byte) error {
		return stream.Send(&gitalypb.CreateBundleFromRefListResponse{Data: p})
	})
	return rs.WriteBundle(ctx, name, matched, w)
}

// FetchBundle ingests a client-streamed bundle into an existing repository.
func (rs *repositoryService) FetchBundle(stream gitalypb.RepositoryService_FetchBundleServer) error {
	ctx := stream.Context()
	var name string
	var data []byte
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = rs.ExistingRepo(ctx, req.GetRepository()); err != nil {
				return err
			}
		}
		data = append(data, req.GetData()...)
	}
	if err := rs.IngestBundle(ctx, name, data, false); err != nil {
		return err
	}
	return stream.SendAndClose(&gitalypb.FetchBundleResponse{})
}

// CreateRepositoryFromBundle creates the repository and seeds it from a
// client-streamed bundle in one call.
func (rs *repositoryService) CreateRepositoryFromBundle(stream gitalypb.RepositoryService_CreateRepositoryFromBundleServer) error {
	ctx := stream.Context()
	var name string
	var data []byte
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if err := rs.createRepositoryChecked(ctx, req.GetRepository()); err != nil {
				return err
			}
			name, err = rs.RepoName(req.GetRepository())
			if err != nil {
				return err
			}
		}
		data = append(data, req.GetData()...)
	}
	// UpdateHead on first seed: bundle HEAD names the default branch.
	if err := rs.IngestBundle(ctx, name, data, true); err != nil {
		_ = rs.Objects.DeleteRepo(ctx, name) // don't leave a half-seeded repo behind
		return err
	}
	return stream.SendAndClose(&gitalypb.CreateRepositoryFromBundleResponse{})
}

// createRepositoryChecked mirrors CreateRepository's validation and creation
// for the bundle-seed path.
func (rs *repositoryService) createRepositoryChecked(ctx context.Context, repo *gitalypb.Repository) error {
	name, err := rs.RepoName(repo)
	if err != nil {
		return err
	}
	if err := rs.Meta.CreateRepo(ctx, name, rs.Algo); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return status.Errorf(codes.AlreadyExists, "gitaly: repository %q already exists", name)
		}
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}
