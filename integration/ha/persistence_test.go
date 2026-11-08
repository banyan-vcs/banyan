// Goal: SQLite metadata persistence — a full server restart keeps repos,
// packs, object index, and timeline heads (branch ref-map incl. subtree
// timelines) across processes (T17).
package ha

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// --- T17 SQLite metadata persistence: full server restart keeps all state ---
// The M2 headline: with -meta sqlite, repos, packs, object index, and timeline
// heads (branch ref-map incl. subtree timelines) live in one SQLite file, and
// the shard root on disk — a brand-new process serves clones immediately.
func TestSQLiteMetaRestartPersistence(t *testing.T) {
	support.NeedGit(t)
	dir := t.TempDir()
	shardRoot := filepath.Join(dir, "shards")
	dbPath := filepath.Join(dir, "meta.db")

	newSrv := func(t *testing.T) *support.Server {
		meta, err := meta.NewSQLiteMetaStore(dbPath)
		require.NoError(t, err, "open sqlite meta")
		t.Cleanup(func() { _ = meta.Close() })
		local := storage.NewLocalShardStore(shardRoot)
		nodes := []storage.Node{{ID: "t1", Local: true}}
		objects := storage.NewObjectService(meta, local, nodes, model.SHA1)
		mux := http.NewServeMux()
		gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
		rpc.NewServer(meta, objects, model.SHA1).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return &support.Server{Server: srv}
	}

	// Instance 1: seed + advance root + a subtree adoption push.
	s1 := newSrv(t)
	src := support.SeedRepo(t, s1, "t17")
	support.Write(t, filepath.Join(src, "README.md"), "readme-v2\n")
	support.Git(t, src, "commit", "-qam", "t17: c2")
	support.Git(t, src, "push", "-q", s1.RepoURL("t17"), "main")

	sub := support.CloneRepo(t, s1, s1.TreeURL("t17", "lib/nested"))
	support.Write(t, filepath.Join(sub, "SHARED.txt"), "base\nfrom-sub\n")
	support.Git(t, sub, "commit", "-qam", "t17: sub change")
	support.Git(t, sub, "push", "-q", "origin", "main")
	c2Head := support.HeadOf(t, src)
	subHead := support.HeadOf(t, sub)

	// Full restart: nothing survives but the SQLite file and the shard root.
	s1.Close()

	s2 := newSrv(t)
	root := support.CloneRepo(t, s2, s2.RepoURL("t17"))
	require.Equal(t, "readme-v2\n", support.ReadFile(t, filepath.Join(root, "README.md")), "post-restart root README")
	require.True(t, support.HasCommit(t, root, c2Head), "post-restart root missing c2")
	require.Equal(t, "base\nfrom-sub\n", support.ReadFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")),
		"post-restart root missing subtree adoption")
	// Subtree view + its timeline survive too.
	sub2 := support.CloneRepo(t, s2, s2.TreeURL("t17", "lib/nested"))
	assert.Equal(t, "base\nfrom-sub\n", support.ReadFile(t, filepath.Join(sub2, "SHARED.txt")), "post-restart subtree view")
	assert.True(t, support.HasCommit(t, sub2, subHead), "post-restart subtree history missing the pushed commit")
	// And the server keeps accepting new pushes after restart (src must
	// rebase first: the adoption merge advanced root main past src's c2).
	support.Git(t, src, "pull", "--rebase", s2.RepoURL("t17"), "main")
	support.Write(t, filepath.Join(src, "README.md"), "readme-v3\n")
	support.Git(t, src, "commit", "-qam", "t17: post-restart")
	support.Git(t, src, "push", "-q", s2.RepoURL("t17"), "main")
	root3 := support.CloneRepo(t, s2, s2.RepoURL("t17"))
	require.Equal(t, "readme-v3\n", support.ReadFile(t, filepath.Join(root3, "README.md")), "post-restart push lost")
	support.FsckClean(t, root3)
	support.FsckClean(t, sub2)
}
