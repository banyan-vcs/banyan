package gitaly

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// repoRef is the wire shape for a banyan repo name.
func repoRef(name string) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: "default", RelativePath: name}
}

// TestGitalyOperationsAndLFS covers the OperationService ref slice, the
// web-edit UserCommitFiles stream and the LFS-pointer trio (docs/19).
func TestGitalyOperationsAndLFS(t *testing.T) {
	srv, _, objects, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	op := gitalypb.NewOperationServiceClient(conn)
	bs := gitalypb.NewBlobServiceClient(conn)

	repoName := "ops-lab"
	repo := repoRef(repoName)
	head, _ := seedHistory(t, svc, repoName,
		map[string]string{"hello.txt": "hello gitaly\n", "docs/guide.md": "guide"},
		map[string]string{"docs/guide.md": "guide v2", "main.go": "package main"})
	// mainTip tracks main's advancing head across subtests.
	mainTip := head

	lfsHex := strings.Repeat("6a8fba1e", 8) // 64 hex chars, the LFS digest width
	pointerBlob := "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:" + lfsHex + "\n" +
		"size 123456\n"
	pointerOID := model.ObjectOID(model.BlobType, []byte(pointerBlob), model.SHA1)

	t.Run("lfs pointers", func(t *testing.T) {
		// Commit the pointer, then delete it: full-history semantics still
		// list it (rev-list --objects, not the tip snapshot).
		require.NoError(t, svc.PutObjects(ctx, repoName, []service.PutObject{
			{OID: pointerOID, Type: "blob", Data: []byte(pointerBlob)}}))
		c1, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repoName, Base: mainTip,
			Message: "add lfs file", Changes: []service.Change{{Path: "big.bin", Blob: pointerOID}}})
		require.NoError(t, err)
		c2, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repoName, Base: c1.Head,
			Message: "drop lfs file", Changes: []service.Change{{Path: "big.bin", Delete: true}}})
		require.NoError(t, err)
		mainTip = c2.Head

		stream, err := bs.ListLFSPointers(ctx, &gitalypb.ListLFSPointersRequest{
			Repository: repo, Revisions: []string{"main"}})
		require.NoError(t, err)
		var got []*gitalypb.LFSPointer
		var recvErr error
		for {
			resp, err := stream.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					recvErr = err
				}
				break
			}
			got = append(got, resp.GetLfsPointers()...)
		}
		require.NoError(t, recvErr, "ListLFSPointers stream failed")
		require.Len(t, got, 1)
		assert.Equal(t, pointerOID.String(), got[0].GetOid())
		assert.Equal(t, pointerBlob, string(got[0].GetData()))
		assert.Equal(t, int64(len(pointerBlob)), got[0].GetSize())
		assert.Equal(t, lfsHex, string(got[0].GetFileOid()))
		assert.Equal(t, int64(123456), got[0].GetFileSize())

		// Get: pointer + non-pointer objects answer silence; a miss errors.
		normalOID := model.ObjectOID(model.BlobType, []byte("hello gitaly\n"), model.SHA1)
		getStream, err := bs.GetLFSPointers(ctx, &gitalypb.GetLFSPointersRequest{
			Repository: repo,
			BlobIds:    []string{pointerOID.String(), normalOID.String(), head.String()}})
		require.NoError(t, err)
		n := 0
		for {
			resp, err := getStream.Recv()
			if err != nil {
				break
			}
			n += len(resp.GetLfsPointers())
		}
		assert.Equal(t, 1, n)

		// Server-stream errors surface on Recv, not on the open call.
		drainCode := func(s interface {
			Recv() (*gitalypb.GetLFSPointersResponse, error)
		}) codes.Code {
			for {
				if _, err := s.Recv(); err != nil {
					return status.Code(err)
				}
			}
		}
		es, err := bs.GetLFSPointers(ctx, &gitalypb.GetLFSPointersRequest{
			Repository: repo, BlobIds: nil})
		require.NoError(t, err)
		assert.Equal(t, codes.InvalidArgument, drainCode(es))

		ls, err := bs.ListLFSPointers(ctx, &gitalypb.ListLFSPointersRequest{Repository: repo})
		require.NoError(t, err)
		_, lerr := ls.Recv()
		assert.Equal(t, codes.InvalidArgument, status.Code(lerr))

		// ListAll: over every object in the database.
		allStream, err := bs.ListAllLFSPointers(ctx, &gitalypb.ListAllLFSPointersRequest{Repository: repo})
		require.NoError(t, err)
		n = 0
		for {
			resp, err := allStream.Recv()
			if err != nil {
				break
			}
			n += len(resp.GetLfsPointers())
		}
		assert.Equal(t, 1, n)
	})

	t.Run("create and delete branch", func(t *testing.T) {
		out, err := op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo, BranchName: []byte("feature"), StartPoint: []byte("main")})
		require.NoError(t, err)
		assert.Equal(t, "feature", string(out.GetBranch().GetName()))
		assert.Equal(t, mainTip.String(), out.GetBranch().GetTargetCommit().GetId())

		_, err = op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo, BranchName: []byte("feature"), StartPoint: []byte("main")})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))

		_, err = op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo, BranchName: []byte("ghost"), StartPoint: []byte("nope")})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))

		// An abbreviated OID is InvalidArgument (upstream objectHash.FromHex
		// fails identically); a full-length wrong OID is the CAS mismatch.
		_, err = op.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
			Repository: repo, BranchName: []byte("feature"), ExpectedOldOid: "deadbeef"})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		_, err = op.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
			Repository: repo, BranchName: []byte("feature"), ExpectedOldOid: strings.Repeat("0", 40)})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err), "err=%v", err)

		_, err = op.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
			Repository: repo, BranchName: []byte("feature"), ExpectedOldOid: mainTip.String()})
		require.NoError(t, err)

		_, err = op.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
			Repository: repo, BranchName: []byte("feature")})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("tags", func(t *testing.T) {
		out, err := op.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
			Repository: repo, TagName: []byte("v1.0"), TargetRevision: []byte("main"),
			Message: []byte("release v1.0\n"), User: &gitalypb.User{
				GlId: "user-1", Name: []byte("Dev Eloper"), Email: []byte("dev@example.com")},
			Timestamp: timestamppb.New(time.Unix(1700000000, 0))})
		require.NoError(t, err)
		assert.Equal(t, "v1.0", string(out.GetTag().GetName()))
		assert.Equal(t, "release v1.0\n", string(out.GetTag().GetMessage()))
		assert.Equal(t, "Dev Eloper", string(out.GetTag().GetTagger().GetName()))
		assert.NotEqual(t, head.String(), out.GetTag().GetId(), "annotated tag has its own object")

		// The tag object itself must parse and peel to the commit.
		_, raw, err := objects.Raw(ctx, repoName, model.OID(out.GetTag().GetId()))
		require.NoError(t, err)
		tag, err := model.DecodeTag(raw)
		require.NoError(t, err)
		assert.Equal(t, mainTip, tag.Object)

		_, err = op.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
			Repository: repo, TagName: []byte("v1.0"), TargetRevision: []byte("main")})
		assert.Equal(t, codes.AlreadyExists, status.Code(err))

		lw, err := op.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
			Repository: repo, TagName: []byte("light"), TargetRevision: []byte("main")})
		require.NoError(t, err)
		assert.Equal(t, mainTip.String(), lw.GetTag().GetId(), "lightweight tag points at the commit")

		_, err = op.UserDeleteTag(ctx, &gitalypb.UserDeleteTagRequest{
			Repository: repo, TagName: []byte("light")})
		require.NoError(t, err)
		_, err = op.UserDeleteTag(ctx, &gitalypb.UserDeleteTagRequest{
			Repository: repo, TagName: []byte("light")})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("update and fast-forward branch", func(t *testing.T) {
		_, err := op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo, BranchName: []byte("ff"), StartPoint: []byte("main")})
		require.NoError(t, err)

		preAdvance := mainTip
		nextBlob := model.ObjectOID(model.BlobType, []byte("next\n"), model.SHA1)
		require.NoError(t, svc.PutObjects(ctx, repoName, []service.PutObject{
			{OID: nextBlob, Type: "blob", Data: []byte("next\n")}}))
		next, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repoName, Base: mainTip,
			Message: "advance", Changes: []service.Change{{Path: "next.txt", Blob: nextBlob}}})
		require.NoError(t, err)

		_, err = op.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
			Repository: repo, Branch: []byte("ff"), CommitId: next.Head.String()})
		require.NoError(t, err)

		// An ancestor target is not a fast-forward.
		_, err = op.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
			Repository: repo, Branch: []byte("ff"), CommitId: head.String()})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Equal(t, "not fast forward", status.Convert(err).Message())

		_, err = op.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
			Repository: repo, Branch: []byte("ghost"), CommitId: next.Head.String()})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))

		_, err = op.UserUpdateBranch(ctx, &gitalypb.UserUpdateBranchRequest{
			Repository: repo, BranchName: []byte("ff"), Newrev: []byte(head.String())})
		require.NoError(t, err)

		// ff now sits at the seed tip; an oldrev matching some other commit
		// (the pre-advance main tip) must refuse.
		_, err = op.UserUpdateBranch(ctx, &gitalypb.UserUpdateBranchRequest{
			Repository: repo, BranchName: []byte("ff"), Newrev: []byte(next.Head.String()),
			Oldrev: []byte(preAdvance.String())})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		mainTip = next.Head
	})

	t.Run("user commit files", func(t *testing.T) {
		stream, err := op.UserCommitFiles(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
				Header: &gitalypb.UserCommitFilesRequestHeader{
					Repository:        repo,
					BranchName:        []byte("feature"),
					StartBranchName:   []byte("main"),
					CommitMessage:     []byte("web edit\n"),
					CommitAuthorName:  []byte("Web Editor"),
					CommitAuthorEmail: []byte("editor@example.com"),
					User: &gitalypb.User{
						GlId: "user-2", Name: []byte("Committer Hat"), Email: []byte("committer@example.com")},
					Timestamp: timestamppb.New(time.Unix(1700000100, 0)),
				}}}))
		action := func(h *gitalypb.UserCommitFilesActionHeader, content []byte) {
			require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
				UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
					Action: &gitalypb.UserCommitFilesAction{
						UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Header{Header: h}}}}))
			if content != nil {
				require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
					UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
						Action: &gitalypb.UserCommitFilesAction{
							UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Content{Content: content}}}}))
			}
		}
		action(&gitalypb.UserCommitFilesActionHeader{
			Action: gitalypb.UserCommitFilesActionHeader_CREATE, FilePath: []byte("web/new.txt"),
			ExecuteFilemode: true}, []byte("from the web\n"))
		action(&gitalypb.UserCommitFilesActionHeader{
			Action:       gitalypb.UserCommitFilesActionHeader_MOVE,
			FilePath:     []byte("docs/manual.md"),
			PreviousPath: []byte("docs/guide.md"), InferContent: true}, nil)
		action(&gitalypb.UserCommitFilesActionHeader{
			Action: gitalypb.UserCommitFilesActionHeader_DELETE, FilePath: []byte("hello.txt")}, nil)
		action(&gitalypb.UserCommitFilesActionHeader{
			Action: gitalypb.UserCommitFilesActionHeader_CREATE_DIR, FilePath: []byte("somedir")}, nil)
		res, err := stream.CloseAndRecv()
		require.NoError(t, err)
		require.NotEmpty(t, res.GetBranchUpdate().GetCommitId())
		assert.True(t, res.GetBranchUpdate().GetBranchCreated(), "branch born from start branch")

		commitOID := model.OID(res.GetBranchUpdate().GetCommitId())
		typ, raw, err := objects.Raw(ctx, repoName, commitOID)
		require.NoError(t, err)
		require.Equal(t, model.CommitType, typ)
		c, err := model.DecodeCommit(raw)
		require.NoError(t, err)
		assert.Equal(t, "Web Editor", c.Author.Name)
		assert.Equal(t, "Committer Hat", c.Committer.Name)
		assert.Equal(t, int64(1700000100), c.Committer.When.Unix())
		assert.Equal(t, "web edit\n", c.Message)
		assert.Equal(t, []model.OID{mainTip}, c.Parents, "parent is the start-branch tip")

		for path, want := range map[string]string{
			"web/new.txt":      "from the web\n",
			"docs/manual.md":   "guide v2",
			"somedir/.gitkeep": "",
		} {
			entry, err := srv.entryAtPath(ctx, repoName, c.TreeOID, path)
			require.NoError(t, err, path)
			require.NotNil(t, entry, path)
			_, data, err := objects.Raw(ctx, repoName, entry.Target)
			require.NoError(t, err)
			assert.Equal(t, want, string(data), path)
		}
		entry, err := srv.entryAtPath(ctx, repoName, c.TreeOID, "hello.txt")
		require.NoError(t, err)
		assert.Nil(t, entry, "deleted")
		exec, err := srv.entryAtPath(ctx, repoName, c.TreeOID, "web/new.txt")
		require.NoError(t, err)
		assert.Equal(t, "100755", exec.Mode, "CREATE with ExecuteFilemode")
	})
}
