// T74 — object pools and internal metadata (docs/integration/gitaly-compat.md). The pool is created
// from the origin's closure, linked members materialize the shared objects
// (banyan's alternates equivalent), GetObjectPool answers the registry, and
// the InternalGitaly pool-metadata RPCs walk the same records. The oracle
// is behavioral: a repo that was empty before linking serves the origin's
// objects afterwards.
package gitalyrpc

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT74GitalyObjectPool(t *testing.T) {
	support.NeedGit(t)
	s, meta, objects := support.NewServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t74"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t74")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	rs := gitalypb.NewRepositoryServiceClient(conn)
	ps := gitalypb.NewObjectPoolServiceClient(conn)
	ig := gitalypb.NewInternalGitalyClient(conn)
	cs := gitalypb.NewCommitServiceClient(conn)

	origin := &gitalypb.Repository{StorageName: "default", RelativePath: "t74-origin"}
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, src+"/base.txt", "shared base\n")
	support.Git(t, src, "add", "-A")
	support.GitDated(t, src, 1700000000, "commit", "-qm", "base commit")
	support.Git(t, src, "push", "-q", s.RepoURL("t74-origin"), "main")
	baseOID := strings.Fields(support.Git(t, src, "rev-parse", "main"))[0]

	poolHex := strings.Repeat("ab", 32)
	poolRel := "@pools/ab/cd/" + poolHex + ".git"
	poolKey := "pools-" + poolHex
	pool := &gitalypb.ObjectPool{Repository: &gitalypb.Repository{
		StorageName: "default", RelativePath: poolRel}}

	_, err = ps.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: pool, Origin: origin})
	require.NoError(t, err)

	// A second create refuses — the target exists (upstream's shape).
	_, err = ps.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: pool, Origin: origin})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	// Pools live under @pools/.
	_, err = ps.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: &gitalypb.ObjectPool{Repository: &gitalypb.Repository{
			StorageName: "default", RelativePath: "not-a-pool"}},
		Origin: origin})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// The origin is linked as the pool's upstream by creation.
	got, err := ps.GetObjectPool(ctx, &gitalypb.GetObjectPoolRequest{Repository: origin})
	require.NoError(t, err)
	require.NotNil(t, got.GetObjectPool())
	assert.Equal(t, poolRel, got.GetObjectPool().GetRepository().GetRelativePath())

	// Link an empty fork: materialization makes the pool's objects serve.
	fork := &gitalypb.Repository{StorageName: "default", RelativePath: "t74-fork"}
	_, err = rs.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: fork})
	require.NoError(t, err)
	_, err = ps.LinkRepositoryToObjectPool(ctx, &gitalypb.LinkRepositoryToObjectPoolRequest{
		ObjectPool: pool, Repository: fork})
	require.NoError(t, err)

	fc, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{
		Repository: fork, Revision: []byte(baseOID)})
	require.NoError(t, err)
	require.NotNil(t, fc.GetCommit(), "linked member resolves pool objects")
	assert.Equal(t, []byte("base commit"), fc.GetCommit().GetSubject())

	got, err = ps.GetObjectPool(ctx, &gitalypb.GetObjectPoolRequest{Repository: fork})
	require.NoError(t, err)
	assert.Equal(t, poolRel, got.GetObjectPool().GetRepository().GetRelativePath())

	// InternalGitaly walks repos and the pool registry.
	walk, err := ig.WalkRepos(ctx, &gitalypb.WalkReposRequest{StorageName: "default"})
	require.NoError(t, err)
	var walked []string
	for {
		resp, rerr := walk.Recv()
		if rerr == io.EOF {
			break
		}
		require.NoError(t, rerr)
		walked = append(walked, resp.GetRelativePath())
	}
	assert.Contains(t, walked, "t74-origin")
	assert.Contains(t, walked, poolKey)

	scan, err := ig.ScanPoolMetadata(ctx, &gitalypb.ScanPoolMetadataRequest{StorageName: "default"})
	require.NoError(t, err)
	type row struct {
		member, pool string
		upstream     bool
	}
	var rowsSeen []row
	for {
		resp, rerr := scan.Recv()
		if rerr == io.EOF {
			break
		}
		require.NoError(t, rerr)
		rowsSeen = append(rowsSeen, row{resp.GetRelativePath(), resp.GetPoolDiskPath(), resp.GetIsUpstream()})
	}
	assert.Contains(t, rowsSeen, row{"t74-fork", poolRel, false})
	assert.Contains(t, rowsSeen, row{"t74-origin", poolRel, true})

	listPools, err := ig.ListPoolMetadata(ctx, &gitalypb.ListPoolMetadataRequest{StorageName: "default"})
	require.NoError(t, err)
	var poolsListed []string
	for {
		resp, rerr := listPools.Recv()
		if rerr == io.EOF {
			break
		}
		require.NoError(t, rerr)
		poolsListed = append(poolsListed, resp.GetPoolDiskPath())
	}
	assert.Equal(t, []string{poolRel}, poolsListed)

	upstreamStream, err := ig.ListPoolUpstreams(ctx)
	require.NoError(t, err)
	require.NoError(t, upstreamStream.Send(&gitalypb.ListPoolUpstreamsRequest{
		StorageName: "default", PoolDiskPaths: []string{poolRel}}))
	require.NoError(t, upstreamStream.CloseSend())
	upResp, err := upstreamStream.Recv()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{poolRel: "t74-origin"}, upResp.GetUpstreams())

	// FetchIntoObjectPool extends the pool with the origin's new commit.
	support.Write(t, src+"/extra.txt", "more\n")
	support.Git(t, src, "add", "-A")
	support.GitDated(t, src, 1700000100, "commit", "-qm", "second commit")
	support.Git(t, src, "push", "-q", s.RepoURL("t74-origin"), "main")
	_, err = ps.FetchIntoObjectPool(ctx, &gitalypb.FetchIntoObjectPoolRequest{
		Origin: origin, ObjectPool: pool})
	require.NoError(t, err)

	// Disconnect unlinks; the member keeps serving (its objects are local).
	_, err = ps.DisconnectGitAlternates(ctx, &gitalypb.DisconnectGitAlternatesRequest{Repository: fork})
	require.NoError(t, err)
	got, err = ps.GetObjectPool(ctx, &gitalypb.GetObjectPoolRequest{Repository: fork})
	require.NoError(t, err)
	assert.Nil(t, got.GetObjectPool(), "disconnected member has no pool")
	fc, err = cs.FindCommit(ctx, &gitalypb.FindCommitRequest{
		Repository: fork, Revision: []byte(baseOID)})
	require.NoError(t, err)
	assert.NotNil(t, fc.GetCommit(), "materialized objects stay after disconnect")

	// DeleteObjectPool removes the pool repo and every membership record.
	_, err = ps.DeleteObjectPool(ctx, &gitalypb.DeleteObjectPoolRequest{ObjectPool: pool})
	require.NoError(t, err)
	got, err = ps.GetObjectPool(ctx, &gitalypb.GetObjectPoolRequest{Repository: origin})
	require.NoError(t, err)
	assert.Nil(t, got.GetObjectPool(), "members unlinked with the pool")
	_, err = rs.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: poolRel}})
	require.NoError(t, err)
}
