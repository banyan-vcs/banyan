// weave_provenance_test.go — T88: graft provenance under CHAINED adoption.
//
// T78 ② pins the spine-recovery shape: a root client adopts the woven view,
// so the root-local spine embeds the weave's grafts. This test generalizes
// the adoption one level down — the LIB client adopts the lib view (whose
// appended graft of a nested source is a scoped-weave output), commits on
// it and pushes: the lib timeline itself embeds a fold of the nested
// source. After ResetWoven, EVERY scope rebuilds from tips alone and must
// stay identity-stable: re-advertised tips byte-identical, every subject
// exactly once, in BOTH the root and the lib view. The provenance set is
// collected from skipped candidates too — the lib-chain graft output is
// itself embedded in the root spine, and its banyan-woven-from header is
// what suppresses the nested source's re-graft.
package subtree

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestWovenProvenanceChainedAdoption(t *testing.T) {
	support.NeedGit(t)
	const tiedUnix = int64(1762343471) // one committer second for the whole fixture

	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(support.TempDirQuiet(t))
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(ms, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := &support.Server{Server: srv}

	// Seed: root repo with lib/ and lib/nested/ so three timeline scopes
	// exist from the first push.
	src := t.TempDir()
	support.GitDated(t, src, tiedUnix, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "wv-prov\n")
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-1\n")
	support.Write(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "base\n")
	support.GitDated(t, src, tiedUnix, "add", "-A")
	support.GitDated(t, src, tiedUnix, "commit", "-qm", "wv: seed")
	support.GitDated(t, src, tiedUnix, "push", "-q", s.RepoURL("wp"), "main")

	// n1: the strictly-below source (lib/nested timeline).
	n := support.CloneRepo(t, s, s.TreeURL("wp", "lib/nested"))
	support.AppendLine(t, filepath.Join(n, "SHARED.txt"), "by-n\n")
	support.GitDated(t, n, tiedUnix, "add", "-A")
	support.GitDated(t, n, tiedUnix, "commit", "-qm", "wv: n adds nested")
	support.GitDated(t, n, tiedUnix, "push", "-q", "origin", "main")

	// The lib client clones AFTER n1: the lib view it gets carries the
	// scoped graft of n1. Its commit lands on that graft and pushes — the
	// lib timeline now embeds a fold of the nested source (chained adoption).
	lib := support.CloneRepo(t, s, s.TreeURL("wp", "lib"))
	assert.Equal(t, "base\nby-n\n", support.ReadFile(t, filepath.Join(lib, "nested", "SHARED.txt")),
		"lib 视图应已织入 nested 源")
	support.AppendLine(t, filepath.Join(lib, "a.txt"), "by-lib\n")
	support.GitDated(t, lib, tiedUnix, "add", "-A")
	support.GitDated(t, lib, tiedUnix, "commit", "-qm", "wv: lib edits a")
	support.GitDated(t, lib, tiedUnix, "push", "-q", "origin", "main")

	// The root client adopts the woven root view and pushes — the root-local
	// spine embeds the weave's folds of BOTH the lib chain and (through it)
	// the nested source.
	root := support.CloneRepo(t, s, s.RepoURL("wp"))
	support.AppendLine(t, filepath.Join(root, "top.txt"), "by-root\n")
	support.GitDated(t, root, tiedUnix, "add", "-A")
	support.GitDated(t, root, tiedUnix, "commit", "-qm", "wv: root top")
	support.GitDated(t, root, tiedUnix, "push", "-q", "origin", "main")

	headLine := func(repoURL string) string {
		t.Helper()
		return strings.TrimSpace(support.Git(t, "", "ls-remote", repoURL, "refs/heads/main"))
	}
	rootTip := headLine(s.RepoURL("wp"))
	libTip := headLine(s.TreeURL("wp", "lib"))
	require.NotEmpty(t, rootTip)

	// Full woven-family reset: the next advertisement of either scope
	// rebuilds from the timeline tips alone.
	require.NoError(t, gw.Views.ResetWoven(context.Background(), "wp", "main"))

	assert.Equal(t, rootTip, headLine(s.RepoURL("wp")), "重建后根视图广告 tip 必须逐字节一致")
	assert.Equal(t, libTip, headLine(s.TreeURL("wp", "lib")), "重建后 lib 视图广告 tip 必须逐字节一致")

	subjectsOnce := func(dir, view string) {
		t.Helper()
		counts := map[string]int{}
		for _, line := range strings.Split(support.Git(t, dir, "log", "--format=%s"), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				counts[line]++
			}
		}
		for subj, n := range counts {
			assert.Equal(t, 1, n, "%s 视图 subject 重复出现 %d 次: %q", view, n, subj)
		}
		require.Empty(t, strings.TrimSpace(support.Git(t, dir, "log", "--merges", "--format=%s")), "%s 视图历史应线性", view)
	}

	freshRoot := support.CloneRepo(t, s, s.RepoURL("wp"))
	assert.Equal(t, "a-1\nby-lib\n", support.ReadFile(t, filepath.Join(freshRoot, "lib", "a.txt")))
	assert.Equal(t, "base\nby-n\n", support.ReadFile(t, filepath.Join(freshRoot, "lib", "nested", "SHARED.txt")))
	assert.Equal(t, "by-root\n", support.ReadFile(t, filepath.Join(freshRoot, "top.txt")))
	subjectsOnce(freshRoot, "根")
	support.FsckClean(t, freshRoot)

	freshLib := support.CloneRepo(t, s, s.TreeURL("wp", "lib"))
	assert.Equal(t, "a-1\nby-lib\n", support.ReadFile(t, filepath.Join(freshLib, "a.txt")))
	assert.Equal(t, "base\nby-n\n", support.ReadFile(t, filepath.Join(freshLib, "nested", "SHARED.txt")))
	subjectsOnce(freshLib, "lib")
	support.FsckClean(t, freshLib)
}
