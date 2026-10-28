package gitaly

import (
	"context"
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

// TestGitalyRebaseAndPatchSurface covers slice 7 (docs/19): the two rebase
// RPCs, the mbox patch flow and the submodule pointer update.
func TestGitalyRebaseAndPatchSurface(t *testing.T) {
	srv, _, objects, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	op := gitalypb.NewOperationServiceClient(conn)

	repoName := "rebase-lab"
	repo := repoRef(repoName)
	require.NoError(t, svc.Meta.CreateRepo(ctx, repoName, model.SHA1))
	commitFile := func(branch string, base model.OID, path, content string) model.OID {
		t.Helper()
		blob := model.ObjectOID(model.BlobType, []byte(content), model.SHA1)
		require.NoError(t, svc.PutObjects(ctx, repoName, []service.PutObject{
			{OID: blob, Type: "blob", Data: []byte(content)}}))
		res, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repoName, Branch: branch,
			Base: base, Message: "edit " + path, Changes: []service.Change{{Path: path, Blob: blob}}})
		require.NoError(t, err)
		return res.Head
	}

	user := &gitalypb.User{GlId: "user-1", Name: []byte("Ree Baser"), Email: []byte("rebase@example.com")}
	ts := timestamppb.New(time.Unix(1700002000, 0))

	// Base: one commit on main; feature adds two commits; main adds one
	// more (divergence without conflicts).
	base := commitFile("main", model.OID(""), "shared.txt", "shared base\n")
	_, err = op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
		Repository: repo, BranchName: []byte("feature"), StartPoint: []byte(base.String())})
	require.NoError(t, err)
	f1 := commitFile("feature", base, "f1.txt", "feature one\n")
	f2 := commitFile("feature", f1, "f2.txt", "feature two\n")
	m1 := commitFile("main", base, "m1.txt", "main one\n")
	_ = m1

	t.Run("rebase to ref replays the branch", func(t *testing.T) {
		out, err := op.UserRebaseToRef(ctx, &gitalypb.UserRebaseToRefRequest{
			Repository: repo, User: user,
			SourceSha:      f2.String(),
			TargetRef:      []byte("refs/merge-requests/11/head"),
			FirstParentRef: []byte("main"),
			Timestamp:      ts,
		})
		require.NoError(t, err)
		rebased := model.OID(out.GetCommitId())

		// Linear replay on top of main's advertised tip: the chain walks
		// replay-f1 → main's tip, authors preserved.
		rec, err := srv.commitRecord(ctx, repoName, rebased)
		require.NoError(t, err)
		advertised, _, _ := (&operationService{s: srv}).branchState(ctx, repoName, "main")
		require.Len(t, rec.Parents, 1)
		parentRec, err := srv.commitRecord(ctx, repoName, rec.Parents[0])
		require.NoError(t, err)
		require.Equal(t, []model.OID{advertised}, parentRec.Parents, "replay sits on main's tip")
		typ, raw, err := objects.Raw(ctx, repoName, rebased)
		require.NoError(t, err)
		c, err := model.DecodeCommit(raw)
		require.NoError(t, err)
		require.Equal(t, model.CommitType, typ)
		assert.Equal(t, "banyan", c.Author.Name, "fixture commits author as banyan")
		assert.Equal(t, "Ree Baser", c.Committer.Name, "rebase committer is the acting user")

		// The replayed tip's tree carries both sides' files.
		for _, path := range []string{"shared.txt", "m1.txt", "f1.txt", "f2.txt"} {
			e, err := srv.entryAtPath(ctx, repoName, rec.Tree, path)
			require.NoError(t, err, path)
			require.NotNil(t, e, path)
		}
		// The replayed tip's message survives the rewrite.
		_, praw, err := objects.Raw(ctx, repoName, rebased)
		require.NoError(t, err)
		pc, err := model.DecodeCommit(praw)
		require.NoError(t, err)
		assert.Equal(t, "edit f2.txt\n", pc.Message, "replayed commit keeps its message")

		// The target ref advertises the rebased tip.
		head, ok, _ := (&operationService{s: srv}).timelineHead(ctx, repoName,
			fullRefTimeline(repoName, "refs/merge-requests/11/head"))
		require.True(t, ok)
		assert.Equal(t, rebased, head)
	})

	t.Run("rebase confirmable moves the branch", func(t *testing.T) {
		stream, err := op.UserRebaseConfirmable(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserRebaseConfirmableRequest{
			UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Header_{Header: &gitalypb.UserRebaseConfirmableRequest_Header{
				Repository:       repo,
				User:             user,
				Branch:           []byte("feature"),
				BranchSha:        f2.String(),
				RemoteRepository: repo,
				RemoteBranch:     []byte("main"),
				Timestamp:        ts,
			}}}))
		first, err := stream.Recv()
		require.NoError(t, err)
		rebaseSha := first.GetRebaseSha()
		require.NotEmpty(t, rebaseSha)

		require.NoError(t, stream.Send(&gitalypb.UserRebaseConfirmableRequest{
			UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Apply{Apply: true}}))
		second, err := stream.Recv()
		require.NoError(t, err)
		assert.True(t, second.GetRebaseApplied())

		advertised, _, _ := (&operationService{s: srv}).branchState(ctx, repoName, "feature")
		assert.Equal(t, rebaseSha, advertised.String(), "branch now sits at the rebased tip")
	})

	t.Run("rebase confirmable abort", func(t *testing.T) {
		// Rebase feature onto main again — now a no-op replay (already
		// rebased) — and abort; the branch must not move.
		before, _, _ := (&operationService{s: srv}).branchState(ctx, repoName, "feature")
		stream, err := op.UserRebaseConfirmable(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserRebaseConfirmableRequest{
			UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Header_{Header: &gitalypb.UserRebaseConfirmableRequest_Header{
				Repository:       repo,
				User:             user,
				Branch:           []byte("feature"),
				BranchSha:        before.String(),
				RemoteRepository: repo,
				RemoteBranch:     []byte("main"),
				Timestamp:        ts,
			}}}))
		_, err = stream.Recv()
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserRebaseConfirmableRequest{
			UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Apply{Apply: false}}))
		_, err = stream.Recv()
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		after, _, _ := (&operationService{s: srv}).branchState(ctx, repoName, "feature")
		assert.Equal(t, before, after, "abort leaves the branch untouched")
	})

	t.Run("apply patch series", func(t *testing.T) {
		mbox := "From 1111111 Mon Sep 17 00:00:00 2001\n" +
			"From: Patch Author <author@example.com>\n" +
			"Date: Thu, 1 Jan 2026 00:00:00 +0000\n" +
			"Subject: [PATCH] add and edit files\n" +
			"\n" +
			"Adds new.txt and edits shared.txt.\n" +
			"\n" +
			"diff --git a/new.txt b/new.txt\n" +
			"new file mode 100644\n" +
			"index 0000000..e69de29\n" +
			"--- /dev/null\n" +
			"+++ b/new.txt\n" +
			"@@ -0,0 +1 @@\n" +
			"+patched content\n" +
			"diff --git a/shared.txt b/shared.txt\n" +
			"index 1234567..89abcde 100644\n" +
			"--- a/shared.txt\n" +
			"+++ b/shared.txt\n" +
			"@@ -1 +1,2 @@\n" +
			" shared base\n" +
			"+second line from patch\n"

		stream, err := op.UserApplyPatch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
			UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{Header: &gitalypb.UserApplyPatchRequest_Header{
				Repository:   repo,
				User:         user,
				TargetBranch: []byte("main"),
				Timestamp:    ts,
			}}}))
		require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
			UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Patches{Patches: []byte(mbox)}}))
		res, err := stream.CloseAndRecv()
		require.NoError(t, err)
		commitID := model.OID(res.GetBranchUpdate().GetCommitId())
		require.False(t, commitID.IsZero())

		typ, raw, err := objects.Raw(ctx, repoName, commitID)
		require.NoError(t, err)
		require.Equal(t, model.CommitType, typ)
		c, err := model.DecodeCommit(raw)
		require.NoError(t, err)
		assert.Equal(t, "Patch Author", c.Author.Name)
		assert.Equal(t, "add and edit files\n\nAdds new.txt and edits shared.txt.\n", c.Message)

		rec, err := srv.commitRecord(ctx, repoName, commitID)
		require.NoError(t, err)
		e, err := srv.entryAtPath(ctx, repoName, rec.Tree, "new.txt")
		require.NoError(t, err)
		require.NotNil(t, e)
		_, data, err := objects.Raw(ctx, repoName, e.Target)
		require.NoError(t, err)
		assert.Equal(t, "patched content\n", string(data))
		e, err = srv.entryAtPath(ctx, repoName, rec.Tree, "shared.txt")
		require.NoError(t, err)
		_, data, err = objects.Raw(ctx, repoName, e.Target)
		require.NoError(t, err)
		assert.Equal(t, "shared base\nsecond line from patch\n", string(data))
	})

	t.Run("apply patch conflict", func(t *testing.T) {
		mbox := "From 2222222 Mon Sep 17 00:00:00 2001\n" +
			"From: Patch Author <author@example.com>\n" +
			"Subject: [PATCH] conflicting edit\n" +
			"\n" +
			"diff --git a/shared.txt b/shared.txt\n" +
			"--- a/shared.txt\n" +
			"+++ b/shared.txt\n" +
			"@@ -1,2 +1,2 @@\n" +
			"totally different context\n" +
			"-shared base\n" +
			"+changed\n"
		stream, err := op.UserApplyPatch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
			UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{Header: &gitalypb.UserApplyPatchRequest_Header{
				Repository:   repo,
				User:         user,
				TargetBranch: []byte("main"),
				Timestamp:    ts,
			}}}))
		require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
			UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Patches{Patches: []byte(mbox)}}))
		_, err = stream.CloseAndRecv()
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "Patch failed at 0001")
	})

	t.Run("update submodule pointer", func(t *testing.T) {
		// Seed a gitlink entry via the service path (gitlinks skip blob
		// validation; the target commit lives in the submodule repo).
		fake := model.OID("0123456789012345678901234567890123456789")
		seed, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repoName, Branch: "submod",
			Message: "seed submodule", Changes: []service.Change{{
				Path: "vendor/lib", Blob: fake, Mode: "160000"}}})
		require.NoError(t, err)

		out, err := op.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
			Repository: repo, User: user, Branch: []byte("submod"),
			Submodule:     []byte("vendor/lib"),
			CommitSha:     "9876543210987654321098765432109876543210",
			CommitMessage: []byte("Bump vendor/lib\n"), Timestamp: ts,
		})
		require.NoError(t, err)
		newOID := model.OID(out.GetBranchUpdate().GetCommitId())

		rec, err := srv.commitRecord(ctx, repoName, newOID)
		require.NoError(t, err)
		e, err := srv.entryAtPath(ctx, repoName, rec.Tree, "vendor/lib")
		require.NoError(t, err)
		require.NotNil(t, e)
		assert.Equal(t, "160000", e.Mode)
		assert.Equal(t, "9876543210987654321098765432109876543210", e.Target.String())
		assert.Equal(t, []model.OID{seed.Head}, rec.Parents)

		// Already-at and invalid-path shapes.
		_, err = op.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
			Repository: repo, User: user, Branch: []byte("submod"),
			Submodule:     []byte("vendor/lib"),
			CommitSha:     "9876543210987654321098765432109876543210",
			CommitMessage: []byte("again\n"), Timestamp: ts,
		})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "already at")

		_, err = op.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
			Repository: repo, User: user, Branch: []byte("submod"),
			Submodule:     []byte("shared.txt"),
			CommitSha:     "0123456789012345678901234567890123456789",
			CommitMessage: []byte("nope\n"), Timestamp: ts,
		})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}
