// weave_consistency_test.go — T78: the cluster-correctness invariant
// docs/02 §13 leans on — weave materialization is a pure function of the
// timeline tips, so a second gateway with fully cold in-process state
// (fresh ObjectService: synth table, decoded-pack memo, location cache; and
// a fresh view.Service rootCache) must derive byte-identical woven views
// over the same storage: same advertised OIDs, same served content.
//
// The sharp edge is timestamp ties: every commit in the fixture shares ONE
// committer second, so any wall-clock input into synthesis — or an unstable
// tie-break in the (when, anchor, oid) ordering — would fork the synthesized
// chain OIDs between gateways (the failure mode: gateway A advertises a tip
// whose closure gateway B never derives). The ResetWoven rebuild path is
// exercised too: with graft provenance (banyan-woven-from) the rebuild
// recovers each source's fold progress from the root-local spine and stays
// byte-stable — subjects once, re-advertised tip identical.
package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestWeaveRederivationConsistency(t *testing.T) {
	needGit(t)
	const tiedUnix = int64(1762343471) // 2026-11-05T11:11:11Z — every commit in one second

	// Gateway A's plane, inline: the handles (ms, shard store, gitwire
	// server) stay reachable so gateway B can share the durable state.
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(tempDirQuiet(t))
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objectsA := storage.NewObjectService(ms, local, nodes, model.SHA1)
	muxA := http.NewServeMux()
	gwA := gitwire.NewServer(ms, objectsA, model.SHA1)
	gwA.Register(muxA)
	srvA := httptest.NewServer(muxA)
	t.Cleanup(srvA.Close)
	sA := &server{Server: srvA}

	// Gateway B: cold everything — it may only read persisted bytes and
	// re-derive. Any clock input in synthesis shows up here as an OID fork.
	objectsB := storage.NewObjectService(ms, local, nodes, model.SHA1)
	muxB := http.NewServeMux()
	gitwire.NewServer(ms, objectsB, model.SHA1).Register(muxB)
	srvB := httptest.NewServer(muxB)
	t.Cleanup(srvB.Close)
	sB := &server{Server: srvB}

	// Fixture: activity on two subtree timelines interleaved with
	// root-local commits, every commit tied to one committer second.
	// Root-side edits stay outside lib so subtree pushes never need pull
	// (the docs/02 §12 non-interference contract keeps the sequence push-clean).
	src := t.TempDir()
	gitDated(t, src, tiedUnix, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "README.md"), "wv-1\n")
	write(t, filepath.Join(src, "lib", "a.txt"), "a-1\n")
	gitDated(t, src, tiedUnix, "add", "-A")
	gitDated(t, src, tiedUnix, "commit", "-qm", "wv: seed")
	gitDated(t, src, tiedUnix, "push", "-q", sA.repoURL("ww"), "main")

	sub := cloneRepo(t, sA, sA.treeURL("ww", "lib"))
	write(t, filepath.Join(sub, "a.txt"), "a-2\n")
	gitDated(t, sub, tiedUnix, "add", "-A")
	gitDated(t, sub, tiedUnix, "commit", "-qm", "wv: sub 2")
	gitDated(t, sub, tiedUnix, "push", "-q", "origin", "main")

	root := cloneRepo(t, sA, sA.repoURL("ww"))
	write(t, filepath.Join(root, "top.txt"), "top-1\n")
	gitDated(t, root, tiedUnix, "add", "-A")
	gitDated(t, root, tiedUnix, "commit", "-qm", "wv: root 1")
	gitDated(t, root, tiedUnix, "push", "-q", "origin", "main")

	write(t, filepath.Join(sub, "nested", "SHARED.txt"), "wv-tie\n")
	gitDated(t, sub, tiedUnix, "add", "-A")
	gitDated(t, sub, tiedUnix, "commit", "-qm", "wv: sub 3")
	gitDated(t, sub, tiedUnix, "push", "-q", "origin", "main")

	// The subtree push moved the woven root tip — the root client pulls
	// --rebase first (the tied committer date rides along via env, keeping
	// the whole fixture in one second).
	gitDated(t, root, tiedUnix, "pull", "--rebase", "-q", "origin", "main")
	write(t, filepath.Join(root, "README.md"), "wv-2\n")
	gitDated(t, root, tiedUnix, "add", "-A")
	gitDated(t, root, tiedUnix, "commit", "-qm", "wv: root 2")
	gitDated(t, root, tiedUnix, "push", "-q", "origin", "main")

	headLine := func(s *server) string {
		t.Helper()
		return strings.TrimSpace(git(t, "", "ls-remote", s.repoURL("ww"), "refs/heads/main"))
	}

	// ① A vs B: the cold gateway must advertise the byte-identical woven tip.
	tipA := headLine(sA)
	require.NotEmpty(t, tipA, "gateway A 广告为空")
	assert.Equal(t, tipA, headLine(sB), "冷网关重推导的广告 tip 必须与 A 逐字节一致（时间戳输入或非确定平局裁决会在这里分叉 OID）")

	// ② Forced re-derivation on A: wipe the /.woven family — the next
	// advertisement rebuilds every scope from the timeline tips alone. Graft
	// provenance (banyan-woven-from) makes the rebuild recover each source's
	// fold progress from the root-local spine (clients commit on woven tips,
	// so the spine embeds the weave's own grafts): embedded sources are
	// recognized and skipped, the spine folds back byte-identically — the
	// re-advertised tip equals the pre-reset tip and every subject appears
	// exactly once. (A marker-only preservation fix was tried and REJECTED:
	// markers assume the folded prefix lives in the /.woven output chain,
	// which the reset itself erases — preserving them silently DROPS
	// untouched sources from the rebuilt view; T44: nested content lost.)
	require.NoError(t, gwA.Views.ResetWoven(context.Background(), "ww", "main"))
	assert.Equal(t, tipA, headLine(sA), "重建后广告 tip 必须与 reset 前逐字节一致（脊柱经溯源恢复折叠进度）")
	rebuilt := cloneRepo(t, sA, sA.repoURL("ww"))
	assert.Equal(t, "a-2\n", readFile(t, filepath.Join(rebuilt, "lib", "a.txt")))
	assert.Equal(t, "wv-tie\n", readFile(t, filepath.Join(rebuilt, "lib", "nested", "SHARED.txt")))
	assert.Equal(t, "top-1\n", readFile(t, filepath.Join(rebuilt, "top.txt")))
	assert.Equal(t, "wv-2\n", readFile(t, filepath.Join(rebuilt, "README.md")))
	require.Empty(t, strings.TrimSpace(git(t, rebuilt, "log", "--merges", "--format=%s")), "重建视图历史应线性无 merge")
	rebuiltSubj := map[string]int{}
	for _, line := range strings.Split(git(t, rebuilt, "log", "--format=%s"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rebuiltSubj[line]++
		}
	}
	for s, n := range rebuiltSubj {
		assert.Equal(t, 1, n, "重建视图中 subject 重复出现 %d 次: %q", n, s)
	}
	fsckClean(t, rebuilt)

	// ③ The cold gateway serves the anchored weave to a real client:
	// content intact, linear history, every subject exactly once — the
	// linear-once promise of the woven view.
	fresh := cloneRepo(t, sB, sB.repoURL("ww"))
	assert.Equal(t, "a-2\n", readFile(t, filepath.Join(fresh, "lib", "a.txt")))
	assert.Equal(t, "wv-tie\n", readFile(t, filepath.Join(fresh, "lib", "nested", "SHARED.txt")))
	assert.Equal(t, "top-1\n", readFile(t, filepath.Join(fresh, "top.txt")))
	assert.Equal(t, "wv-2\n", readFile(t, filepath.Join(fresh, "README.md")))
	require.Empty(t, strings.TrimSpace(git(t, fresh, "log", "--merges", "--format=%s")), "织入历史应线性无 merge")
	subj := map[string]int{}
	for _, line := range strings.Split(git(t, fresh, "log", "--format=%s"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			subj[line]++
		}
	}
	for s, n := range subj {
		assert.Equal(t, 1, n, "锚定织入视图 subject 重复出现 %d 次: %q", n, s)
	}
	fsckClean(t, fresh)
}
