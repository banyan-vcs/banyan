package repository

import (
	"context"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Custom hooks tarball surface. banyan's executable hook plane is the HTTP
// callback registry (docs/gateway/git-compat.md §7), not the on-disk custom_hooks tree — but
// the tarball round-trip keeps gitaly-side backup/restore portable: Get
// answers what was stored (empty stream when nothing was), Set/Restore
// persist the bytes verbatim in the metadata plane.

// GetCustomHooks streams the stored custom-hooks tarball; an empty stream
// answers "no hooks present" (upstream: an empty tar is equivalent).
func (rs *repositoryService) GetCustomHooks(req *gitalypb.GetCustomHooksRequest, stream gitalypb.RepositoryService_GetCustomHooksServer) error {
	ctx := stream.Context()
	name, err := rs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	data, found, err := rs.Meta.GetRepoBlob(ctx, name, kit.CustomHooksKey)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if !found || len(data) == 0 {
		return nil
	}
	return kit.StreamBytes(data, func(p []byte) error {
		return stream.Send(&gitalypb.GetCustomHooksResponse{Data: p})
	})
}

// hooksChunk is the shared shape of the Set/Restore request messages.
type hooksChunk interface {
	GetRepository() *gitalypb.Repository
	GetData() []byte
}

// storeHooksTarball drains a client stream whose first message carries the
// repository and persists the concatenated bytes as the hooks tarball.
func storeHooksTarball[T hooksChunk](ctx context.Context, rs *repositoryService,
	recv func() (T, error), done func() error) error {
	var name string
	var data []byte
	first := true
	for {
		req, err := recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if first {
			first = false
			if name, err = rs.ExistingRepo(ctx, req.GetRepository()); err != nil {
				return err
			}
		}
		data = append(data, req.GetData()...)
	}
	if first {
		return status.Error(codes.InvalidArgument, "gitaly: repository is required")
	}
	if err := rs.Meta.SetRepoBlob(ctx, name, kit.CustomHooksKey, data); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return done()
}

// SetCustomHooks persists a client-streamed tarball, replacing any previous
// one (last write wins, matching the on-disk semantics).
func (rs *repositoryService) SetCustomHooks(stream gitalypb.RepositoryService_SetCustomHooksServer) error {
	return storeHooksTarball(stream.Context(), rs,
		stream.Recv, func() error {
			return stream.SendAndClose(&gitalypb.SetCustomHooksResponse{})
		})
}

// RestoreCustomHooks is the backup-restore spelling of the same operation.
func (rs *repositoryService) RestoreCustomHooks(stream gitalypb.RepositoryService_RestoreCustomHooksServer) error {
	return storeHooksTarball(stream.Context(), rs,
		stream.Recv, func() error {
			return stream.SendAndClose(&gitalypb.RestoreCustomHooksResponse{})
		})
}
