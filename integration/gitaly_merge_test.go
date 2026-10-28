// T65 — user-facing merge surface (docs/19): UserMergeBranch, UserMergeToRef,
// UserSquash, UserCherryPick and UserRevert, verified against git's own
// merge machinery — merge-tree --write-tree for the merged tree, real
// cherry-pick/revert in a local clone for the applied changes.
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

func TestT65GitalyMergeSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t65"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t65")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	op := gitalypb.NewOperationServiceClient(conn)
	ds := gitalypb.NewDiffServiceClient(conn)

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/alpha.txt", "alpha base\n")
	write(t, src+"/keep.txt", "kept\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c1 base")
	git(t, src, "checkout", "-q", "-b", "feature")
	write(t, src+"/feat.txt", "feature work\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c2 feature work")
	git(t, src, "checkout", "-q", "main")
	write(t, src+"/main-side.txt", "main side work\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c3 main side work")
	git(t, src, "push", "-q", s.repoURL("proj"), "main", "feature")

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}
	user := &gitalypb.User{GlId: "user-1", Name: []byte("Mer Ger"), Email: []byte("merger@example.com")}
	ts := timestamppb.New(time.Unix(1735689900, 0))

	remoteTip := func(ref string) string {
		out := git(t, src, "ls-remote", s.repoURL("proj"), ref)
		fields := strings.Fields(out)
		require.Len(t, fields, 2, "ref %s: %q", ref, out)
		return fields[0]
	}
	fetchMain := func() string {
		git(t, src, "fetch", "-q", s.repoURL("proj"), "main")
		return strings.TrimSpace(git(t, src, "rev-parse", "FETCH_HEAD"))
	}

	t.Run("merge branch equals git merge-tree", func(t *testing.T) {
		// git's own merge of the same pair, computed locally first.
		gitTree := strings.TrimSpace(git(t, src, "merge-tree", "--write-tree", "main", "feature"))
		require.NotEmpty(t, gitTree)

		stream, err := op.UserMergeBranch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			CommitId: remoteTip("refs/heads/feature"),
			Message:  []byte("Merge branch 'feature' into main\n"), Timestamp: ts,
		}))
		first, err := stream.Recv()
		require.NoError(t, err)
		mergeOID := first.GetCommitId()
		require.NotEmpty(t, mergeOID)

		require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{Apply: true}))
		second, err := stream.Recv()
		require.NoError(t, err)
		assert.Equal(t, mergeOID, second.GetBranchUpdate().GetCommitId())

		// git fetches the merged branch: the tree byte-equals git's merge.
		fetched := fetchMain()
		fetchedTree := strings.TrimSpace(git(t, src, "rev-parse", fetched+"^{tree}"))
		assert.Equal(t, gitTree, fetchedTree, "merged tree equals git merge-tree")
		lsTree := git(t, src, "ls-tree", "--name-only", fetched)
		for _, path := range []string{"alpha.txt", "keep.txt", "feat.txt", "main-side.txt"} {
			assert.Contains(t, lsTree, path, "merged tree carries %s", path)
		}
	})

	t.Run("merge to ref lands in refs/merge-requests", func(t *testing.T) {
		out, err := op.UserMergeToRef(ctx, &gitalypb.UserMergeToRefRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			SourceSha: remoteTip("refs/heads/feature"),
			TargetRef: []byte("refs/merge-requests/9/head"),
			Message:   []byte("merge train result\n"), Timestamp: ts,
		})
		require.NoError(t, err)
		assert.Equal(t, out.GetCommitId(), remoteTip("refs/merge-requests/9/head"))

		git(t, src, "fetch", "-q", s.repoURL("proj"), "refs/merge-requests/9/head")
		head := strings.TrimSpace(git(t, src, "rev-parse", "FETCH_HEAD"))
		assert.Equal(t, out.GetCommitId(), head)
		parents := strings.Fields(git(t, src, "show", "-s", "--format=%P", head))
		assert.Len(t, parents, 2, "merge-train ref carries a two-parent merge commit")
	})

	t.Run("conflict agrees with git merge-tree verdict", func(t *testing.T) {
		// Branch clash from the common base; both sides edit alpha.txt.
		base := strings.TrimSpace(git(t, src, "merge-base", "main", "feature"))
		git(t, src, "checkout", "-q", "-b", "clash", base)
		write(t, src+"/alpha.txt", "clash side\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "clash edit")
		// Sync local main to the merged remote main, then edit the same file.
		git(t, src, "checkout", "-q", "main")
		git(t, src, "pull", "-q", "--rebase", s.repoURL("proj"), "main")
		write(t, src+"/alpha.txt", "main side\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "main edit")
		git(t, src, "push", "-q", s.repoURL("proj"), "main", "clash")

		_, err := gitTry(src, "merge-tree", "--write-tree", "main", "clash")
		require.Error(t, err, "git itself reports the conflict")

		stream, err := op.UserMergeBranch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			CommitId: remoteTip("refs/heads/clash"),
			Message:  []byte("Merge branch 'clash'\n"), Timestamp: ts,
		}))
		_, err = stream.Recv()
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		var files [][]byte
		for _, d := range status.Convert(err).Details() {
			if e, ok := d.(*gitalypb.UserMergeBranchError); ok {
				files = e.GetMergeConflict().GetConflictingFiles()
			}
		}
		assert.Equal(t, [][]byte{[]byte("alpha.txt")}, files)
	})

	t.Run("squash result carries the end tree", func(t *testing.T) {
		start := strings.TrimSpace(git(t, src, "merge-base", "main", "feature"))
		featTip := remoteTip("refs/heads/feature")
		out, err := op.UserSquash(ctx, &gitalypb.UserSquashRequest{
			Repository: repo, User: user,
			StartSha: start, EndSha: featTip,
			Author:        &gitalypb.User{Name: []byte("Squash Er"), Email: []byte("squash@example.com")},
			CommitMessage: []byte("squashed feature\n"), Timestamp: ts,
		})
		require.NoError(t, err)
		res, err := gitalypb.NewCommitServiceClient(conn).FindCommit(ctx, &gitalypb.FindCommitRequest{
			Repository: repo, Revision: []byte(out.GetSquashSha())})
		require.NoError(t, err)
		commit := res.GetCommit()
		require.NotNil(t, commit)
		assert.Equal(t, "Squash Er", string(commit.GetAuthor().GetName()), "squash author")

		// Tree equality with the end commit: an empty delta between the
		// squash commit and the end tip.
		deltaStream, err := ds.CommitDelta(ctx, &gitalypb.CommitDeltaRequest{
			Repository: repo, LeftCommitId: out.GetSquashSha(), RightCommitId: featTip})
		require.NoError(t, err)
		n := 0
		for {
			resp, err := deltaStream.Recv()
			if err != nil {
				break
			}
			n += len(resp.GetDeltas())
		}
		assert.Equal(t, 0, n, "squash tree equals the end tree")
	})

	t.Run("cherry-pick and revert against git's own", func(t *testing.T) {
		// A fresh change on a side branch that main does not carry yet.
		git(t, src, "checkout", "-q", "-b", "pickme", "main")
		write(t, src+"/picked.txt", "picked work\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "pickable change")
		git(t, src, "push", "-q", s.repoURL("proj"), "pickme")
		git(t, src, "checkout", "-q", "main")

		// git's own cherry-pick onto main in a scratch clone.
		pickSrc := t.TempDir()
		git(t, pickSrc, "clone", "-q", "--no-local", s.repoURL("proj"), ".")
		git(t, pickSrc, "checkout", "-q", "main")
		picked := remoteTip("refs/heads/pickme")
		git(t, pickSrc, "cherry-pick", picked)
		wantTree := strings.TrimSpace(git(t, pickSrc, "rev-parse", "HEAD^{tree}"))

		out, err := op.UserCherryPick(ctx, &gitalypb.UserCherryPickRequest{
			Repository: repo, User: user, BranchName: []byte("main"),
			Commit:  &gitalypb.GitCommit{Id: picked},
			Message: []byte("cherry: pickable change\n"), Timestamp: ts,
		})
		require.NoError(t, err)
		newOID := out.GetBranchUpdate().GetCommitId()

		fetched := fetchMain()
		gotTree := strings.TrimSpace(git(t, src, "rev-parse", fetched+"^{tree}"))
		assert.Equal(t, wantTree, gotTree, "cherry-picked tree matches git")
		assert.Equal(t,
			strings.TrimSpace(git(t, src, "show", "-s", "--format=%an <%ae>", "pickme")),
			strings.TrimSpace(git(t, src, "show", "-s", "--format=%an <%ae>", fetched)),
			"picked commit's author survives")

		// Picking the same change again is a no-op.
		_, err = op.UserCherryPick(ctx, &gitalypb.UserCherryPickRequest{
			Repository: repo, User: user, BranchName: []byte("main"),
			Commit: &gitalypb.GitCommit{Id: picked},
			Message: []byte("cherry: again\n"), Timestamp: ts,
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))

		// git reverts its own cherry-pick; compare trees.
		git(t, pickSrc, "revert", "--no-edit", "HEAD")
		wantRevTree := strings.TrimSpace(git(t, pickSrc, "rev-parse", "HEAD^{tree}"))

		rev, err := op.UserRevert(ctx, &gitalypb.UserRevertRequest{
			Repository: repo, User: user, BranchName: []byte("main"),
			Commit:  &gitalypb.GitCommit{Id: newOID},
			Message: []byte("Revert \"cherry: pickable change\"\n"), Timestamp: ts,
		})
		require.NoError(t, err)
		require.NotEmpty(t, rev.GetBranchUpdate().GetCommitId())
		fetched = fetchMain()
		gotRevTree := strings.TrimSpace(git(t, src, "rev-parse", fetched+"^{tree}"))
		assert.Equal(t, wantRevTree, gotRevTree, "reverted tree matches git")
		assert.Equal(t, "Mer Ger",
			strings.TrimSpace(git(t, src, "show", "-s", "--format=%an", fetched)),
			"revert authors as the acting user")
	})

	t.Run("fast-forward after merge converges both views", func(t *testing.T) {
		git(t, src, "checkout", "-q", "main")
		git(t, src, "pull", "-q", "--rebase", s.repoURL("proj"), "main")
		git(t, src, "checkout", "-q", "-b", "ff-src")
		write(t, src+"/more.txt", "more work\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "c4 advance")
		git(t, src, "push", "-q", s.repoURL("proj"), "ff-src")

		out, err := op.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			CommitId: remoteTip("refs/heads/ff-src")})
		require.NoError(t, err)
		target := remoteTip("refs/heads/ff-src")
		assert.Equal(t, target, out.GetBranchUpdate().GetCommitId())
		assert.Equal(t, target, remoteTip("refs/heads/main"), "main fast-forwards to the target")

		fetched := fetchMain()
		assert.Equal(t, target, fetched)
	})
}
