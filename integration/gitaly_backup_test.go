// T72 — server-side backups (docs/19). BackupRepository writes a bundle, a
// refs file and the custom-hooks tarball into the sink (upstream's key
// layout on a local directory); RestoreRepository rebuilds a removed repo
// from those bytes — the oracle is a git clone of the restored repo
// matching the original. Incremental runs stack a second step whose bundle
// carries only the new objects; the BackupService ID registry orders runs
// by marker modification time.
package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT72GitalyBackupSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	sink := t.TempDir()
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{
		Token: "t72", BackupDir: sink})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t72")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	rs := gitalypb.NewRepositoryServiceClient(conn)
	bs := gitalypb.NewBackupServiceClient(conn)

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "t72"}
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/readme.md", "v1\n")
	git(t, src, "add", "-A")
	gitDated(t, src, 1700000000, "commit", "-qm", "c1")
	write(t, src+"/lib.go", "package lib\n")
	git(t, src, "add", "-A")
	gitDated(t, src, 1700000100, "commit", "-qm", "c2")
	git(t, src, "tag", "-a", "v1.0", "-m", "release")
	git(t, src, "push", "-q", s.repoURL("t72"), "main", "--tags")
	c2 := strings.Fields(git(t, src, "rev-parse", "main"))[0]

	// A custom-hooks tarball rides along (the repo-blob plane).
	tarball := []byte("custom_hooks/\x00pre-receive\x00payload\x00")
	setStream, err := rs.SetCustomHooks(ctx)
	require.NoError(t, err)
	require.NoError(t, setStream.Send(&gitalypb.SetCustomHooksRequest{Repository: repo}))
	require.NoError(t, setStream.Send(&gitalypb.SetCustomHooksRequest{Data: tarball}))
	_, err = setStream.CloseAndRecv()
	require.NoError(t, err)

	_, err = rs.BackupRepository(ctx, &gitalypb.BackupRepositoryRequest{
		Repository: repo, BackupId: "run-1"})
	require.NoError(t, err)

	// Sink layout mirrors upstream's keys.
	for _, rel := range []string{
		"./default/t72/run-1/001.bundle",
		"./default/t72/run-1/001.refs",
		"./default/t72/run-1/001.custom_hooks.tar",
		"manifests/default/t72/run-1.toml",
	} {
		_, statErr := os.Stat(filepath.Join(sink, filepath.FromSlash(rel)))
		assert.NoError(t, statErr, "sink key %s", rel)
	}
	refsFile, err := os.ReadFile(filepath.Join(sink, "default", "t72", "run-1", "001.refs"))
	require.NoError(t, err)
	refText := string(refsFile)
	assert.Contains(t, refText, c2+" refs/heads/main")
	assert.Contains(t, refText, " refs/tags/v1.0")

	// --- full restore of a removed repository, oracle = git clone ---
	_, err = rs.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{Repository: repo})
	require.NoError(t, err)
	_, err = rs.RestoreRepository(ctx, &gitalypb.RestoreRepositoryRequest{
		Repository: repo, BackupId: "run-1"})
	require.NoError(t, err)

	work := t.TempDir()
	git(t, work, "clone", "-q", s.repoURL("t72"), "restored")
	restored := work + "/restored"
	assert.Equal(t, c2, strings.Fields(git(t, restored, "rev-parse", "main"))[0])
	assert.Equal(t, "v1\n", readRepoFile(t, restored, "readme.md"))
	assert.Equal(t, "package lib\n", readRepoFile(t, restored, "lib.go"))
	assert.Equal(t, "c1", strings.TrimSpace(git(t, restored, "log", "--format=%s", "main~1")))
	assert.Equal(t, "release", strings.TrimSpace(git(t, restored, "tag", "-l", "--format=%(contents:subject)", "v1.0")))

	hooks, err := rs.GetCustomHooks(ctx, &gitalypb.GetCustomHooksRequest{Repository: repo})
	require.NoError(t, err)
	var gotHooks []byte
	for {
		resp, rerr := hooks.Recv()
		if rerr != nil {
			break // EOF closes the stream
		}
		gotHooks = append(gotHooks, resp.GetData()...)
	}
	assert.Equal(t, tarball, gotHooks, "custom hooks restore from the sink step")

	// --- incremental run: only the new commit rides the second bundle ---
	write(t, src+"/extra.txt", "increment\n")
	git(t, src, "add", "-A")
	gitDated(t, src, 1700000200, "commit", "-qm", "c3")
	git(t, src, "push", "-q", s.repoURL("t72"), "main")
	c3 := strings.Fields(git(t, src, "rev-parse", "main"))[0]

	_, err = rs.BackupRepository(ctx, &gitalypb.BackupRepositoryRequest{
		Repository: repo, BackupId: "run-2", Incremental: true})
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(sink, "default", "t72", "run-2", "002.bundle"))
	assert.NoError(t, statErr, "increment lands as step 002")
	manifest2, err := os.ReadFile(filepath.Join(sink, "manifests", "default", "t72", "run-2.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(manifest2), "[[steps]]", "run-2 manifest stacks both steps")
	assert.Contains(t, string(manifest2), "previous_ref_path")

	_, err = rs.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{Repository: repo})
	require.NoError(t, err)
	_, err = rs.RestoreRepository(ctx, &gitalypb.RestoreRepositoryRequest{
		Repository: repo, BackupId: "run-2"})
	require.NoError(t, err)
	git2 := t.TempDir()
	git(t, git2, "clone", "-q", s.repoURL("t72"), "inc")
	assert.Equal(t, c3, strings.Fields(git(t, git2+"/inc", "rev-parse", "main"))[0],
		"incremental restore replays every step in order")

	// --- backup ID registry ---
	_, err = bs.WriteBackupID(ctx, &gitalypb.WriteBackupIDRequest{StorageName: "default", BackupId: "run-1"})
	require.NoError(t, err)
	_, err = bs.WriteBackupID(ctx, &gitalypb.WriteBackupIDRequest{StorageName: "default", BackupId: "run-2"})
	require.NoError(t, err)
	latest, err := bs.ReadLatestBackupID(ctx, &gitalypb.ReadLatestBackupIDRequest{StorageName: "default"})
	require.NoError(t, err)
	assert.Equal(t, "run-2", latest.GetBackupId())
	_, err = bs.WriteBackupID(ctx, &gitalypb.WriteBackupIDRequest{StorageName: "default", BackupId: ""})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// --- an unconfigured sink refuses exactly like upstream ---
	gs2 := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t72b"})
	lis2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs2.Serve(lis2) }()
	t.Cleanup(gs2.Stop)
	conn2, err := gitaly.Dial(lis2.Addr().String(), "t72b")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn2.Close() })
	rs2 := gitalypb.NewRepositoryServiceClient(conn2)
	bs2 := gitalypb.NewBackupServiceClient(conn2)
	_, err = rs2.BackupRepository(ctx, &gitalypb.BackupRepositoryRequest{
		Repository: repo, BackupId: "x"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = bs2.ReadLatestBackupID(ctx, &gitalypb.ReadLatestBackupIDRequest{})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}
