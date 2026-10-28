// T64 — OperationService ref slice, UserCommitFiles and the LFS-pointer
// trio (docs/19 slice 5). Git is the oracle: branch/tag moves verify via
// ls-remote and cat-file on fetched objects, the web-edit commit verifies
// as an ordinary commit git can fetch and read back.
package integration

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT64GitalyOperationsSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t64"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t64")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	op := gitalypb.NewOperationServiceClient(conn)
	bs := gitalypb.NewBlobServiceClient(conn)

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/readme.md", "project\n")
	lfsOID := strings.Repeat("ab", 32)
	write(t, src+"/assets/logo.png", // a hand-crafted LFS pointer, as git-lfs would write
		"version https://git-lfs.github.com/spec/v1\noid sha256:"+lfsOID+"\nsize 524288\n")
	write(t, src+"/doomed.txt", "deleted before the tip\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c1 seed")
	git(t, src, "rm", "-q", "doomed.txt")
	git(t, src, "commit", "-qm", "c2 drop doomed")
	git(t, src, "push", "-q", s.repoURL("proj"), "main")

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}
	remote := func(pattern string) map[string]string {
		out := map[string]string{}
		for _, line := range strings.Split(git(t, src, "ls-remote", s.repoURL("proj"), pattern), "\n") {
			seg := strings.Fields(line)
			if len(seg) == 2 {
				out[seg[1]] = seg[0]
			}
		}
		return out
	}

	t.Run("lfs pointers against git objects", func(t *testing.T) {
		pointerBlobOID := strings.TrimSpace(git(t, src, "rev-parse", "HEAD~1:assets/logo.png"))

		stream, err := bs.ListLFSPointers(ctx, &gitalypb.ListLFSPointersRequest{
			Repository: repo, Revisions: []string{"main"}})
		require.NoError(t, err)
		var got []*gitalypb.LFSPointer
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			got = append(got, resp.GetLfsPointers()...)
		}
		require.Len(t, got, 1)
		assert.Equal(t, pointerBlobOID, got[0].GetOid())
		assert.Equal(t, lfsOID, string(got[0].GetFileOid()))
		assert.Equal(t, int64(524288), got[0].GetFileSize())
		// The pointer blob's git rendering and the wire Data agree.
		assert.Equal(t,
			strings.TrimSpace(git(t, src, "cat-file", "-p", pointerBlobOID))+"\n",
			string(got[0].GetData()))

		// Get filters non-pointers; git tree/commit OIDs answer silence.
		treeOID := strings.TrimSpace(git(t, src, "rev-parse", "HEAD^{tree}"))
		commitOID := strings.TrimSpace(git(t, src, "rev-parse", "HEAD"))
		getStream, err := bs.GetLFSPointers(ctx, &gitalypb.GetLFSPointersRequest{
			Repository: repo, BlobIds: []string{pointerBlobOID, treeOID, commitOID}})
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

	t.Run("branch lifecycle via ls-remote", func(t *testing.T) {
		mainOID := remote("refs/heads/main")["refs/heads/main"]
		require.NotEmpty(t, mainOID)

		out, err := op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo, BranchName: []byte("review"), StartPoint: []byte("main")})
		require.NoError(t, err)
		assert.Equal(t, mainOID, out.GetBranch().GetTargetCommit().GetId())
		assert.Equal(t, mainOID, remote("refs/heads/review")["refs/heads/review"])

		_, err = op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo, BranchName: []byte("review"), StartPoint: []byte("main")})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))

		_, err = op.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
			Repository: repo, BranchName: []byte("review"), ExpectedOldOid: mainOID})
		require.NoError(t, err)
		assert.Empty(t, remote("refs/heads/review"))
	})

	t.Run("tags round-trip through git", func(t *testing.T) {
		mainOID := remote("refs/heads/main")["refs/heads/main"]
		out, err := op.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
			Repository: repo, TagName: []byte("v9.9"), TargetRevision: []byte("main"),
			Message: []byte("annotated by t64\n"), User: &gitalypb.User{
				GlId: "user-9", Name: []byte("Tag Ger"), Email: []byte("tagger@example.com")},
			Timestamp: timestamppb.New(time.Unix(1735689600, 0))})
		require.NoError(t, err)
		tagOID := out.GetTag().GetId()
		assert.Equal(t, tagOID, remote("refs/tags/v9.9")["refs/tags/v9.9"])

		git(t, src, "fetch", "-q", s.repoURL("proj"), "refs/tags/v9.9:refs/tags/v9.9")
		body := git(t, src, "cat-file", "-p", "v9.9")
		assert.Contains(t, body, "object "+mainOID)
		assert.Contains(t, body, "type commit")
		assert.Contains(t, body, "tag v9.9")
		assert.Contains(t, body, "tagger Tag Ger <tagger@example.com> 1735689600")
		assert.Contains(t, body, "annotated by t64")
		// git itself peels the fetched tag to the pushed commit.
		assert.Equal(t, mainOID, strings.TrimSpace(git(t, src, "rev-parse", "v9.9^{commit}")))

		_, err = op.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
			Repository: repo, TagName: []byte("v9.9"), TargetRevision: []byte("main")})
		assert.Equal(t, codes.AlreadyExists, status.Code(err))

		lw, err := op.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
			Repository: repo, TagName: []byte("light"), TargetRevision: []byte("main")})
		require.NoError(t, err)
		assert.Equal(t, mainOID, lw.GetTag().GetId())

		_, err = op.UserDeleteTag(ctx, &gitalypb.UserDeleteTagRequest{Repository: repo, TagName: []byte("light")})
		require.NoError(t, err)
		assert.Empty(t, remote("refs/tags/light"))
	})

	t.Run("fast-forward and update branch", func(t *testing.T) {
		_, err := op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo, BranchName: []byte("ff"), StartPoint: []byte("main")})
		require.NoError(t, err)

		write(t, src+"/next.txt", "advance\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "c3 advance")
		git(t, src, "push", "-q", s.repoURL("proj"), "main")
		newOID := remote("refs/heads/main")["refs/heads/main"]
		oldOID := remote("refs/heads/ff")["refs/heads/ff"]

		res, err := op.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
			Repository: repo, Branch: []byte("ff"), CommitId: newOID})
		require.NoError(t, err)
		assert.Equal(t, newOID, res.GetBranchUpdate().GetCommitId())
		assert.Equal(t, newOID, remote("refs/heads/ff")["refs/heads/ff"])

		_, err = op.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
			Repository: repo, Branch: []byte("ff"), CommitId: oldOID})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Equal(t, "not fast forward", status.Convert(err).Message())

		_, err = op.UserUpdateBranch(ctx, &gitalypb.UserUpdateBranchRequest{
			Repository: repo, BranchName: []byte("ff"),
			Newrev: []byte(oldOID), Oldrev: []byte(newOID)})
		require.NoError(t, err)
		assert.Equal(t, oldOID, remote("refs/heads/ff")["refs/heads/ff"])
	})

	t.Run("user commit files reads back as an ordinary commit", func(t *testing.T) {
		stream, err := op.UserCommitFiles(ctx)
		require.NoError(t, err)
		send := func(m *gitalypb.UserCommitFilesRequest) {
			require.NoError(t, stream.Send(m))
		}
		send(&gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
				Header: &gitalypb.UserCommitFilesRequestHeader{
					Repository:        repo,
					BranchName:        []byte("web"),
					StartBranchName:   []byte("main"),
					CommitMessage:     []byte("edit from the web UI\n\nsecond paragraph\n"),
					CommitAuthorName:  []byte("Web Editor"),
					CommitAuthorEmail: []byte("editor@example.com"),
					User:              &gitalypb.User{GlId: "user-7", Name: []byte("Project Bot"), Email: []byte("bot@example.com")},
					Timestamp:         timestamppb.New(time.Unix(1735689700, 0)),
				}}})
		action := func(h *gitalypb.UserCommitFilesActionHeader, content []byte) {
			send(&gitalypb.UserCommitFilesRequest{
				UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
					Action: &gitalypb.UserCommitFilesAction{
						UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Header{Header: h}}}})
			if content != nil {
				send(&gitalypb.UserCommitFilesRequest{
					UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
						Action: &gitalypb.UserCommitFilesAction{
							UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Content{Content: content}}}})
			}
		}
		action(&gitalypb.UserCommitFilesActionHeader{
			Action: gitalypb.UserCommitFilesActionHeader_CREATE, FilePath: []byte("web/added.txt")}, []byte("added via stream\n"))
		action(&gitalypb.UserCommitFilesActionHeader{
			Action: gitalypb.UserCommitFilesActionHeader_UPDATE, FilePath: []byte("readme.md")}, []byte("project v2\n"))
		action(&gitalypb.UserCommitFilesActionHeader{
			Action: gitalypb.UserCommitFilesActionHeader_MOVE, FilePath: []byte("assets/renamed.png"),
			PreviousPath: []byte("assets/logo.png"), InferContent: true}, nil)
		action(&gitalypb.UserCommitFilesActionHeader{
			Action: gitalypb.UserCommitFilesActionHeader_DELETE, FilePath: []byte("next.txt")}, nil)
		res, err := stream.CloseAndRecv()
		require.NoError(t, err)
		commitID := res.GetBranchUpdate().GetCommitId()
		require.NotEmpty(t, commitID)
		assert.True(t, res.GetBranchUpdate().GetBranchCreated())

		// git fetches the branch and reads the commit: full oracle round-trip.
		git(t, src, "fetch", "-q", s.repoURL("proj"), "web")
		fetched := strings.TrimSpace(git(t, src, "rev-parse", "FETCH_HEAD"))
		assert.Equal(t, commitID, fetched)

		assert.Equal(t, "Web Editor <editor@example.com>",
			strings.TrimSpace(git(t, src, "show", "-s", "--format=%an <%ae>", commitID)))
		assert.Equal(t, "Project Bot <bot@example.com>",
			strings.TrimSpace(git(t, src, "show", "-s", "--format=%cn <%ce>", commitID)))
		assert.Equal(t, "1735689700",
			strings.TrimSpace(git(t, src, "show", "-s", "--format=%ct", commitID)))
		assert.Equal(t, "edit from the web UI\n\nsecond paragraph\n",
			strings.TrimRight(git(t, src, "show", "-s", "--format=%B", commitID), "\n")+"\n")

		for path, want := range map[string]string{
			"web/added.txt":     "added via stream\n",
			"readme.md":         "project v2\n",
			"assets/renamed.png": "version https://git-lfs.github.com/spec/v1\noid sha256:" + lfsOID + "\nsize 524288\n",
		} {
			got := git(t, src, "show", commitID+":"+path)
			assert.Equal(t, want, got, path)
		}
		_, derr := gitTry(src, "show", commitID+":next.txt")
		require.Error(t, derr, "next.txt deleted from the tree")
	})
}
