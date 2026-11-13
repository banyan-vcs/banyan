// Goal: auxiliary ref namespaces (tags and any refs/<ns>/<name>) end-to-end,
// and the repack mechanism — fragment merge with delta encoding, object
// preservation, byte shrink, and restart survival (T20/T21, docs/gateway/git-compat.md §6,
// docs/storage/storage.md §5, docs/storage/indexing-packing.md §3.2).
package ha

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
)

// --- T20 tags and other ref namespaces ---
// Annotated tags point at tag objects (closure must serve the tag object and
// walk into its target), lightweight tags at commits; any other refs/<ns>/
// <name> is stored verbatim. All are advertised on the root repo only.
func TestTagsAndAuxRefs(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t20")

	support.Git(t, src, "tag", "-a", "v1.0", "-m", "first release")
	support.Git(t, src, "tag", "light")
	support.Git(t, src, "push", "-q", s.RepoURL("t20"), "main", "--tags")
	out := support.Git(t, "", "ls-remote", s.RepoURL("t20"))
	require.Contains(t, out, "refs/tags/v1.0", "annotated tag 缺失")
	require.Contains(t, out, "refs/tags/light", "lightweight tag 缺失")

	// A fresh clone carries the tags; content and fsck prove the tag object
	// reached the client (annotated tags dereference through cat-file -p).
	dst := support.CloneRepo(t, s, s.RepoURL("t20"))
	tags := support.Git(t, dst, "tag", "-l")
	assert.Contains(t, tags, "v1.0")
	assert.Contains(t, tags, "light")
	support.Git(t, dst, "checkout", "-q", "v1.0")
	assert.Equal(t, "base\n", support.ReadFile(t, filepath.Join(dst, "lib", "nested", "SHARED.txt")))
	tagged := strings.TrimSpace(support.Git(t, dst, "rev-parse", "v1.0^{commit}"))
	assert.Equal(t, support.HeadOf(t, src), tagged, "v1.0 应指向推送时主分支提交")
	assert.NotEmpty(t, strings.TrimSpace(support.Git(t, dst, "cat-file", "-p", "v1.0")), "tag 对象体")
	support.FsckClean(t, dst)

	// Shallow clone of a tag want peels to the commit.
	d1 := t.TempDir()
	support.Git(t, "", "clone", "-q", "--depth", "1", "--branch", "v1.0", s.RepoURL("t20"), d1)
	assert.Equal(t, 1, support.CountCommits(t, d1), "tag depth1 提交数")

	// Custom namespace: push refs/notes/x; visible on ls-remote, fetchable
	// by explicit refspec (git clone only auto-follows heads + tags).
	support.Git(t, src, "update-ref", "refs/notes/x", support.HeadOf(t, src))
	support.Git(t, src, "push", "-q", s.RepoURL("t20"), "refs/notes/x:refs/notes/x")
	out = support.Git(t, "", "ls-remote", s.RepoURL("t20"))
	require.Contains(t, out, "refs/notes/x", "自定义命名空间 ref 缺失")
	other := support.CloneRepo(t, s, s.RepoURL("t20"))
	support.Git(t, other, "fetch", "-q", s.RepoURL("t20"), "refs/notes/x:refs/notes/x")
	assert.Equal(t, support.HeadOf(t, src), strings.TrimSpace(support.Git(t, other, "rev-parse", "refs/notes/x")))

	// Tag deletion via :refs/tags/<name>.
	support.Git(t, src, "push", "-q", s.RepoURL("t20"), ":refs/tags/light")
	out = support.Git(t, "", "ls-remote", s.RepoURL("t20"))
	assert.NotContains(t, out, "refs/tags/light", "删除后 tag 仍在广告")
	assert.Contains(t, out, "refs/tags/v1.0", "其余 tag 不受影响")

	// Tag updates are guarded by the CAS old-value (non-FF without force rejected).
	support.Git(t, src, "commit", "-q", "--allow-empty", "-m", "t20: advance")
	support.Git(t, src, "push", "-q", s.RepoURL("t20"), "main")
	support.Git(t, src, "tag", "-a", "v1.0", "-m", "moved", "--force")
	outTry, err := support.GitTry(src, "push", s.RepoURL("t20"), "v1.0")
	require.Error(t, err, "陈旧 tag 更新未被拒:\n%s", outTry)
	require.Contains(t, outTry, "rejected")
}


// --- T21 repack: fragment merge + delta + object preservation + restart ---
func TestRepackFragmentMerge(t *testing.T) {
	support.NeedGit(t)
	dir := t.TempDir()
	s, objects := support.NewStack(t, filepath.Join(dir, "shards"), "", false)
	src := support.SeedRepo(t, s, "t21")

	// Fragmentation: several pushes + a subtree push (each weave
	// materialization persists single-object synthetics) leave many packs.
	// big.txt versions are deliberately large: the delta-encoding win the
	// byte assertion pins must dwarf one stray ~74B single-object synthetic
	// pack, not race it.
	for i := 1; i <= 3; i++ {
		support.AppendLine(t, filepath.Join(src, "big.txt"), strings.Repeat("version line for delta compression ", 160)+strings.Repeat("x", i)+"\n")
		support.Git(t, src, "add", "-A")
		support.Git(t, src, "commit", "-qm", "t21: bulk "+string(rune('a'+i)))
		support.Git(t, src, "push", "-q", s.RepoURL("t21"), "main")
	}
	sub := support.CloneRepo(t, s, s.TreeURL("t21", "lib/nested"))
	support.AppendLine(t, filepath.Join(sub, "SHARED.txt"), "repack-sub\n")
	support.Git(t, sub, "commit", "-qam", "t21: sub")
	support.Git(t, sub, "push", "-q", "origin", "main")

	ctx := context.Background()
	unionObjs := func(packs []meta.PackInfo) map[model.OID]bool {
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
	// Quiesce before snapshotting: synthetic objects (weave materialization,
	// index renders) appear lazily after the pushes, and under CPU
	// starvation (CI runners run few vCPUs) they can straddle the snapshot
	// window. Poll until packs and objects stop changing, then freeze the
	// before-universe both assertions compare against.
	var before []meta.PackInfo
	var beforeUnion map[model.OID]bool
	for stable, i := 0, 0; i < 80 && stable < 2; i++ {
		packs, err := objects.Meta.ListPacks(ctx, "t21")
		require.NoError(t, err)
		union := unionObjs(packs)
		if beforeUnion != nil && len(union) == len(beforeUnion) && len(packs) == len(before) {
			stable++
		} else {
			stable = 0
		}
		before, beforeUnion = packs, union
		time.Sleep(25 * time.Millisecond)
	}
	require.Greater(t, len(before), 3, "预备条件：应已碎片化")
	bytesBeforePre := 0
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
	// the violating run — every object from the before-union must survive.
	// Superset is legal: the lightweight-commit index renders synthetic
	// commit objects asynchronously, and under CPU starvation (CI runners)
	// renders can land between the two snapshots — new content, not a
	// repack defect (that would be a MISSING object, which this catches).
	afterUnion := unionObjs(after)
	for o := range beforeUnion {
		assert.True(t, afterUnion[o], "repack 不得丢失对象 %s（并集口径）", o)
	}

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
	fresh := support.CloneRepo(t, s, s.RepoURL("t21"))
	require.True(t, support.HasCommit(t, fresh, support.HeadOf(t, src)))
	assert.Contains(t, support.ReadFile(t, filepath.Join(fresh, "lib", "nested", "SHARED.txt")), "repack-sub")
	support.FsckClean(t, fresh)
	support.Git(t, fresh, "cat-file", "-p", "HEAD^{tree}") // sanity: trees resolvable

	// Below-threshold is a no-op. (The fresh clone above re-fragmented a
	// little — advertisement materializes the weave and persists synthetics
	// — which is exactly why repack runs periodically above a threshold.)
	res, err := objects.Repack(ctx, "t21", 64, 0)
	require.NoError(t, err)
	assert.Equal(t, res.PacksBefore, res.PacksAfter, "阈值之下应为 no-op")
}

// --- T21b repack restart survival: SQLite meta + shards on disk ---
func TestRepackRestartSurvival(t *testing.T) {
	support.NeedGit(t)
	dir := t.TempDir()
	shardRoot := filepath.Join(dir, "shards")
	dbPath := filepath.Join(dir, "meta.db")

	s1, objects1 := support.NewStack(t, shardRoot, dbPath, true)
	src := support.SeedRepo(t, s1, "t21b")
	for i := 1; i <= 3; i++ {
		support.AppendLine(t, filepath.Join(src, "f.txt"), "same-prefix-line "+strings.Repeat("z", 40)+string(rune('a'+i))+"\n")
		support.Git(t, src, "add", "-A")
		support.Git(t, src, "commit", "-qm", "t21b: advance")
		support.Git(t, src, "push", "-q", s1.RepoURL("t21b"), "main")
	}
	tip := support.HeadOf(t, src)

	ctx := context.Background()
	_, err := objects1.Repack(ctx, "t21b", 2, 0)
	require.NoError(t, err)
	packsBeforeRestart, err := objects1.Meta.ListPacks(ctx, "t21b")
	require.NoError(t, err)
	s1.Close()

	// Brand-new process state (fresh ObjectService, cold caches) on the same
	// SQLite file + shard root: clones must work and be complete.
	s2, objects2 := support.NewStack(t, shardRoot, dbPath, true)
	fresh := support.CloneRepo(t, s2, s2.RepoURL("t21b"))
	require.True(t, support.HasCommit(t, fresh, tip), "重启后缺提交")
	support.FsckClean(t, fresh)
	packs, err := objects2.Meta.ListPacks(ctx, "t21b")
	require.NoError(t, err)
	assert.Equal(t, len(packsBeforeRestart), len(packs), "重启不应改变 pack 布局（几何合并产物持久）")
}

// --- T31 synth batching: one weave materialization = one pack (docs/operations/performance.md P0-2) ---
// Before batching, every synthetic (projection commits, fold rewrites) landed
// as its own single-object pack — one weave of N objects meant N packs, and in
// cluster mode N×(R+1) network operations. The view entries now run inside a
// synth batch whose pre-CAS flush persists everything as ONE pack (the
// durability point that keeps "registration is the fact" intact).
func TestSynthBatchSinglePack(t *testing.T) {
	support.NeedGit(t)
	dir := t.TempDir()
	s, objects := support.NewStack(t, filepath.Join(dir, "shards"), "", false)
	src := support.SeedRepo(t, s, "t31")
	// A second commit grows the projection chain the subtree view materializes.
	support.AppendLine(t, filepath.Join(src, "README.md"), "t31: more\n")
	support.Git(t, src, "commit", "-qam", "t31: second")
	support.Git(t, src, "push", "-q", s.RepoURL("t31"), "main")

	ctx := context.Background()
	before, err := objects.Meta.ListPacks(ctx, "t31")
	require.NoError(t, err)

	// First advertisement of the subtree view materializes projection + fold
	// synthetics — all inside one batch.
	sub := support.CloneRepo(t, s, s.TreeURL("t31", "lib/nested"))
	after, err := objects.Meta.ListPacks(ctx, "t31")
	require.NoError(t, err)
	assert.Equal(t, 1, len(after)-len(before),
		"一次子树物化应只产生一个合成 pack（实际 %d → %d）", len(before), len(after))

	assert.Equal(t, "base\n", support.ReadFile(t, filepath.Join(sub, "SHARED.txt")))
	support.FsckClean(t, sub)
	// The root view stays consistent with the materialized objects.
	fresh := support.CloneRepo(t, s, s.RepoURL("t31"))
	support.FsckClean(t, fresh)
	require.True(t, support.HasCommit(t, fresh, support.HeadOf(t, src)))
}
