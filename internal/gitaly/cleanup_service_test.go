package gitaly

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

func rewriteStream(t *testing.T, ctx context.Context, client gitalypb.CleanupServiceClient,
	repo *gitalypb.Repository, blobs []string, redactions [][]byte) error {
	t.Helper()
	stream, err := client.RewriteHistory(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.RewriteHistoryRequest{
		Repository: repo, Blobs: blobs, Redactions: redactions}))
	_, err = stream.CloseAndRecv()
	return err
}

// treeEntryOID fetches one tree entry's OID at a revision via GetTreeEntries.
func treeEntryOID(t *testing.T, ctx context.Context, cs gitalypb.CommitServiceClient,
	repo *gitalypb.Repository, rev, path string) (string, bool) {
	t.Helper()
	stream, err := cs.GetTreeEntries(ctx, &gitalypb.GetTreeEntriesRequest{
		Repository: repo, Revision: []byte(rev), Path: []byte("")})
	require.NoError(t, err)
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return "", false
		}
		require.NoError(t, err)
		for _, e := range resp.GetEntries() {
			if string(e.GetPath()) == path {
				return string(e.GetOid()), true
			}
		}
	}
}

func TestGitalyRewriteHistory(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	cl := gitalypb.NewCleanupServiceClient(conn)
	cs := gitalypb.NewCommitServiceClient(conn)
	rs := gitalypb.NewRepositoryServiceClient(conn)

	const repoName = "rewrite-me"
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}
	_, err = rs.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: repo})
	require.NoError(t, err)

	seed := []struct {
		msg   string
		files map[string]string
	}{
		{"one", map[string]string{"hello.txt": "greetings secret plan\n", "creds.txt": "password=hunter2\n"}},
		{"two", map[string]string{"hello.txt": "greetings secret plan\nmore\n", "keep.txt": "fine\n"}},
		{"three", map[string]string{"creds.txt": "password=hunter2\n"}},
	}
	var base model.OID
	for _, s := range seed {
		objs := []service.PutObject{}
		changes := []service.Change{}
		for path, content := range s.files {
			oid := model.ObjectOID(model.BlobType, []byte(content), model.SHA1)
			objs = append(objs, service.PutObject{OID: oid, Type: "blob", Data: []byte(content)})
			changes = append(changes, service.Change{Path: path, Blob: oid})
		}
		require.NoError(t, svc.PutObjects(ctx, repoName, objs))
		req := service.CommitRequest{Repo: repoName, Message: s.msg, Changes: changes}
		if !base.IsZero() {
			req.Base = base
		}
		r, err := svc.CreateCommit(ctx, req)
		require.NoError(t, err)
		base = r.Head
	}
	credsOID := model.ObjectOID(model.BlobType, []byte("password=hunter2\n"), model.SHA1)

	// A second branch and an aux ref both anchored at the middle commit —
	// the rewrite must move every ref that rides the rewritten history.
	mid, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{Repository: repo, Revision: []byte("HEAD~1")})
	require.NoError(t, err)
	midOID := mid.GetCommit().GetId()
	_, err = rs.WriteRef(ctx, &gitalypb.WriteRefRequest{
		Repository: repo, Ref: []byte("refs/heads/side"), Revision: []byte(midOID)})
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRef(ctx, service.RefUpdate{
		Repo: repoName, Name: "refs/merge-requests/1/head", New: model.OID(midOID)}))

	oldTip, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{Repository: repo, Revision: []byte("HEAD")})
	require.NoError(t, err)

	err = rewriteStream(t, ctx, cl, repo, []string{string(credsOID)}, [][]byte{[]byte("literal:secret")})
	require.NoError(t, err)

	newTip, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{Repository: repo, Revision: []byte("HEAD")})
	require.NoError(t, err)
	assert.NotEqual(t, oldTip.GetCommit().GetId(), newTip.GetCommit().GetId(), "tip must be rewritten")
	assert.Equal(t, []byte("three"), newTip.GetCommit().GetSubject())

	// The stripped blob is gone at every revision; redacted content landed.
	_, ok := treeEntryOID(t, ctx, cs, repo, "HEAD", "creds.txt")
	assert.False(t, ok, "stripped path must vanish at HEAD")
	_, ok = treeEntryOID(t, ctx, cs, repo, "HEAD~1", "creds.txt")
	assert.False(t, ok, "stripped path must vanish one back")
	helloOID, ok := treeEntryOID(t, ctx, cs, repo, "HEAD", "hello.txt")
	require.True(t, ok, "hello.txt survives the rewrite")
	blobStream, err := gitalypb.NewBlobServiceClient(conn).GetBlob(ctx, &gitalypb.GetBlobRequest{
		Repository: repo, Oid: helloOID, Limit: -1})
	require.NoError(t, err)
	var helloData []byte
	for {
		resp, rerr := blobStream.Recv()
		if rerr == io.EOF {
			break
		}
		require.NoError(t, rerr)
		helloData = append(helloData, resp.GetData()...)
	}
	require.NotEmpty(t, helloData)
	assert.Contains(t, string(helloData), "***REMOVED***")
	assert.NotContains(t, string(helloData), "secret")

	// History is walkable and every ref moved onto rewritten commits.
	count, err := gitalypb.NewCommitServiceClient(conn).CountCommits(ctx, &gitalypb.CountCommitsRequest{
		Repository: repo, Revision: []byte("HEAD")})
	require.NoError(t, err)
	assert.EqualValues(t, 3, count.GetCount())
	side, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{Repository: repo, Revision: []byte("refs/heads/side")})
	require.NoError(t, err)
	assert.Equal(t, []byte("two"), side.GetCommit().GetSubject())
	mrTip, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{Repository: repo, Revision: []byte("refs/merge-requests/1/head")})
	require.NoError(t, err)
	assert.Equal(t, []byte("two"), mrTip.GetCommit().GetSubject())
}

func TestGitalyRewriteHistoryValidation(t *testing.T) {
	_, _, _, _, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	cl := gitalypb.NewCleanupServiceClient(conn)
	rs := gitalypb.NewRepositoryServiceClient(conn)

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "rw-valid"}
	_, err = rs.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: repo})
	require.NoError(t, err)

	err = rewriteStream(t, ctx, cl, repo, nil, nil)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "empty rules must be rejected")

	err = rewriteStream(t, ctx, cl, repo, []string{"not-an-oid"}, nil)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	err = rewriteStream(t, ctx, cl, repo, nil, [][]byte{[]byte("has\nnewline")})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// Repository only on the first message.
	stream, err := cl.RewriteHistory(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.RewriteHistoryRequest{
		Repository: repo, Redactions: [][]byte{[]byte("x")}}))
	require.NoError(t, stream.Send(&gitalypb.RewriteHistoryRequest{
		Repository: repo, Redactions: [][]byte{[]byte("y")}}))
	_, err = stream.CloseAndRecv()
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGitalyApplyBfgObjectMapStream(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	cl := gitalypb.NewCleanupServiceClient(conn)
	fs2 := gitalypb.NewRefServiceClient(conn)

	repoName := "bfg-map"
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}
	head, _ := seedHistory(t, svc, repoName,
		map[string]string{"hello.txt": "one"}, map[string]string{"two.txt": "two"})

	// Internal refs anchored at the head commit + a normal branch that must
	// survive the map application untouched.
	require.NoError(t, svc.UpdateRef(ctx, service.RefUpdate{
		Repo: repoName, Name: "refs/keep-around/1", New: head}))
	require.NoError(t, svc.UpdateRef(ctx, service.RefUpdate{
		Repo: repoName, Name: "refs/heads/main", New: head}))
	mapped := head
	if mapped[0] == 'a' {
		mapped = "b" + mapped[1:]
	} else {
		mapped = "a" + mapped[1:]
	}

	stream, err := cl.ApplyBfgObjectMapStream(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.ApplyBfgObjectMapStreamRequest{
		Repository: repo, ObjectMap: []byte("old                                      new\n")}))
	require.NoError(t, stream.Send(&gitalypb.ApplyBfgObjectMapStreamRequest{
		ObjectMap: []byte(string(head) + " " + string(mapped) + "\n")}))
	require.NoError(t, stream.CloseSend())
	entries := []*gitalypb.ApplyBfgObjectMapStreamResponse_Entry{}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		entries = append(entries, resp.GetEntries()...)
	}
	require.Len(t, entries, 1)
	assert.Equal(t, string(head), entries[0].GetOldOid())
	assert.Equal(t, string(mapped), entries[0].GetNewOid())
	assert.Equal(t, gitalypb.ObjectType_UNKNOWN, entries[0].GetType(), "unmapped new OID answers UNKNOWN")

	refs, err := fs2.FindRefsByOID(ctx, &gitalypb.FindRefsByOIDRequest{Repository: repo, Oid: string(head)})
	require.NoError(t, err)
	assert.NotContains(t, refs.GetRefs(), "refs/keep-around/1", "internal ref must be removed")
	found, err := fs2.RefExists(ctx, &gitalypb.RefExistsRequest{
		Repository: repo, Ref: []byte("refs/heads/main")})
	require.NoError(t, err)
	assert.True(t, found.GetValue(), "branch ref must survive the map")
}
