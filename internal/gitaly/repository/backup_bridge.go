package repository

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/gitaly/backup"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// The RepositoryService backup pair (server-side backups; the sink layout
// lives above). Skipped-repo semantics collapse into the manifest's
// non_existent record — the single-node sink always accepts the run.
func (rs *repositoryService) BackupRepository(ctx context.Context, in *gitalypb.BackupRepositoryRequest) (*gitalypb.BackupRepositoryResponse, error) {
	run := backup.Runner{Base: rs.Base}
	sink, err := run.SinkForNode()
	if err != nil {
		return nil, err
	}
	if err := run.CreateBackup(ctx, sink, in.GetRepository(), in.GetVanityRepository(),
		in.GetBackupId(), in.GetLatestBackupId(), in.GetPathPrefix(), in.GetIncremental()); err != nil {
		return nil, err
	}
	return &gitalypb.BackupRepositoryResponse{}, nil
}

func (rs *repositoryService) RestoreRepository(ctx context.Context, in *gitalypb.RestoreRepositoryRequest) (*gitalypb.RestoreRepositoryResponse, error) {
	run := backup.Runner{Base: rs.Base}
	sink, err := run.SinkForNode()
	if err != nil {
		return nil, err
	}
	if err := run.RestoreBackup(ctx, sink, in.GetRepository(), in.GetVanityRepository(),
		in.GetBackupId(), in.GetAlwaysCreate(), in.GetUseLatest(), in.GetPathPrefix()); err != nil {
		return nil, err
	}
	return &gitalypb.RestoreRepositoryResponse{}, nil
}
