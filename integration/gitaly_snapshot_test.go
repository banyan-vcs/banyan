// T73 — snapshots, replication and bundle generation (docs/19). GetSnapshot
// answers a bare-repository TAR (HEAD, packed-refs, pack + idx) — the idx is
// written by banyan and proven correct by plain git cloning from the
// unpacked directory. CreateRepositoryFromSnapshot round-trips through an
// HTTP server; ReplicateRepository mirrors a repo onto a fresh name; the
// bundle-URI generator drops a bundle in the sink's uri/ area.
package integration

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT73GitalySnapshotSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	sink := t.TempDir()
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{
		Token: "t73", BackupDir: sink})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t73")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	rs := gitalypb.NewRepositoryServiceClient(conn)

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "t73"}
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/a.txt", "alpha\n")
	git(t, src, "add", "-A")
	gitDated(t, src, 1700000000, "commit", "-qm", "c1")
	git(t, src, "checkout", "-q", "-b", "topic")
	write(t, src+"/b.txt", "beta\n")
	git(t, src, "add", "-A")
	gitDated(t, src, 1700000100, "commit", "-qm", "c2 topic")
	git(t, src, "checkout", "-q", "main")
	git(t, src, "tag", "-a", "v1", "-m", "first")
	git(t, src, "push", "-q", s.repoURL("t73"), "main", "topic", "--tags")
	mainOID := strings.Fields(git(t, src, "rev-parse", "main"))[0]
	topicOID := strings.Fields(git(t, src, "rev-parse", "topic"))[0]

	snap, err := rs.GetSnapshot(ctx, &gitalypb.GetSnapshotRequest{Repository: repo})
	require.NoError(t, err)
	var tarBytes []byte
	for {
		resp, rerr := snap.Recv()
		if rerr != nil {
			break // EOF
		}
		tarBytes = append(tarBytes, resp.GetData()...)
	}
	require.NotEmpty(t, tarBytes)

	// Unpack the snapshot as a bare repo and let git consume it — this
	// exercises the idx (fanout, offsets, crcs, trailer) for real.
	bare := t.TempDir()
	require.NoError(t, untar(t, tarBytes, bare))
	work := t.TempDir()
	git(t, work, "clone", "-q", bare, "from-snap")
	cloned := work + "/from-snap"
	assert.Equal(t, mainOID, strings.Fields(git(t, cloned, "rev-parse", "origin/main"))[0],
		"git must resolve the advertised branch from banyan's pack+idx")
	assert.Equal(t, topicOID, strings.Fields(git(t, cloned, "rev-parse", "origin/topic"))[0])
	assert.Equal(t, "first", strings.TrimSpace(git(t, cloned, "tag", "-l", "--format=%(contents:subject)", "v1")))
	assert.Equal(t, "alpha\n", readRepoFile(t, cloned, "a.txt"))
	// fsck validates crcs and offsets across the whole pack.
	git(t, cloned, "fsck", "--strict")

	// --- CreateRepositoryFromSnapshot over HTTP ---
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer t73-token", r.Header.Get("Authorization"))
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()
	_, err = rs.CreateRepositoryFromSnapshot(ctx, &gitalypb.CreateRepositoryFromSnapshotRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "t73-from-snap"},
		HttpUrl:    srv.URL, HttpAuth: "Bearer t73-token"})
	require.NoError(t, err)
	work2 := t.TempDir()
	git(t, work2, "clone", "-q", s.repoURL("t73-from-snap"), "seeded")
	assert.Equal(t, mainOID, strings.Fields(git(t, work2+"/seeded", "rev-parse", "origin/main"))[0])
	assert.Equal(t, "alpha\n", readRepoFile(t, work2+"/seeded", "a.txt"))

	// The topic branch made it through the snapshot round trip.
	git(t, work2, "clone", "-q", "--branch", "topic", s.repoURL("t73-from-snap"), work2+"/topic")
	assert.Equal(t, "beta\n", readRepoFile(t, work2+"/topic", "b.txt"))

	// --- ReplicateRepository ---
	dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t73-replica"}
	seedRepo(t, s, "t73-replica") // destination pre-exists; replication replaces it
	_, err = rs.ReplicateRepository(ctx, &gitalypb.ReplicateRepositoryRequest{
		Repository: dest, Source: repo})
	require.NoError(t, err)
	work3 := t.TempDir()
	git(t, work3, "clone", "-q", s.repoURL("t73-replica"), "replica")
	assert.Equal(t, mainOID, strings.Fields(git(t, work3+"/replica", "rev-parse", "origin/main"))[0])
	git(t, work3, "clone", "-q", "--branch", "topic", s.repoURL("t73-replica"), work3+"/rt")
	assert.Equal(t, topicOID, strings.Fields(git(t, work3+"/rt", "rev-parse", "origin/topic"))[0])
	assert.Equal(t, "first", strings.TrimSpace(git(t, work3+"/replica", "tag", "-l", "--format=%(contents:subject)", "v1")))

	// --- GenerateBundleURI drops a bundle in the sink's uri/ area ---
	_, err = rs.GenerateBundleURI(ctx, &gitalypb.GenerateBundleURIRequest{Repository: repo})
	require.NoError(t, err)
	bundlePath := filepath.Join(sink, "default", "t73", "uri", "default.bundle")
	bundleData, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(bundleData), "# v2 git bundle"))
	// The generated bundle is a real git bundle: verify + clone from it.
	bundleDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bundleDir, "gen.bundle"), bundleData, 0o644))
	git(t, src, "bundle", "verify", bundleDir+"/gen.bundle")
}
