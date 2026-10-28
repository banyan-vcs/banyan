package gitaly

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

const testToken = "s3cr3t"

// startServer boots a gitaly compat server over an in-memory banyan stack
// and returns it with its dial address.
func startServer(t *testing.T) (*Server, storage.MetaStore, *storage.ObjectService, *service.Service, string) {
	t.Helper()
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	objects := storage.NewObjectService(meta, local, []storage.Node{{ID: "local-1", Local: true}}, model.SHA1)
	svc := service.New(meta, objects, model.SHA1)
	srv := NewServer(meta, objects, model.SHA1, Options{Token: testToken})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.grpcSrv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return srv, meta, objects, svc, lis.Addr().String()
}

// seedHistory commits three files across two commits and returns the branch
// head plus the root-file blob OID.
func seedHistory(t *testing.T, svc *service.Service, repo string, files1, files2 map[string]string) (model.OID, model.OID) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, svc.Meta.CreateRepo(ctx, repo, model.SHA1))
	put := func(files map[string]string) {
		objs := make([]service.PutObject, 0, len(files))
		for _, content := range files {
			objs = append(objs, service.PutObject{
				OID: model.ObjectOID(model.BlobType, []byte(content), model.SHA1), Type: "blob", Data: []byte(content)})
		}
		require.NoError(t, svc.PutObjects(ctx, repo, objs))
	}
	changes := func(files map[string]string) []service.Change {
		var cs []service.Change
		for path, content := range files {
			cs = append(cs, service.Change{Path: path, Blob: model.ObjectOID(model.BlobType, []byte(content), model.SHA1)})
		}
		return cs
	}
	put(files1)
	r1, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repo, Message: "first", Changes: changes(files1)})
	require.NoError(t, err)
	put(files2)
	r2, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repo, Base: r1.Head, Message: "second", Changes: changes(files2)})
	require.NoError(t, err)
	var rootBlob model.OID
	for path, content := range files1 {
		if path == "hello.txt" {
			rootBlob = model.ObjectOID(model.BlobType, []byte(content), model.SHA1)
		}
	}
	return r2.Head, rootBlob
}

func TestGitalyAuthGate(t *testing.T) {
	_, _, _, _, addr := startServer(t)
	ctx := context.Background()

	conn, err := Dial(addr, "wrong-token")
	require.NoError(t, err)
	defer conn.Close()
	ss := gitalypb.NewServerServiceClient(conn)
	_, err = ss.ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))

	conn2, err := Dial(addr, "")
	require.NoError(t, err)
	defer conn2.Close()
	_, err = gitalypb.NewServerServiceClient(conn2).ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestGitalyServerService(t *testing.T) {
	_, _, _, _, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	ss := gitalypb.NewServerServiceClient(conn)

	info, err := ss.ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, ProtoVersion, info.GetServerVersion())
	require.Len(t, info.GetStorageStatuses(), 1)
	assert.Equal(t, "default", info.GetStorageStatuses()[0].GetStorageName())
	assert.True(t, info.GetStorageStatuses()[0].GetReadable())

	rdy, err := ss.ReadinessCheck(ctx, &gitalypb.ReadinessCheckRequest{})
	require.NoError(t, err)
	assert.NotNil(t, rdy.GetOkResponse())

	sig, err := ss.ServerSignature(ctx, &gitalypb.ServerSignatureRequest{})
	require.NoError(t, err)
	assert.Contains(t, string(sig.GetPublicKey()), "PUBLIC KEY")
}

func TestGitalyRepositoryLifecycle(t *testing.T) {
	_, _, _, _, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	rs := gitalypb.NewRepositoryServiceClient(conn)

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "@hashed/6b/86/6b86b273.git"}

	ex, err := rs.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
	require.NoError(t, err)
	assert.False(t, ex.GetExists())

	_, err = rs.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: repo, DefaultBranch: []byte("master")})
	require.NoError(t, err)

	ex, err = rs.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
	require.NoError(t, err)
	assert.True(t, ex.GetExists())

	_, err = rs.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: repo})
	assert.Equal(t, codes.AlreadyExists, status.Code(err))

	// addressing: unknown storage and missing fields are protocol errors
	_, err = rs.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{
		Repository: &gitalypb.Repository{StorageName: "other", RelativePath: "x.git"}})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = rs.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGitalyRefAndCommitSurface(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()

	repoName := "proj-1"
	head, rootBlob := seedHistory(t, svc, repoName,
		map[string]string{"hello.txt": "hello gitaly\n", "docs/guide.md": "guide"},
		map[string]string{"docs/guide.md": "guide v2", "src/main.go": "package main"})
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}

	rs := gitalypb.NewRefServiceClient(conn)
	cs := gitalypb.NewCommitServiceClient(conn)
	bs := gitalypb.NewBlobServiceClient(conn)

	t.Run("default branch", func(t *testing.T) {
		def, err := rs.FindDefaultBranchName(ctx, &gitalypb.FindDefaultBranchNameRequest{Repository: repo})
		require.NoError(t, err)
		assert.Equal(t, "main", string(def.GetName()))
		h, err := rs.FindDefaultBranchName(ctx, &gitalypb.FindDefaultBranchNameRequest{Repository: repo, HeadOnly: true})
		require.NoError(t, err)
		assert.Equal(t, "HEAD", string(h.GetName()))
	})

	t.Run("find branch", func(t *testing.T) {
		for _, name := range []string{"main", "refs/heads/main"} {
			b, err := rs.FindBranch(ctx, &gitalypb.FindBranchRequest{Repository: repo, Name: []byte(name)})
			require.NoError(t, err, name)
			assert.Equal(t, head.String(), b.GetBranch().GetTargetCommit().GetId())
			assert.Len(t, b.GetBranch().GetTargetCommit().GetParentIds(), 1)
		}
		_, err := rs.FindBranch(ctx, &gitalypb.FindBranchRequest{Repository: repo, Name: []byte("nope")})
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("ref exists", func(t *testing.T) {
		v, err := rs.RefExists(ctx, &gitalypb.RefExistsRequest{Repository: repo, Ref: []byte("refs/heads/main")})
		require.NoError(t, err)
		assert.True(t, v.GetValue())
		v, err = rs.RefExists(ctx, &gitalypb.RefExistsRequest{Repository: repo, Ref: []byte("refs/heads/ghost")})
		require.NoError(t, err)
		assert.False(t, v.GetValue())
		_, err = rs.RefExists(ctx, &gitalypb.RefExistsRequest{Repository: repo, Ref: []byte("main")})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("list refs", func(t *testing.T) {
		var refs []*gitalypb.ListRefsResponse_Reference
		stream, err := rs.ListRefs(ctx, &gitalypb.ListRefsRequest{
			Repository: repo, Patterns: [][]byte{[]byte("refs/heads/*")}, Head: true})
		require.NoError(t, err)
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			refs = append(refs, resp.GetReferences()...)
		}
		names := map[string]string{}
		for _, r := range refs {
			names[string(r.GetName())] = r.GetTarget()
		}
		assert.Equal(t, head.String(), names["HEAD"])
		assert.Equal(t, head.String(), names["refs/heads/main"])
		_, ok := names["refs/heads/nonexistent"]
		assert.False(t, ok)
	})

	t.Run("find refs by oid", func(t *testing.T) {
		refs, err := rs.FindRefsByOID(ctx, &gitalypb.FindRefsByOIDRequest{Repository: repo, Oid: head.String()})
		require.NoError(t, err)
		assert.Contains(t, refs.GetRefs(), "refs/heads/main")
	})

	t.Run("find commit", func(t *testing.T) {
		for _, rev := range []string{"main", "HEAD", head.String()} {
			c, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{Repository: repo, Revision: []byte(rev)})
			require.NoError(t, err, rev)
			require.NotNil(t, c.GetCommit(), rev)
			assert.Equal(t, head.String(), c.GetCommit().GetId())
			assert.NotEmpty(t, c.GetCommit().GetSubject())
			assert.Equal(t, "second", string(c.GetCommit().GetSubject()))
		}
		c, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{Repository: repo, Revision: []byte("refs/heads/ghost")})
		require.NoError(t, err)
		assert.Nil(t, c.GetCommit())
	})

	t.Run("commit is ancestor", func(t *testing.T) {
		v, err := cs.CommitIsAncestor(ctx, &gitalypb.CommitIsAncestorRequest{
			Repository: repo, AncestorId: c1Of(t, svc, repoName, head), ChildId: head.String()})
		require.NoError(t, err)
		assert.True(t, v.GetValue())
		v, err = cs.CommitIsAncestor(ctx, &gitalypb.CommitIsAncestorRequest{
			Repository: repo, AncestorId: head.String(), ChildId: c1Of(t, svc, repoName, head)})
		require.NoError(t, err)
		assert.False(t, v.GetValue())
	})

	t.Run("list commits", func(t *testing.T) {
		stream, err := cs.ListCommits(ctx, &gitalypb.ListCommitsRequest{Repository: repo, Revisions: []string{"main"}})
		require.NoError(t, err)
		var ids []string
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, c := range resp.GetCommits() {
				ids = append(ids, c.GetId())
			}
		}
		assert.Len(t, ids, 2)
		assert.Equal(t, head.String(), ids[0])
	})

	t.Run("count commits", func(t *testing.T) {
		c, err := cs.CountCommits(ctx, &gitalypb.CountCommitsRequest{Repository: repo, Revision: []byte("main")})
		require.NoError(t, err)
		assert.Equal(t, int32(2), c.GetCount())
	})

	t.Run("tree entries", func(t *testing.T) {
		stream, err := cs.GetTreeEntries(ctx, &gitalypb.GetTreeEntriesRequest{Repository: repo, Revision: []byte("main")})
		require.NoError(t, err)
		var entries []*gitalypb.TreeEntry
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			entries = append(entries, resp.GetEntries()...)
		}
		kinds := map[string]gitalypb.TreeEntry_EntryType{}
		for _, e := range entries {
			kinds[string(e.GetPath())] = e.GetType()
		}
		assert.Equal(t, gitalypb.TreeEntry_BLOB, kinds["hello.txt"])
		assert.Equal(t, gitalypb.TreeEntry_TREE, kinds["docs"])
		assert.Equal(t, gitalypb.TreeEntry_TREE, kinds["src"])

		stream, err = cs.GetTreeEntries(ctx, &gitalypb.GetTreeEntriesRequest{
			Repository: repo, Revision: []byte("main"), Path: []byte("docs"), Recursive: true})
		require.NoError(t, err)
		entries = nil
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			entries = append(entries, resp.GetEntries()...)
		}
		require.Len(t, entries, 1)
		assert.Equal(t, "docs/guide.md", string(entries[0].GetPath()))
	})

	t.Run("get blob", func(t *testing.T) {
		stream, err := bs.GetBlob(ctx, &gitalypb.GetBlobRequest{Repository: repo, Oid: rootBlob.String()})
		require.NoError(t, err)
		var data []byte
		var size int64
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			if resp.GetSize() > 0 {
				size = resp.GetSize()
			}
			data = append(data, resp.GetData()...)
		}
		assert.Equal(t, "hello gitaly\n", string(data))
		assert.Equal(t, int64(len("hello gitaly\n")), size)
	})

	t.Run("update and delete refs", func(t *testing.T) {
		updStream, err := rs.UpdateReferences(ctx)
		require.NoError(t, err)
		require.NoError(t, updStream.Send(&gitalypb.UpdateReferencesRequest{
			Repository: repo,
			Updates: []*gitalypb.UpdateReferencesRequest_Update{{
				Reference: []byte("refs/tags/v1"), NewObjectId: []byte(head.String())}}}))
		_, err = updStream.CloseAndRecv()
		require.NoError(t, err)
		v, err := rs.RefExists(ctx, &gitalypb.RefExistsRequest{Repository: repo, Ref: []byte("refs/tags/v1")})
		require.NoError(t, err)
		assert.True(t, v.GetValue())

		_, err = rs.DeleteRefs(ctx, &gitalypb.DeleteRefsRequest{Repository: repo, Refs: [][]byte{[]byte("refs/tags/v1")}})
		require.NoError(t, err)
		v, err = rs.RefExists(ctx, &gitalypb.RefExistsRequest{Repository: repo, Ref: []byte("refs/tags/v1")})
		require.NoError(t, err)
		assert.False(t, v.GetValue())
	})

	t.Run("unimplemented rpc answers unimplemented", func(t *testing.T) {
		stream, err := gitalypb.NewDiffServiceClient(conn).DiffStats(ctx, &gitalypb.DiffStatsRequest{})
		require.NoError(t, err) // dial succeeds; the failure surfaces on Recv
		_, err = stream.Recv()
		assert.Equal(t, codes.Unimplemented, status.Code(err))
	})
}

// c1Of walks to the first parent of head.
func c1Of(t *testing.T, svc *service.Service, repo string, head model.OID) string {
	t.Helper()
	ci, err := svc.Objects.CommitInfo(repo, head)
	require.NoError(t, err)
	require.NotEmpty(t, ci.Parents)
	return ci.Parents[0].String()
}
