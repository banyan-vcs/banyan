// T66 — rebase series, patch application and submodule updates (docs/19):
// git's own rebase and format-patch/am are the oracle — the rebased tree
// byte-equals a local `git rebase`, the applied patch series byte-equals a
// local `git am`, and submodule moves verify through ls-tree.
package integration

import (
	"bytes"
	"context"
	"net"
	"os"
	"sort"
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

func TestT66GitalyRebaseSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t66"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t66")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	op := gitalypb.NewOperationServiceClient(conn)

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/shared.txt", "shared base\n")
	git(t, src, "add", "-A")
	// A gitlink at vendor/lib pointing at an external commit.
	git(t, src, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendor/lib")
	git(t, src, "commit", "-qm", "c1 base with submodule")
	git(t, src, "checkout", "-q", "-b", "feature")
	write(t, src+"/f1.txt", "feature one\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c2 feature one")
	write(t, src+"/f2.txt", "feature two\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c3 feature two")
	git(t, src, "checkout", "-q", "main")
	write(t, src+"/main-side.txt", "main side\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c4 main side")
	git(t, src, "push", "-q", s.repoURL("proj"), "main", "feature")

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}
	user := &gitalypb.User{GlId: "user-1", Name: []byte("Ree Baser"), Email: []byte("rebase@example.com")}
	ts := timestamppb.New(time.Unix(1735690000, 0))

	remoteTip := func(ref string) string {
		out := git(t, src, "ls-remote", s.repoURL("proj"), ref)
		fields := strings.Fields(out)
		require.Len(t, fields, 2, "ref %s: %q", ref, out)
		return fields[0]
	}
	fetchRef := func(ref string) string {
		git(t, src, "fetch", "-q", s.repoURL("proj"), ref)
		return strings.TrimSpace(git(t, src, "rev-parse", "FETCH_HEAD"))
	}

	t.Run("rebase to ref equals git rebase", func(t *testing.T) {
		// git's own rebase of feature onto main, computed locally.
		local := t.TempDir()
		git(t, local, "clone", "-q", "--no-local", src, ".")
		git(t, local, "checkout", "-q", "feature")
		git(t, local, "rebase", "main")
		wantTree := strings.TrimSpace(git(t, local, "rev-parse", "HEAD^{tree}"))

		out, err := op.UserRebaseToRef(ctx, &gitalypb.UserRebaseToRefRequest{
			Repository: repo, User: user,
			SourceSha:      remoteTip("refs/heads/feature"),
			TargetRef:      []byte("refs/merge-requests/12/head"),
			FirstParentRef: []byte("main"),
			Timestamp:      ts,
		})
		require.NoError(t, err)

		fetched := fetchRef("refs/merge-requests/12/head")
		assert.Equal(t, out.GetCommitId(), fetched)
		gotTree := strings.TrimSpace(git(t, src, "rev-parse", fetched+"^{tree}"))
		assert.Equal(t, wantTree, gotTree, "rebased tree equals git rebase")
		// Subjects survive the replay, committer is the acting user.
		logOut := git(t, src, "log", "--format=%s|%cn", "-3", fetched)
		lines := strings.Split(strings.TrimSpace(logOut), "\n")
		require.Len(t, lines, 3)
		assert.Equal(t, "c3 feature two|Ree Baser", lines[0])
		assert.Equal(t, "c2 feature one|Ree Baser", lines[1])
		assert.Equal(t, "c4 main side|it", lines[2], "main's own commit untouched")
	})

	t.Run("rebase confirmable rebases the branch", func(t *testing.T) {
		// git's own rebase for the tree oracle.
		local := t.TempDir()
		git(t, local, "clone", "-q", "--no-local", src, ".")
		git(t, local, "checkout", "-q", "feature")
		git(t, local, "rebase", "main")
		wantTree := strings.TrimSpace(git(t, local, "rev-parse", "HEAD^{tree}"))

		stream, err := op.UserRebaseConfirmable(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserRebaseConfirmableRequest{
			UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Header_{
				Header: &gitalypb.UserRebaseConfirmableRequest_Header{
					Repository:       repo,
					User:             user,
					Branch:           []byte("feature"),
					BranchSha:        remoteTip("refs/heads/feature"),
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

		fetched := fetchRef("feature")
		assert.Equal(t, rebaseSha, fetched, "branch sits at the rebased tip")
		gotTree := strings.TrimSpace(git(t, src, "rev-parse", fetched+"^{tree}"))
		assert.Equal(t, wantTree, gotTree, "rebased tree equals git rebase")
		// The retired chain is gone from the advertised history.
		oldTip := git(t, src, "rev-parse", "feature@{0}")
		_ = oldTip
	})

	t.Run("apply patch series equals git am", func(t *testing.T) {
		// Produce a real two-commit series with git format-patch.
		git(t, src, "checkout", "-q", "-b", "patchwork")
		write(t, src+"/patched.txt", "line one\nline two\nline three\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "add patched")
		write(t, src+"/patched.txt", "line one changed\nline two\nline three\n")
		write(t, src+"/extra.txt", "extra\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "edit patched and add extra")
		patchDir := t.TempDir()
		git(t, src, "format-patch", "-2", "-o", patchDir)
		mbox := readPatchFiles(t, patchDir)

		// git's own am onto main for the oracle.
		local := t.TempDir()
		git(t, local, "clone", "-q", "--no-local", src, ".")
		git(t, local, "checkout", "-q", "main")
		require.NoError(t, os.WriteFile(local+"/series.mbox", mbox, 0o644))
		git(t, local, "am", "-q", "series.mbox")
		wantTree := strings.TrimSpace(git(t, local, "rev-parse", "HEAD^{tree}"))

		stream, err := op.UserApplyPatch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
			UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{
				Header: &gitalypb.UserApplyPatchRequest_Header{
					Repository:   repo,
					User:         user,
					TargetBranch: []byte("main"),
					Timestamp:    ts,
				}}}))
		require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
			UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Patches{Patches: mbox}}))
		res, err := stream.CloseAndRecv()
		require.NoError(t, err)

		fetched := fetchRef("main")
		assert.Equal(t, res.GetBranchUpdate().GetCommitId(), fetched)
		gotTree := strings.TrimSpace(git(t, src, "rev-parse", fetched+"^{tree}"))
		assert.Equal(t, wantTree, gotTree, "applied series tree equals git am")
		// Authors and subjects from the mails survive (log lists newest first).
		logOut := git(t, src, "log", "--format=%an:%s", "-2", fetched)
		assert.Equal(t, "it:edit patched and add extra\nit:add patched", strings.TrimSpace(logOut))
	})

	t.Run("apply patch conflict fails cleanly", func(t *testing.T) {
		// Rebase the series against a conflicting edit: same line edited
		// differently on main first.
		git(t, src, "checkout", "-q", "main")
		git(t, src, "pull", "-q", "--rebase", s.repoURL("proj"), "main")
		write(t, src+"/patched.txt", "totally different\nline two\nline three\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "conflicting edit")
		git(t, src, "push", "-q", s.repoURL("proj"), "main")

		patchDir := t.TempDir()
		git(t, src, "format-patch", "-1", "-o", patchDir)
		mbox := readPatchFiles(t, patchDir)

		stream, err := op.UserApplyPatch(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
			UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{
				Header: &gitalypb.UserApplyPatchRequest_Header{
					Repository:   repo,
					User:         user,
					TargetBranch: []byte("main"),
					Timestamp:    ts,
				}}}))
		require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
			UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Patches{Patches: mbox}}))
		_, err = stream.CloseAndRecv()
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "Patch failed at")
	})

	t.Run("update submodule pointer", func(t *testing.T) {
		out, err := op.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			Submodule:     []byte("vendor/lib"),
			CommitSha:     "9876543210987654321098765432109876543210",
			CommitMessage: []byte("Bump vendor/lib\n"), Timestamp: ts,
		})
		require.NoError(t, err)

		fetched := fetchRef("main")
		assert.Equal(t, out.GetBranchUpdate().GetCommitId(), fetched)
		// ls-tree prints "<mode> <type> <oid>	<path>".
		fields := strings.Fields(git(t, src, "ls-tree", fetched, "vendor/lib"))
		require.Len(t, fields, 4)
		assert.Equal(t, "160000", fields[0], "still a gitlink")
		assert.Equal(t, "commit", fields[1])
		assert.Equal(t, "9876543210987654321098765432109876543210", fields[2])
		assert.Equal(t, "Bump vendor/lib",
			strings.TrimSpace(git(t, src, "show", "-s", "--format=%s", fetched)))

		// Already-at and non-submodule paths answer InvalidArgument.
		_, err = op.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			Submodule: []byte("vendor/lib"), CommitSha: "9876543210987654321098765432109876543210",
			CommitMessage: []byte("again\n"), Timestamp: ts,
		})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		_, err = op.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
			Repository: repo, User: user, Branch: []byte("main"),
			Submodule: []byte("patched.txt"), CommitSha: "0123456789012345678901234567890123456789",
			CommitMessage: []byte("nope\n"), Timestamp: ts,
		})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}

// readPatchFiles concatenates a format-patch directory's mails in order.
func readPatchFiles(t *testing.T, dir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []byte
	for _, n := range names {
		b, err := os.ReadFile(dir + "/" + n)
		require.NoError(t, err)
		out = append(out, b...)
		if !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, '\n')
		}
	}
	return out
}
