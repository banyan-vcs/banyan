package gitaly

import (
	"context"
	"io"
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

// TestGitalyMergeSurface covers the five user-facing merge RPCs over
// divergent branch fixtures (docs/19): clean merges verify commit shape,
// conflicts verify the structured error details Rails parses.
func TestGitalyMergeSurface(t *testing.T) {
	srv, _, objects, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()
	op := gitalypb.NewOperationServiceClient(conn)

	repoName := "merge-lab"
	repo := repoRef(repoName)
	require.NoError(t, svc.Meta.CreateRepo(ctx, repoName, model.SHA1))
	// Base history: one file, one commit.
	blobA := model.ObjectOID(model.BlobType, []byte("alpha v1\n"), model.SHA1)
	require.NoError(t, svc.PutObjects(ctx, repoName, []service.PutObject{
		{OID: blobA, Type: "blob", Data: []byte("alpha v1\n")}}))
	base, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repoName,
		Message: "base", Changes: []service.Change{{Path: "alpha.txt", Blob: blobA}}})
	require.NoError(t, err)
	mainTip := base.Head

	commitOn := func(branch string, baseHead model.OID, path, content string) model.OID {
		t.Helper()
		blob := model.ObjectOID(model.BlobType, []byte(content), model.SHA1)
		require.NoError(t, svc.PutObjects(ctx, repoName, []service.PutObject{
			{OID: blob, Type: "blob", Data: []byte(content)}}))
		res, err := svc.CreateCommit(ctx, service.CommitRequest{Repo: repoName, Branch: branch,
			Base: baseHead, Message: "edit " + path, Changes: []service.Change{{Path: path, Blob: blob}}})
		require.NoError(t, err)
		return res.Head
	}
	branchAt := func(branch string, at model.OID) {
		t.Helper()
		_, err := op.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo, BranchName: []byte(branch), StartPoint: []byte(at.String())})
		require.NoError(t, err)
	}
	// advertised reads the branch tip exactly as a client would (the woven
	// view) — after merges the raw timeline head and the advertised tip
	// legitimately differ (linearization), and clients always rebase their
	// next base onto what they read.
	advertised := func(branch string) model.OID {
		t.Helper()
		oid, err := srv.resolveRevision(ctx, repoName, "refs/heads/"+branch)
		require.NoError(t, err)
		return oid
	}

	user := &gitalypb.User{GlId: "user-1", Name: []byte("Mer Ger"), Email: []byte("merger@example.com")}
	ts := timestamppb.New(time.Unix(1700001000, 0))

	t.Run("clean merge branch", func(t *testing.T) {
		branchAt("feature", mainTip)
		featTip := commitOn("feature", mainTip, "feat.txt", "feature work\n")

		stream, err := op.UserMergeBranch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			CommitId: featTip.String(), Message: []byte("Merge branch 'feature'\n"), Timestamp: ts,
		}))
		first, err := stream.Recv()
		require.NoError(t, err)
		mergeOID := first.GetCommitId()
		require.NotEmpty(t, mergeOID)

		require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{Apply: true}))
		second, err := stream.Recv()
		require.NoError(t, err)
		if err == nil {
			_, rerr := stream.Recv()
			require.ErrorIs(t, rerr, io.EOF)
		}
		assert.Equal(t, mergeOID, second.GetBranchUpdate().GetCommitId())

		typ, raw, err := objects.Raw(ctx, repoName, model.OID(mergeOID))
		require.NoError(t, err)
		require.Equal(t, model.CommitType, typ)
		c, err := model.DecodeCommit(raw)
		require.NoError(t, err)
		assert.Equal(t, []model.OID{mainTip, featTip}, c.Parents, "two-parent merge")
		assert.Equal(t, "Merge branch 'feature'\n", c.Message)
		assert.Equal(t, "Mer Ger", c.Author.Name)
		for _, path := range []string{"alpha.txt", "feat.txt"} {
			e, err := srv.entryAtPath(ctx, repoName, c.TreeOID, path)
			require.NoError(t, err, path)
			require.NotNil(t, e, path)
		}
		// The advertised tip is the linearized twin of the merge (tree
		// identical); clients continue from what they read.
		mainTip = advertised("main")
	})

	t.Run("conflict surfaces structured detail", func(t *testing.T) {
		branchAt("clash", mainTip)
		ours := commitOn("main", mainTip, "alpha.txt", "ours wins here\n")
		theirs := commitOn("clash", mainTip, "alpha.txt", "theirs wins here\n")

		stream, err := op.UserMergeBranch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			CommitId: theirs.String(), Message: []byte("Merge branch 'clash'\n"), Timestamp: ts,
		}))
		_, err = stream.Recv()
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))

		var detail *gitalypb.UserMergeBranchError
		for _, d := range status.Convert(err).Details() {
			if e, ok := d.(*gitalypb.UserMergeBranchError); ok {
				detail = e
			}
		}
		require.NotNil(t, detail, "structured merge-conflict detail")
		mc := detail.GetMergeConflict()
		require.NotNil(t, mc)
		assert.Equal(t, [][]byte{[]byte("alpha.txt")}, mc.GetConflictingFiles())
		assert.Equal(t, []string{ours.String(), theirs.String()}, mc.GetConflictingCommitIds())
		mainTip = ours // main moved to ours only
	})

	t.Run("merge aborted by client", func(t *testing.T) {
		branchAt("abort", mainTip)
		featTip := commitOn("abort", mainTip, "abort.txt", "aborted work\n")

		stream, err := op.UserMergeBranch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			CommitId: featTip.String(), Message: []byte("Merge branch 'abort'\n"), Timestamp: ts,
		}))
		first, err := stream.Recv()
		require.NoError(t, err)
		require.NotEmpty(t, first.GetCommitId())

		require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{Apply: false}))
		_, err = stream.Recv()
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "aborted")
	})

	t.Run("merge to ref", func(t *testing.T) {
		branchAt("train", mainTip)
		featTip := commitOn("train", mainTip, "train.txt", "train work\n")

		out, err := op.UserMergeToRef(ctx, &gitalypb.UserMergeToRefRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			SourceSha: featTip.String(), TargetRef: []byte("refs/merge-requests/7/head"),
			Message: []byte("merge train result\n"), Timestamp: ts,
		})
		require.NoError(t, err)
		require.NotEmpty(t, out.GetCommitId())

		head, ok, _ := (&operationService{s: srv}).timelineHead(ctx, repoName,
			fullRefTimeline(repoName, "refs/merge-requests/7/head"))
		require.True(t, ok)
		assert.Equal(t, out.GetCommitId(), head.String())

		_, err = op.UserMergeToRef(ctx, &gitalypb.UserMergeToRefRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			SourceSha: featTip.String(), TargetRef: []byte("refs/heads/nope"), Message: []byte("x\n"),
		})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "target ref must be refs/merge-requests/*")
	})

	t.Run("squash", func(t *testing.T) {
		branchAt("bulk", mainTip)
		c1 := commitOn("bulk", mainTip, "s1.txt", "one\n")
		c2 := commitOn("bulk", c1, "s2.txt", "two\n")

		out, err := op.UserSquash(ctx, &gitalypb.UserSquashRequest{
			Repository: repo, User: user,
			StartSha: mainTip.String(), EndSha: c2.String(),
			Author:        &gitalypb.User{Name: []byte("Squash Er"), Email: []byte("squash@example.com")},
			CommitMessage: []byte("squashed work"), Timestamp: ts,
		})
		require.NoError(t, err)
		squashOID := model.OID(out.GetSquashSha())
		require.False(t, squashOID.IsZero())

		typ, raw, err := objects.Raw(ctx, repoName, squashOID)
		require.NoError(t, err)
		require.Equal(t, model.CommitType, typ)
		c, err := model.DecodeCommit(raw)
		require.NoError(t, err)
		assert.Equal(t, []model.OID{mainTip}, c.Parents, "single parent = start")
		assert.Equal(t, "Squash Er", c.Author.Name)
		assert.Equal(t, "squashed work\n", c.Message, "message gains the trailing newline")
		// The squash tree is the end commit's tree.
		endRec, err := srv.commitRecord(ctx, repoName, c2)
		require.NoError(t, err)
		assert.Equal(t, endRec.Tree, c.TreeOID)
	})

	t.Run("cherry-pick and revert", func(t *testing.T) {
		branchAt("pick", mainTip)
		picked := commitOn("pick", mainTip, "picked.txt", "picked work\n")

		out, err := op.UserCherryPick(ctx, &gitalypb.UserCherryPickRequest{
			Repository: repo, User: user, BranchName: []byte("main"),
			Commit: &gitalypb.GitCommit{Id: picked.String()},
			Message: []byte("cherry: picked work\n"), Timestamp: ts,
		})
		require.NoError(t, err)
		newOID := model.OID(out.GetBranchUpdate().GetCommitId())
		typ, raw, err := objects.Raw(ctx, repoName, newOID)
		require.NoError(t, err)
		c, err := model.DecodeCommit(raw)
		require.NoError(t, err)
		assert.Equal(t, model.CommitType, typ)
		assert.Equal(t, []model.OID{mainTip}, c.Parents)
		e, err := srv.entryAtPath(ctx, repoName, c.TreeOID, "picked.txt")
		require.NoError(t, err)
		require.NotNil(t, e, "picked change applied onto main")

		// Picking it again changes nothing: the empty-result error.
		_, err = op.UserCherryPick(ctx, &gitalypb.UserCherryPickRequest{
			Repository: repo, User: user, BranchName: []byte("main"),
			Commit: &gitalypb.GitCommit{Id: picked.String()},
			Message: []byte("cherry: again\n"), Timestamp: ts,
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		var already bool
		for _, d := range status.Convert(err).Details() {
			if e, ok := d.(*gitalypb.UserCherryPickError); ok && e.GetChangesAlreadyApplied() != nil {
				already = true
			}
		}
		assert.True(t, already, "changes-already-applied detail")
		mainTip = newOID

		rev, err := op.UserRevert(ctx, &gitalypb.UserRevertRequest{
			Repository: repo, User: user, BranchName: []byte("main"),
			Commit: &gitalypb.GitCommit{Id: newOID.String()},
			Message: []byte("Revert \"cherry: picked work\"\n"), Timestamp: ts,
		})
		require.NoError(t, err)
		revOID := model.OID(rev.GetBranchUpdate().GetCommitId())
		typ, raw, err = objects.Raw(ctx, repoName, revOID)
		require.NoError(t, err)
		rc, err := model.DecodeCommit(raw)
		require.NoError(t, err)
		assert.Equal(t, "Mer Ger", rc.Author.Name, "revert authors as the acting user")
		e, err = srv.entryAtPath(ctx, repoName, rc.TreeOID, "picked.txt")
		require.NoError(t, err)
		assert.Nil(t, e, "reverted change is gone")
		mainTip = revOID
	})
}
