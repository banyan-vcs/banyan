// Goal: SQLite metadata persistence — a full server restart keeps repos,
// packs, object index, and timeline heads (branch ref-map incl. subtree
// timelines) across processes (T17).
package integration

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// --- T17 SQLite metadata persistence: full server restart keeps all state ---
// The M2 headline: with -meta sqlite, repos, packs, object index, and timeline
// heads (branch ref-map incl. subtree timelines) live in one SQLite file, and
// the shard root on disk — a brand-new process serves clones immediately.
func TestSQLiteMetaRestartPersistence(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	shardRoot := filepath.Join(dir, "shards")
	dbPath := filepath.Join(dir, "meta.db")

	newSrv := func(t *testing.T) *server {
		meta, err := storage.NewSQLiteMetaStore(dbPath)
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
		return &server{Server: srv}
	}

	// Instance 1: seed + advance root + a subtree adoption push.
	s1 := newSrv(t)
	src := seedRepo(t, s1, "t17")
	write(t, filepath.Join(src, "README.md"), "readme-v2\n")
	git(t, src, "commit", "-qam", "t17: c2")
	git(t, src, "push", "-q", s1.repoURL("t17"), "main")

	sub := cloneRepo(t, s1, s1.treeURL("t17", "lib/nested"))
	write(t, filepath.Join(sub, "SHARED.txt"), "base\nfrom-sub\n")
	git(t, sub, "commit", "-qam", "t17: sub change")
	git(t, sub, "push", "-q", "origin", "main")
	c2Head := headOf(t, src)
	subHead := headOf(t, sub)

	// Full restart: nothing survives but the SQLite file and the shard root.
	s1.Close()

	s2 := newSrv(t)
	root := cloneRepo(t, s2, s2.repoURL("t17"))
	require.Equal(t, "readme-v2\n", readFile(t, filepath.Join(root, "README.md")), "post-restart root README")
	require.True(t, hasCommit(t, root, c2Head), "post-restart root missing c2")
	require.Equal(t, "base\nfrom-sub\n", readFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")),
		"post-restart root missing subtree adoption")
	// Subtree view + its timeline survive too.
	sub2 := cloneRepo(t, s2, s2.treeURL("t17", "lib/nested"))
	assert.Equal(t, "base\nfrom-sub\n", readFile(t, filepath.Join(sub2, "SHARED.txt")), "post-restart subtree view")
	assert.True(t, hasCommit(t, sub2, subHead), "post-restart subtree history missing the pushed commit")
	// And the server keeps accepting new pushes after restart (src must
	// rebase first: the adoption merge advanced root main past src's c2).
	git(t, src, "pull", "--rebase", s2.repoURL("t17"), "main")
	write(t, filepath.Join(src, "README.md"), "readme-v3\n")
	git(t, src, "commit", "-qam", "t17: post-restart")
	git(t, src, "push", "-q", s2.repoURL("t17"), "main")
	root3 := cloneRepo(t, s2, s2.repoURL("t17"))
	require.Equal(t, "readme-v3\n", readFile(t, filepath.Join(root3, "README.md")), "post-restart push lost")
	fsckClean(t, root3)
	fsckClean(t, sub2)
}
