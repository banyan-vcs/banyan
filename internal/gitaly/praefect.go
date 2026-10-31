package gitaly

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The praefect-family services, answered cluster-of-one (docs/19). These
// RPCs belong to praefect's multi-node topology: routing tables, replica
// verification, Raft-less two-phase ref transactions, partition backups.
// banyan's deployment shape is a single node, so the cluster it describes
// has exactly one member — this storage, always primary, always healthy,
// never in dataloss. A caller speaking the praefect dialect gets a
// self-consistent single-replica answer instead of Unimplemented.

// --- PraefectInfoService ---

func (ps *praefectInfoService) virtualStorageGuard(vs string) error {
	if vs != "" && vs != ps.s.Opts.StorageName {
		return status.Errorf(codes.NotFound,
			"gitaly: virtual storage %q not found (this node answers as %q)", vs, ps.s.Opts.StorageName)
	}
	return nil
}

// RepositoryReplicas answers the placement: this storage is the primary
// and the sole replica, carrying the repository's checksum.
func (ps *praefectInfoService) RepositoryReplicas(ctx context.Context, in *gitalypb.RepositoryReplicasRequest) (*gitalypb.RepositoryReplicasResponse, error) {
	if err := ps.virtualStorageGuard(in.GetRepository().GetStorageName()); err != nil {
		return nil, err
	}
	name, err := ps.s.existingRepo(ctx, in.GetRepository())
	if err != nil {
		return nil, err
	}
	repo := &gitalypb.Repository{
		StorageName:  ps.s.Opts.StorageName,
		RelativePath: in.GetRepository().GetRelativePath(),
	}
	details := &gitalypb.RepositoryReplicasResponse_RepositoryDetails{
		Repository: repo,
		Checksum:   repoChecksum(ctx, ps.s, name),
	}
	return &gitalypb.RepositoryReplicasResponse{
		Primary:  details,
		Replicas: []*gitalypb.RepositoryReplicasResponse_RepositoryDetails{details},
	}, nil
}

// SetAuthoritativeStorage pins the primary. With one storage the only
// valid assignment is the node itself.
func (ps *praefectInfoService) SetAuthoritativeStorage(ctx context.Context, in *gitalypb.SetAuthoritativeStorageRequest) (*gitalypb.SetAuthoritativeStorageResponse, error) {
	if err := ps.virtualStorageGuard(in.GetVirtualStorage()); err != nil {
		return nil, err
	}
	if in.GetAuthoritativeStorage() != ps.s.Opts.StorageName {
		return nil, status.Errorf(codes.NotFound,
			"gitaly: storage %q not found (this node answers as %q)",
			in.GetAuthoritativeStorage(), ps.s.Opts.StorageName)
	}
	if _, err := ps.s.existingRepo(ctx, &gitalypb.Repository{
		StorageName:  ps.s.Opts.StorageName,
		RelativePath: in.GetRelativePath(),
	}); err != nil {
		return nil, err
	}
	return &gitalypb.SetAuthoritativeStorageResponse{}, nil
}

// MarkUnverified flags replicas for re-verification. Nothing is ever
// unverified on a single node, so zero replicas get marked.
func (ps *praefectInfoService) MarkUnverified(ctx context.Context, in *gitalypb.MarkUnverifiedRequest) (*gitalypb.MarkUnverifiedResponse, error) {
	if vs := in.GetVirtualStorage(); vs != "" {
		if err := ps.virtualStorageGuard(vs); err != nil {
			return nil, err
		}
	}
	if st := in.GetStorage(); st != nil {
		if err := ps.virtualStorageGuard(st.VirtualStorage); err != nil {
			return nil, err
		}
	}
	return &gitalypb.MarkUnverifiedResponse{ReplicasMarked: 0}, nil
}

// SetReplicationFactor sizes the replica set. One storage can host exactly
// one replica — anything else needs storages this node does not have.
func (ps *praefectInfoService) SetReplicationFactor(ctx context.Context, in *gitalypb.SetReplicationFactorRequest) (*gitalypb.SetReplicationFactorResponse, error) {
	if err := ps.virtualStorageGuard(in.GetVirtualStorage()); err != nil {
		return nil, err
	}
	if in.GetReplicationFactor() < 1 {
		return nil, status.Error(codes.InvalidArgument, "gitaly: replication factor must be at least 1")
	}
	if in.GetReplicationFactor() > 1 {
		return nil, status.Errorf(codes.InvalidArgument,
			"gitaly: replication factor %d exceeds the storages in %q (1)",
			in.GetReplicationFactor(), ps.s.Opts.StorageName)
	}
	if _, err := ps.s.existingRepo(ctx, &gitalypb.Repository{
		StorageName:  ps.s.Opts.StorageName,
		RelativePath: in.GetRelativePath(),
	}); err != nil {
		return nil, err
	}
	return &gitalypb.SetReplicationFactorResponse{}, nil
}

// GetRepositoryMetadata describes the placement record: primary, the
// pack-count generation (banyan's monotone-ish write counter), one
// assigned healthy replica. Lookup by path; banyan has no numeric
// repository-id plane, so ID queries answer NotFound and the response
// carries repository_id 0.
func (ps *praefectInfoService) GetRepositoryMetadata(ctx context.Context, in *gitalypb.GetRepositoryMetadataRequest) (*gitalypb.GetRepositoryMetadataResponse, error) {
	if id := in.GetRepositoryId(); id != 0 {
		return nil, status.Errorf(codes.NotFound,
			"gitaly: repository ID %d not found (repositories are addressed by path on this node)", id)
	}
	path := in.GetPath()
	if path == nil {
		return nil, status.Error(codes.InvalidArgument, "gitaly: a query is required")
	}
	if err := ps.virtualStorageGuard(path.VirtualStorage); err != nil {
		return nil, err
	}
	name, err := ps.s.existingRepo(ctx, &gitalypb.Repository{
		StorageName:  ps.s.Opts.StorageName,
		RelativePath: path.RelativePath,
	})
	if err != nil {
		return nil, err
	}
	packs, err := ps.s.Meta.ListPacks(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.GetRepositoryMetadataResponse{
		VirtualStorage: ps.s.Opts.StorageName,
		RelativePath:   path.RelativePath,
		ReplicaPath:    path.RelativePath,
		Primary:        ps.s.Opts.StorageName,
		Generation:     int64(len(packs)),
		Replicas: []*gitalypb.GetRepositoryMetadataResponse_Replica{{
			Storage:    ps.s.Opts.StorageName,
			Assigned:   true,
			Generation: int64(len(packs)),
			Healthy:    true,
		}},
	}, nil
}

// Dataloss reports repositories with too few healthy replicas — an empty
// list for a cluster of one healthy node.
func (ps *praefectInfoService) Dataloss(in *gitalypb.DatalossRequest, stream gitalypb.PraefectInfoService_DatalossServer) error {
	if err := ps.virtualStorageGuard(in.GetVirtualStorage()); err != nil {
		return err
	}
	// A cluster of one healthy node has no dataloss to report: the
	// stream closes empty.
	return nil
}

// --- PartitionService ---

// ListPartitions answers the storage's partitions — one per repository
// (banyan's per-repo partitioning: every repo is its own failure and
// backup domain), sorted by ID with the pagination cursor carrying the
// last emitted partition.
func (ps *partitionService) ListPartitions(ctx context.Context, in *gitalypb.ListPartitionsRequest) (*gitalypb.ListPartitionsResponse, error) {
	if in.GetStorageName() != "" && in.GetStorageName() != ps.s.Opts.StorageName {
		return nil, status.Errorf(codes.NotFound, "gitaly: unknown storage %q", in.GetStorageName())
	}
	repos, err := ps.s.Meta.ListRepos(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	limit, offset := paginationWindow(in.GetPaginationParams())
	if offset > uint32(len(repos)) {
		offset = uint32(len(repos))
	}
	window := repos[offset:]
	if limit > 0 && uint32(len(window)) > limit {
		window = window[:limit]
	}
	var partitions []*gitalypb.Partition
	for _, repo := range window {
		partitions = append(partitions, &gitalypb.Partition{Id: repo.ID})
	}
	cursor := ""
	if len(partitions) > 0 && (limit == 0 || uint32(len(partitions)) == limit) {
		cursor = partitions[len(partitions)-1].GetId()
	}
	return &gitalypb.ListPartitionsResponse{
		Partitions:       partitions,
		PaginationCursor: &gitalypb.PaginationCursor{NextCursor: cursor},
	}, nil
}

// BackupPartition writes the partition's repository state as one TAR into
// the backup sink (upstream: partition-backups/<storage>/<partition>/
// <LSN>.tar). banyan names the member by the repository's checksum — the
// stand-in for the write LSN: a partition that has not changed backs up to
// the same name and answers AlreadyExists, exactly like an unchanged LSN.
func (ps *partitionService) BackupPartition(ctx context.Context, in *gitalypb.BackupPartitionRequest) (*gitalypb.BackupPartitionResponse, error) {
	sink, err := ps.s.backupSinkOf()
	if err != nil {
		return nil, err
	}
	if in.GetStorageName() != "" && in.GetStorageName() != ps.s.Opts.StorageName {
		return nil, status.Errorf(codes.NotFound, "gitaly: unknown storage %q", in.GetStorageName())
	}
	repo := &gitalypb.Repository{StorageName: ps.s.Opts.StorageName, RelativePath: in.GetPartitionId()}
	name, err := ps.s.existingRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	rs := &repositoryService{s: ps.s}
	rel := fmt.Sprintf("partition-backups/%s/%s/%s.tar", ps.s.Opts.StorageName, in.GetPartitionId(), repoChecksum(ctx, ps.s, name))
	if sink.exists(rel) {
		return nil, status.Error(codes.AlreadyExists, "there is an up-to-date backup for the given partition")
	}
	var tarBuf bytes.Buffer
	if err := rs.writeSnapshot(ctx, name, &tarBuf); err != nil {
		return nil, err
	}
	if err := sink.write(rel, tarBuf.Bytes()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.BackupPartitionResponse{}, nil
}

// --- RefTransaction ---

// VoteTransaction records a node's vote on a reference transaction. The
// single voter always carries the motion: COMMITTED (prepared votes
// acknowledge too — one node cannot be in two phases at once).
func (rt *refTransactionService) VoteTransaction(ctx context.Context, in *gitalypb.VoteTransactionRequest) (*gitalypb.VoteTransactionResponse, error) {
	if _, err := rt.s.existingRepo(ctx, in.GetRepository()); err != nil {
		return nil, err
	}
	return &gitalypb.VoteTransactionResponse{
		State: gitalypb.VoteTransactionResponse_COMMIT,
	}, nil
}

// StopTransaction retires a transaction. Nothing outlives the call on a
// single node — every vote commits inline — so the stop acknowledges.
func (rt *refTransactionService) StopTransaction(ctx context.Context, in *gitalypb.StopTransactionRequest) (*gitalypb.StopTransactionResponse, error) {
	if _, err := rt.s.existingRepo(ctx, in.GetRepository()); err != nil {
		return nil, err
	}
	return &gitalypb.StopTransactionResponse{}, nil
}
