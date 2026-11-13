package gitaly

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// The praefect family answers cluster-of-one (docs/integration/gitaly-compat.md): one storage, one
// primary, one healthy replica, no dataloss, one-replica transactions that
// always commit, and per-repo partitions whose backup name derives from
// the repository checksum.
func TestGitalyPraefectFamily(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	pi := gitalypb.NewPraefectInfoServiceClient(conn)
	pt := gitalypb.NewPartitionServiceClient(conn)
	tx := gitalypb.NewRefTransactionClient(conn)

	repoName := "praefect-one"
	seedHistory(t, svc, repoName,
		map[string]string{"a.txt": "a"}, map[string]string{"a.txt": "b"})
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}

	t.Run("repository replicas", func(t *testing.T) {
		reps, err := pi.RepositoryReplicas(ctx, &gitalypb.RepositoryReplicasRequest{Repository: repo})
		require.NoError(t, err)
		require.NotNil(t, reps.GetPrimary())
		assert.Equal(t, "default", reps.GetPrimary().GetRepository().GetStorageName())
		require.Len(t, reps.GetReplicas(), 1)
		assert.NotEmpty(t, reps.GetPrimary().GetChecksum())
	})

	t.Run("authoritative storage", func(t *testing.T) {
		_, err := pi.SetAuthoritativeStorage(ctx, &gitalypb.SetAuthoritativeStorageRequest{
			VirtualStorage: "default", RelativePath: repoName, AuthoritativeStorage: "default"})
		require.NoError(t, err)
		_, err = pi.SetAuthoritativeStorage(ctx, &gitalypb.SetAuthoritativeStorageRequest{
			VirtualStorage: "default", RelativePath: repoName, AuthoritativeStorage: "elsewhere"})
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("mark unverified and dataloss", func(t *testing.T) {
		mu, err := pi.MarkUnverified(ctx, &gitalypb.MarkUnverifiedRequest{
			Selector: &gitalypb.MarkUnverifiedRequest_VirtualStorage{VirtualStorage: "default"}})
		require.NoError(t, err)
		assert.EqualValues(t, 0, mu.GetReplicasMarked())
		stream, err := pi.Dataloss(ctx, &gitalypb.DatalossRequest{VirtualStorage: "default"})
		require.NoError(t, err)
		count := 0
		for {
			if _, rerr := stream.Recv(); rerr != nil {
				break // EOF: no dataloss rows
			}
			count++
		}
		assert.Zero(t, count)
	})

	t.Run("replication factor", func(t *testing.T) {
		_, err := pi.SetReplicationFactor(ctx, &gitalypb.SetReplicationFactorRequest{
			VirtualStorage: "default", RelativePath: repoName, ReplicationFactor: 1})
		require.NoError(t, err)
		_, err = pi.SetReplicationFactor(ctx, &gitalypb.SetReplicationFactorRequest{
			VirtualStorage: "default", RelativePath: repoName, ReplicationFactor: 3})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		_, err = pi.SetReplicationFactor(ctx, &gitalypb.SetReplicationFactorRequest{
			VirtualStorage: "default", RelativePath: repoName, ReplicationFactor: 0})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("repository metadata", func(t *testing.T) {
		md, err := pi.GetRepositoryMetadata(ctx, &gitalypb.GetRepositoryMetadataRequest{
			Query: &gitalypb.GetRepositoryMetadataRequest_Path_{
				Path: &gitalypb.GetRepositoryMetadataRequest_Path{
					VirtualStorage: "default", RelativePath: repoName}}})
		require.NoError(t, err)
		assert.Equal(t, "default", md.GetPrimary())
		require.Len(t, md.GetReplicas(), 1)
		assert.True(t, md.GetReplicas()[0].GetHealthy())
		assert.True(t, md.GetReplicas()[0].GetAssigned())
		assert.Positive(t, md.GetGeneration(), "pack count is the generation counter")

		_, err = pi.GetRepositoryMetadata(ctx, &gitalypb.GetRepositoryMetadataRequest{
			Query: &gitalypb.GetRepositoryMetadataRequest_RepositoryId{RepositoryId: 42}})
		assert.Equal(t, codes.NotFound, status.Code(err), "no numeric-id plane on this node")
	})

	t.Run("partitions list and backup", func(t *testing.T) {
		ls, err := pt.ListPartitions(ctx, &gitalypb.ListPartitionsRequest{StorageName: "default"})
		require.NoError(t, err)
		require.NotEmpty(t, ls.GetPartitions())
		found := false
		for _, p := range ls.GetPartitions() {
			if p.GetId() == repoName {
				found = true
			}
		}
		assert.True(t, found, "each repo is its own partition")

		sink := t.TempDir()
		_ = sink
		_, err = pt.BackupPartition(ctx, &gitalypb.BackupPartitionRequest{
			StorageName: "default", PartitionId: repoName})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err),
			"sinkless node refuses; the sink flow itself is covered by T72/T73")
	})

	t.Run("ref transactions commit", func(t *testing.T) {
		vote, err := tx.VoteTransaction(ctx, &gitalypb.VoteTransactionRequest{
			Repository: repo, TransactionId: 7, Node: "default",
			ReferenceUpdatesHash: []byte("hash"), Phase: gitalypb.VoteTransactionRequest_PREPARED_PHASE})
		require.NoError(t, err)
		assert.Equal(t, gitalypb.VoteTransactionResponse_COMMIT, vote.GetState())
		_, err = tx.StopTransaction(ctx, &gitalypb.StopTransactionRequest{
			Repository: repo, TransactionId: 7})
		require.NoError(t, err)
	})

}
