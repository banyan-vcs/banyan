// Goal: auxiliary ref namespaces (tags and any refs/<ns>/<name>) end-to-end,
// and the repack mechanism — fragment merge with delta encoding, object
// preservation, byte shrink, and restart survival (T20/T21, docs/04 §6,
// docs/03 §5, docs/09 §3.2).
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
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// --- T20 tags and other ref namespaces ---
// Annotated tags point at tag objects (closure must serve the tag object and
// walk into its target), lightweight tags at commits; any other refs/<ns>/
// <name> is stored verbatim. All are advertised on the root repo only.
func TestTagsAndAuxRefs(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t20")

	git(t, src, "tag", "-a", "v1.0", "-m", "first release")
	git(t, src, "tag", "light")
	git(t, src, "push", "-q", s.repoURL("t20"), "main", "--tags")
	out := git(t, "", "ls-remote", s.repoURL("t20"))
	require.Contains(t, out, "refs/tags/v1.0", "annotated tag 缺失")
	require.Contains(t, out, "refs/tags/light", "lightweight tag 缺失")

	// A fresh clone carries the tags; content and fsck prove the tag object
	// reached the client (annotated tags dereference through cat-file -p).
	dst := cloneRepo(t, s, s.repoURL("t20"))
	tags := git(t, dst, "tag", "-l")
	assert.Contains(t, tags, "v1.0")
	assert.Contains(t, tags, "light")
	git(t, dst, "checkout", "-q", "v1.0")
	assert.Equal(t, "base\n", readFile(t, filepath.Join(dst, "lib", "nested", "SHARED.txt")))
	tagged := strings.TrimSpace(git(t, dst, "rev-parse", "v1.0^{commit}"))
	assert.Equal(t, headOf(t, src), tagged, "v1.0 应指向推送时主分支提交")
	assert.NotEmpty(t, strings.TrimSpace(git(t, dst, "cat-file", "-p", "v1.0")), "tag 对象体")
	fsckClean(t, dst)

	// Shallow clone of a tag want peels to the commit.
	d1 := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "1", "--branch", "v1.0", s.repoURL("t20"), d1)
	assert.Equal(t, 1, countCommits(t, d1), "tag depth1 提交数")

	// Custom namespace: push refs/notes/x; visible on ls-remote, fetchable
	// by explicit refspec (git clone only auto-follows heads + tags).
	git(t, src, "update-ref", "refs/notes/x", headOf(t, src))
	git(t, src, "push", "-q", s.repoURL("t20"), "refs/notes/x:refs/notes/x")
	out = git(t, "", "ls-remote", s.repoURL("t20"))
	require.Contains(t, out, "refs/notes/x", "自定义命名空间 ref 缺失")
	other := cloneRepo(t, s, s.repoURL("t20"))
	git(t, other, "fetch", "-q", s.repoURL("t20"), "refs/notes/x:refs/notes/x")
	assert.Equal(t, headOf(t, src), strings.TrimSpace(git(t, other, "rev-parse", "refs/notes/x")))

	// Tag deletion via :refs/tags/<name>.
	git(t, src, "push", "-q", s.repoURL("t20"), ":refs/tags/light")
	out = git(t, "", "ls-remote", s.repoURL("t20"))
	assert.NotContains(t, out, "refs/tags/light", "删除后 tag 仍在广告")
	assert.Contains(t, out, "refs/tags/v1.0", "其余 tag 不受影响")

	// Tag updates are guarded by the CAS old-value (non-FF without force rejected).
	git(t, src, "commit", "-q", "--allow-empty", "-m", "t20: advance")
	git(t, src, "push", "-q", s.repoURL("t20"), "main")
	git(t, src, "tag", "-a", "v1.0", "-m", "moved", "--force")
	outTry, err := gitTry(src, "push", s.repoURL("t20"), "v1.0")
	require.Error(t, err, "陈旧 tag 更新未被拒:\n%s", outTry)
	require.Contains(t, outTry, "rejected")
}

// newStack builds an in-process server with direct handles (T17 pattern) so
// the test can call storage-level repack and restart the process state.
func newStack(t *testing.T, shardRoot, dbPath string, sqlite bool) (*server, *storage.ObjectService) {
	t.Helper()
	var meta storage.MetaStore
	if sqlite {
		m, err := storage.NewSQLiteMetaStore(dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		meta = m
	} else {
		meta = storage.NewMemoryMetaStore()
	}
	local := storage.NewLocalShardStore(shardRoot)
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &server{Server: srv}, objects
}

// --- T21 repack: fragment merge + delta + object preservation + restart ---
func TestRepackFragmentMerge(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	s, objects := newStack(t, filepath.Join(dir, "shards"), "", false)
	src := seedRepo(t, s, "t21")

	// Fragmentation: several pushes + a subtree push (each weave
	// materialization persists single-object synthetics) leave many packs.
	for i := 1; i <= 3; i++ {
		appendLine(t, filepath.Join(src, "big.txt"), strings.Repeat("version line for delta compression ", 8)+strings.Repeat("x", i)+"\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "t21: bulk "+string(rune('a'+i)))
		git(t, src, "push", "-q", s.repoURL("t21"), "main")
	}
	sub := cloneRepo(t, s, s.treeURL("t21", "lib/nested"))
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "repack-sub\n")
	git(t, sub, "commit", "-qam", "t21: sub")
	git(t, sub, "push", "-q", "origin", "main")

	ctx := context.Background()
	before, err := objects.Meta.ListPacks(ctx, "t21")
	require.NoError(t, err)
	require.Greater(t, len(before), 3, "预备条件：应已碎片化")
	bytesBeforePre := 0
	unionObjs := func(packs []storage.PackInfo) map[model.OID]bool {
		set := map[model.OID]bool{}
		for _, p := range packs {
			objs, err := objects.Meta.PackObjects(ctx, "t21", p.PackOID)
			require.NoError(t, err)
			for _, o := range objs {
				set[o] = true
			}
		}
		return set
	}
	beforeUnion := unionObjs(before)
	for _, p := range before {
		if raw, err := objects.Local.GetPackBytes("t21", p.PackOID); err == nil {
			bytesBeforePre += len(raw)
		}
	}

	// Repack via the RPC endpoint (default threshold forced low by passing 2).
	body := strings.NewReader(`{"repo":"t21","threshold":2}`)
	resp, err := http.Post(s.URL+"/api/v1/repack", "application/json", body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	after, err := objects.Meta.ListPacks(ctx, "t21")
	require.NoError(t, err)
	assert.Less(t, len(after), len(before), "几何合并应减少 pack 数")
	// Object preservation at UNION scope: packs overlap (synthetics
	// duplicate push objects), and the geometric merge only consolidates
	// the violating run — the union across packs must be unchanged.
	assert.Equal(t, beforeUnion, unionObjs(after), "对象必须全部保全（并集口径）")

	// Byte shrink: the merged run is delta-encoded (similar consecutive
	// big.txt versions are prime delta bases), so total bytes drop.
	bytesAfterPre := 0
	for _, p := range after {
		if raw, err := objects.Local.GetPackBytes("t21", p.PackOID); err == nil {
			bytesAfterPre += len(raw)
		}
	}
	assert.Less(t, bytesAfterPre, bytesBeforePre, "delta 化后总字节应小于分片字节总和（%d → %d）", bytesBeforePre, bytesAfterPre)

	// Serving survives: fresh clone complete and fsck-clean (the merged pack
	// now also feeds ExactPackHit whole-pack direct serve).
	fresh := cloneRepo(t, s, s.repoURL("t21"))
	require.True(t, hasCommit(t, fresh, headOf(t, src)))
	assert.Contains(t, readFile(t, filepath.Join(fresh, "lib", "nested", "SHARED.txt")), "repack-sub")
	fsckClean(t, fresh)
	git(t, fresh, "cat-file", "-p", "HEAD^{tree}") // sanity: trees resolvable

	// Below-threshold is a no-op. (The fresh clone above re-fragmented a
	// little — advertisement materializes the weave and persists synthetics
	// — which is exactly why repack runs periodically above a threshold.)
	res, err := objects.Repack(ctx, "t21", 64, 0)
	require.NoError(t, err)
	assert.Equal(t, res.PacksBefore, res.PacksAfter, "阈值之下应为 no-op")
}

// --- T21b repack restart survival: SQLite meta + shards on disk ---
func TestRepackRestartSurvival(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	shardRoot := filepath.Join(dir, "shards")
	dbPath := filepath.Join(dir, "meta.db")

	s1, objects1 := newStack(t, shardRoot, dbPath, true)
	src := seedRepo(t, s1, "t21b")
	for i := 1; i <= 3; i++ {
		appendLine(t, filepath.Join(src, "f.txt"), "same-prefix-line "+strings.Repeat("z", 40)+string(rune('a'+i))+"\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "t21b: advance")
		git(t, src, "push", "-q", s1.repoURL("t21b"), "main")
	}
	tip := headOf(t, src)

	ctx := context.Background()
	_, err := objects1.Repack(ctx, "t21b", 2, 0)
	require.NoError(t, err)
	packsBeforeRestart, err := objects1.Meta.ListPacks(ctx, "t21b")
	require.NoError(t, err)
	s1.Close()

	// Brand-new process state (fresh ObjectService, cold caches) on the same
	// SQLite file + shard root: clones must work and be complete.
	s2, objects2 := newStack(t, shardRoot, dbPath, true)
	fresh := cloneRepo(t, s2, s2.repoURL("t21b"))
	require.True(t, hasCommit(t, fresh, tip), "重启后缺提交")
	fsckClean(t, fresh)
	packs, err := objects2.Meta.ListPacks(ctx, "t21b")
	require.NoError(t, err)
	assert.Equal(t, len(packsBeforeRestart), len(packs), "重启不应改变 pack 布局（几何合并产物持久）")
}

// --- T31 synth batching: one weave materialization = one pack (docs/11 P0-2) ---
// Before batching, every synthetic (projection commits, fold rewrites) landed
// as its own single-object pack — one weave of N objects meant N packs, and in
// cluster mode N×(R+1) network operations. The view entries now run inside a
// synth batch whose pre-CAS flush persists everything as ONE pack (the
// durability point that keeps "registration is the fact" intact).
func TestSynthBatchSinglePack(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	s, objects := newStack(t, filepath.Join(dir, "shards"), "", false)
	src := seedRepo(t, s, "t31")
	// A second commit grows the projection chain the subtree view materializes.
	appendLine(t, filepath.Join(src, "README.md"), "t31: more\n")
	git(t, src, "commit", "-qam", "t31: second")
	git(t, src, "push", "-q", s.repoURL("t31"), "main")

	ctx := context.Background()
	before, err := objects.Meta.ListPacks(ctx, "t31")
	require.NoError(t, err)

	// First advertisement of the subtree view materializes projection + fold
	// synthetics — all inside one batch.
	sub := cloneRepo(t, s, s.treeURL("t31", "lib/nested"))
	after, err := objects.Meta.ListPacks(ctx, "t31")
	require.NoError(t, err)
	assert.Equal(t, 1, len(after)-len(before),
		"一次子树物化应只产生一个合成 pack（实际 %d → %d）", len(before), len(after))

	assert.Equal(t, "base\n", readFile(t, filepath.Join(sub, "SHARED.txt")))
	fsckClean(t, sub)
	// The root view stays consistent with the materialized objects.
	fresh := cloneRepo(t, s, s.repoURL("t31"))
	fsckClean(t, fresh)
	require.True(t, hasCommit(t, fresh, headOf(t, src)))
}
