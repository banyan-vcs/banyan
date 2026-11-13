// Goal: subtree collaboration semantics — linear-weave adoption, consecutive
// pushes, non-interference parallelism vs in-path mutual exclusion, multi-user
// same-file collaboration, and multi-scope arrival stability (T5/T7/T8/T10/T18).
package subtree

import (
	"fmt"
	"github.com/banyan-vcs/banyan/integration/support"

	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- T5 subtree push: linear-weave root view / parent view sees it / file placement ---
// v5 (docs/design/data-model.md §13): the subtree push advances ONLY its own timeline; the root
// view weaves the commit in linearly (same subject, different SHA — allowed),
// no merge scaffolding; the parent view (lib) also sees the change via its
// scoped weave.
func TestSubtreePushAdoption(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t5")

	sub := support.CloneRepo(t, s, s.TreeURL("t5", "lib/nested"))
	support.AppendLine(t, filepath.Join(sub, "SHARED.txt"), "line-1\n")
	support.Git(t, sub, "commit", "-qam", "t5: sub first")
	support.Git(t, sub, "push", "-q", "origin", "main")

	// Root view: subject woven in (SHA differs from C — v5 allows), content
	// placed, and the history is LINEAR (zero merge commits).
	root := support.CloneRepo(t, s, s.RepoURL("t5"))
	require.Contains(t, support.Git(t, root, "log", "--format=%s"), "t5: sub first", "根 log 缺子树提交 subject")
	require.Empty(t, strings.TrimSpace(support.Git(t, root, "log", "--merges", "--format=%s")), "根历史应线性无 merge")
	require.Equal(t, "base\nline-1\n", support.ReadFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")), "根工作树文件")
	// The parent view (lib) sees the descendant change via its scoped weave.
	parent := support.CloneRepo(t, s, s.TreeURL("t5", "lib"))
	assert.Equal(t, "base\nline-1\n", support.ReadFile(t, filepath.Join(parent, "nested", "SHARED.txt")), "父级视图内容")
	assert.Contains(t, support.Git(t, parent, "log", "--format=%s"), "t5: sub first", "父级视图 log 缺子树提交 subject")
	// Blob hash identical across three views.
	bSub, bPar, bRoot := support.BlobOf(t, sub, "SHARED.txt"),
		support.BlobOf(t, parent, "nested/SHARED.txt"), support.BlobOf(t, root, "lib/nested/SHARED.txt")
	require.NotEmpty(t, bSub, "blob 缺失")
	assert.Equal(t, bSub, bPar, "sub/parent blob 不一致")
	assert.Equal(t, bSub, bRoot, "sub/root blob 不一致")
	support.FsckClean(t, sub)
	support.FsckClean(t, parent)
	support.FsckClean(t, root)
}

// --- T7 consecutive subtree pushes (no pull) + own pull = up to date ---
// Regression: before the resolveViewTip fix the second push was always rejected
// (the advertised tip was a synthetic thin commit, not the adopted C).
func TestConsecutiveSubtreePushes(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t7")

	sub := support.CloneRepo(t, s, s.TreeURL("t7", "lib/nested"))
	for i := 1; i <= 3; i++ {
		support.AppendLine(t, filepath.Join(sub, "SHARED.txt"), fmt.Sprintf("c%d\n", i))
		support.Git(t, sub, "commit", "-qam", fmt.Sprintf("t7: consecutive %d", i))
		support.Git(t, sub, "push", "-q", "origin", "main") // any rejection is Fatal
	}
	// After push the server tip = the just-pushed commit → own pull should be up to date.
	require.Contains(t, support.Git(t, sub, "pull"), "Already up to date", "自身 pull 应为 up to date")
}

// --- T8 non-interference parallelism + in-path mutual exclusion (two sides of the docs/design/data-model.md §12 core goal) ---
//
//	v4 semantics: a subtree view tip moves only with [this path's content] —
//	root-side edits outside X leave the tip unmoved, so a stale subtree push
//	succeeds without pull (parallel iteration never interferes); edits inside
//	X advance the tip, and stale pushes are rejected (lost-update guard; recovery pull --rebase → push).
func TestCrossViewNonInterference(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t8")

	root := support.CloneRepo(t, s, s.RepoURL("t8"))
	sub := support.CloneRepo(t, s, s.TreeURL("t8", "lib/nested"))

	// ① Non-interference: root edits outside X (lib/a.txt); the stale subtree push passes directly.
	support.Write(t, filepath.Join(root, "lib", "a.txt"), "a-v2\n")
	support.Git(t, root, "commit", "-qam", "t8: root advances (outside X)")
	support.Git(t, root, "push", "-q", "origin", "main")

	support.AppendLine(t, filepath.Join(sub, "SHARED.txt"), "parallel-ok\n")
	support.Git(t, sub, "commit", "-qam", "t8: sub parallel (unrelated root change)")
	support.Git(t, sub, "push", "-q", "origin", "main") // no pull: non-interference

	root2 := support.CloneRepo(t, s, s.RepoURL("t8"))
	assert.Equal(t, "a-v2\n", support.ReadFile(t, filepath.Join(root2, "lib", "a.txt")), "根侧变更丢失")
	assert.Contains(t, support.ReadFile(t, filepath.Join(root2, "lib", "nested", "SHARED.txt")), "parallel-ok", "子树变更未达根")

	// ② In-path mutual exclusion: root edits inside X; the stale subtree push must be rejected → recovery flow.
	sub2 := support.CloneRepo(t, s, s.TreeURL("t8", "lib/nested"))
	root2edit := support.CloneRepo(t, s, s.RepoURL("t8"))
	support.Write(t, filepath.Join(root2edit, "lib", "nested", "SHARED.txt"), "base\nroot-inside-X\n")
	support.Git(t, root2edit, "commit", "-qam", "t8: root edits inside X")
	support.Git(t, root2edit, "push", "-q", "origin", "main")

	support.AppendLine(t, filepath.Join(sub2, "SHARED.txt"), "divergent\n")
	support.Git(t, sub2, "commit", "-qam", "t8: sub2 stale inside X")
	out, err := support.GitTry(sub2, "push", "origin", "main")
	require.Error(t, err, "X 内陈旧 push 未被拒:\n%s", out)
	require.Contains(t, out, "rejected", "X 内陈旧 push 未被拒")
	// Real conflict resolved explicitly (docs/design/data-model.md §12: the server does no content merge).
	out, err = support.GitTry(sub2, "pull", "--rebase")
	if err != nil {
		require.Contains(t, out, "CONFLICT", "rebase 失败但非冲突: %v", err)
	}
	support.Write(t, filepath.Join(sub2, "SHARED.txt"), "base\nroot-inside-X\ndivergent\n(resolved)\n")
	support.Git(t, sub2, "add", "-A")
	support.Git(t, sub2, "-c", "core.editor=true", "rebase", "--continue")
	support.Git(t, sub2, "push", "-q", "origin", "main")
	root3 := support.CloneRepo(t, s, s.RepoURL("t8"))
	require.Contains(t, support.ReadFile(t, filepath.Join(root3, "lib", "nested", "SHARED.txt")), "divergent",
		"恢复后子树变更未达根")
}

// --- T10 multi-user collaboration: same file, root/parent/subtree three views, five serial cross rounds, zero conflicts ---
// Regression: before view tips were chained, merge-base landed on the repo-scope root
// commit → add/add phantom conflict on path X (even with fully serial edits). Discipline: pull → edit → push.
func TestMultiUserSameFileCollaboration(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t10")

	R := support.CloneRepo(t, s, s.RepoURL("t10"))               // carol：根
	P := support.CloneRepo(t, s, s.TreeURL("t10", "lib"))        // bob：父级
	S := support.CloneRepo(t, s, s.TreeURL("t10", "lib/nested")) // alice：子树

	rounds := []struct {
		dir, rel, line, msg string
	}{
		{S, "SHARED.txt", "line-A by alice\n", "t10 r1: alice"},
		{P, "nested/SHARED.txt", "line-B by bob\n", "t10 r2: bob"},
		{S, "SHARED.txt", "line-C by alice\n", "t10 r3: alice"},
		{R, "lib/nested/SHARED.txt", "line-D by carol\n", "t10 r4: carol"},
		{P, "nested/SHARED.txt", "line-E by bob\n", "t10 r5: bob"},
	}
	var hashes []string
	for _, r := range rounds {
		// Pull first: zero-conflict assertion (on conflict git exits non-zero with CONFLICT in output).
		out, err := support.GitTry(r.dir, "pull")
		require.NoError(t, err, "[%s] pull 失败（串行协作不应冲突）\n%s", r.msg, out)
		require.NotContains(t, out, "CONFLICT", "[%s] pull 冲突（串行协作不应冲突）", r.msg)
		support.AppendLine(t, filepath.Join(r.dir, filepath.FromSlash(r.rel)), r.line)
		support.Git(t, r.dir, "commit", "-qam", r.msg)
		out, err = support.GitTry(r.dir, "push", "origin", "main")
		require.NoError(t, err, "[%s] push 失败（串行协作不应被拒）\n%s", r.msg, out)
		require.NotContains(t, out, "rejected", "[%s] push 被拒（串行协作不应被拒）", r.msg)
		hashes = append(hashes, support.HeadOf(t, r.dir))
	}

	// Wrap-up: everyone pulls, then assert convergence.
	for _, d := range []string{R, P, S} {
		out, err := support.GitTry(d, "pull")
		require.NoError(t, err, "收尾 pull 失败(%s)\nLOCAL:%s\nREMOTE:%s", d,
			support.Git(t, d, "log", "--oneline", "-3"), support.Git(t, d, "log", "--oneline", "-3", "origin/main"))
		require.NotContains(t, out, "CONFLICT", "收尾 pull 冲突(%s)", d)
	}
	bS, bP, bR := support.BlobOf(t, S, "SHARED.txt"),
		support.BlobOf(t, P, "nested/SHARED.txt"), support.BlobOf(t, R, "lib/nested/SHARED.txt")
	require.NotEmpty(t, bS, "blob 缺失")
	assert.Equal(t, bS, bP, "三视图 blob 不一致 (S/P)")
	assert.Equal(t, bS, bR, "三视图 blob 不一致 (S/R)")
	require.Contains(t, support.ReadFile(t, filepath.Join(S, "SHARED.txt")), "line-E by bob", "最终内容缺最后一行")
	// v5 consistency (docs/design/data-model.md §13): the root view is a linear weave — every
	// collaboration commit appears exactly once by SUBJECT; subtree-scope
	// commits enter as rewrites (SHA allowed to differ, T5 semantics), root
	// pushes land verbatim; content consistency is guaranteed by the
	// three-view blob hash assertion above.
	rootLog := support.Git(t, R, "log", "--format=%s")
	for _, r := range rounds {
		assert.Equal(t, 1, strings.Count(rootLog, r.msg+"\n"), "根视图 subject %q 出现次数", r.msg)
	}
	assert.Empty(t, strings.TrimSpace(support.Git(t, R, "log", "--merges", "--format=%s")), "根历史应线性无 merge")
	// The author's own view also keeps the original hash via identity mapping (r1/r3=alice→S).
	for _, i := range []int{0, 2} {
		assert.True(t, support.HasCommit(t, S, hashes[i]),
			"协作提交 %d（%s）未出现在作者自己的子树视图（身份映射应保留原 hash）", i+1, hashes[i][:9])
	}
	support.FsckClean(t, R)
	support.FsckClean(t, P)
	support.FsckClean(t, S)
}

// --- T18 sequential multi-scope arrivals: each commit folds exactly once ---
// Demo-shaped flow (docs/design/data-model.md §13): alternating arrivals across scopes must
// not duplicate subjects nor churn advertised tips. The rewrite-space
// exclusion (prevWoven ancestry) could never see graft sources — their
// originals never enter it, only their rewrites do — so every
// materialization re-folded them; per-source woven markers (source space)
// close that. kind-2 ancestor entries no-op on the parent X-diff, not on
// the current view tree (which misjudges forks and rolls X back).
func TestSequentialMultiScopeStability(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t18")

	nested := support.CloneRepo(t, s, s.TreeURL("t18", "lib/nested"))
	lib := support.CloneRepo(t, s, s.TreeURL("t18", "lib"))
	root := support.CloneRepo(t, s, s.RepoURL("t18"))
	for _, d := range []string{nested, lib, root} {
		support.Git(t, d, "config", "pull.rebase", "true")
	}

	// n1 → root pull → l1 → root pull → r1 (outside X) → n2 without pull.
	support.AppendLine(t, filepath.Join(nested, "SHARED.txt"), "n1\n")
	support.Git(t, nested, "commit", "-qam", "n1")
	support.Git(t, nested, "push", "-q", "origin", "main")
	support.Git(t, root, "pull", "-q", "origin", "main")
	// The lib view legitimately moved (nested ⊂ lib), so lib pulls before
	// pushing — git-equivalent fetch-first, not cross-directory locking.
	support.Git(t, lib, "pull", "-q", "origin", "main")

	support.AppendLine(t, filepath.Join(lib, "a.txt"), "l1\n")
	support.Git(t, lib, "commit", "-qam", "l1")
	support.Git(t, lib, "push", "-q", "origin", "main")
	support.Git(t, root, "pull", "-q", "origin", "main")

	support.Write(t, filepath.Join(root, "README.md"), "r1\n")
	support.Git(t, root, "commit", "-qam", "r1")
	support.Git(t, root, "push", "-q", "origin", "main")

	support.AppendLine(t, filepath.Join(nested, "SHARED.txt"), "n2\n")
	support.Git(t, nested, "commit", "-qam", "n2")
	out, err := support.GitTry(nested, "push", "origin", "main")
	require.NoError(t, err, "子树无 pull push 被拒（非干扰语义破坏）\n%s", out)
	support.Git(t, root, "pull", "-q", "origin", "main")

	// Every subject exactly once, linear history, content intact.
	log := support.Git(t, root, "log", "--format=%s")
	for _, subj := range []string{"n1", "l1", "r1", "n2"} {
		assert.Equal(t, 1, strings.Count(log, subj+"\n"), "subject %q 出现次数（重复折叠）", subj)
	}
	assert.Empty(t, strings.TrimSpace(support.Git(t, root, "log", "--merges", "--format=%s")), "根历史应线性无 merge")
	assert.Equal(t, "base\nn1\nn2\n", support.ReadFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")), "根视图 SHARED")
	assert.Equal(t, "a-v1\nl1\n", support.ReadFile(t, filepath.Join(root, "lib", "a.txt")), "根视图 a.txt")

	// Advertised root tip must not churn across other views' materializations.
	remote := s.RepoURL("t18")
	h1 := support.Git(t, root, "ls-remote", remote, "refs/heads/main")
	support.Git(t, lib, "pull", "-q", "origin", "main")
	h2 := support.Git(t, root, "ls-remote", remote, "refs/heads/main")
	require.Contains(t, h1, "refs/heads/main", "ls-remote 输出异常:\n%s", h1)
	require.Equal(t, h1, h2, "根 tip 在其他视图物化间漂移")

	// All three views converge to identical content.
	support.Git(t, nested, "pull", "-q", "origin", "main")
	assert.Equal(t, "base\nn1\nn2\n", support.ReadFile(t, filepath.Join(nested, "SHARED.txt")), "nested 视图 SHARED")
	assert.Equal(t, "a-v1\nl1\n", support.ReadFile(t, filepath.Join(lib, "a.txt")), "lib 视图 a.txt")
	support.FsckClean(t, nested)
	support.FsckClean(t, lib)
	support.FsckClean(t, root)

	// Rebase recovery, then continue pushing: after resolving an in-X
	// divergence the client's chain holds a cross-view rewrite of the
	// projection PLUS newer local content — ahead of, not stale against,
	// the root side. The projection-side guard must admit it (docs/design/data-model.md §13).
	root2 := support.CloneRepo(t, s, s.RepoURL("t18"))
	support.AppendLine(t, filepath.Join(root2, "lib", "nested", "SHARED.txt"), "root-in-X\n")
	support.Git(t, root2, "commit", "-qam", "root-in-X")
	support.Git(t, root2, "push", "-q", "origin", "main")

	support.AppendLine(t, filepath.Join(nested, "SHARED.txt"), "stale\n")
	support.Git(t, nested, "commit", "-qam", "nested-stale")
	out, err = support.GitTry(nested, "push", "origin", "main")
	require.Error(t, err, "X 内陈旧 push 未被拒:\n%s", out)
	require.Contains(t, out, "rejected", "X 内陈旧 push 未被拒")
	// Same-file divergence: the rebase conflicts; resolve explicitly (the
	// server does no content merge, docs/design/data-model.md §12).
	out, err = support.GitTry(nested, "pull", "-q", "--rebase", "origin", "main")
	if err != nil {
		require.Contains(t, out, "CONFLICT", "rebase 失败但非冲突: %v", err)
	}
	support.Write(t, filepath.Join(nested, "SHARED.txt"), "base\nn1\nn2\nroot-in-X\nstale\n")
	support.Git(t, nested, "add", "-A")
	support.Git(t, nested, "-c", "core.editor=true", "rebase", "--continue")
	out, err = support.GitTry(nested, "push", "origin", "main")
	require.NoError(t, err, "恢复后 push 被拒（投影侧守卫误判领先客户端）\n%s", out)
	// And the very next round-trip still flows.
	support.AppendLine(t, filepath.Join(nested, "SHARED.txt"), "after-recovery\n")
	support.Git(t, nested, "commit", "-qam", "after-recovery")
	out, err = support.GitTry(nested, "push", "origin", "main")
	require.NoError(t, err, "恢复后下一轮 push 被拒\n%s", out)
	support.Git(t, root, "pull", "-q", "origin", "main")
	require.Equal(t, "base\nn1\nn2\nroot-in-X\nstale\nafter-recovery\n",
		support.ReadFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")), "根视图最终 SHARED")
	support.FsckClean(t, nested)
	support.FsckClean(t, root)
}

// --- T52: cross-scope isolation around own-scope pushes (+ mixed-scope
// semantics). The sequence "push to subtree A, re-materialize A's view, then
// push to sibling B and re-materialize both" was never covered by T5/T7/T8 —
// and a review-flow misdiagnosis (feat/review 2cea648) showed why exact-name
// assertions matter: a helper writing the anchor-nested path inside a scoped
// clone produces a MIXED-SCOPE commit whose faithful projection (nested
// paths in the subtree view, anchor-doubled paths in the root weave) reads
// exactly like "view corruption" if tests only count files. This test pins
// both: precise isolation, and mixed-scope pushes as content.
func TestSubtreeViewCrossScopeIsolation(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	dir := t.TempDir()
	support.Git(t, dir, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(dir, "README.md"), "r\n")
	support.Write(t, filepath.Join(dir, "a", "x.txt"), "x1\n")
	support.Write(t, filepath.Join(dir, "b", "y.txt"), "y1\n")
	support.Git(t, dir, "add", "-A")
	support.Git(t, dir, "commit", "-qm", "seed")
	support.Git(t, dir, "push", "-q", s.RepoURL("d1"), "main")

	// Push to A, then re-materialize A (a fresh clone).
	subA := support.CloneRepo(t, s, s.TreeURL("d1", "a"))
	support.Write(t, filepath.Join(subA, "x.txt"), "x2\n")
	support.Git(t, subA, "commit", "-qam", "a: change")
	support.Git(t, subA, "push", "-q", "origin", "main")
	reA := support.CloneRepo(t, s, s.TreeURL("d1", "a"))
	require.Equal(t, []string{"x.txt"}, strings.Fields(support.Git(t, reA, "ls-files")), "A scoped")
	require.Equal(t, "x2\n", support.ReadFile(t, filepath.Join(reA, "x.txt")), "A content")

	// B must still push and stay scoped — exact name, not just count.
	subB := support.CloneRepo(t, s, s.TreeURL("d1", "b"))
	require.Equal(t, []string{"y.txt"}, strings.Fields(support.Git(t, subB, "ls-files")), "B scoped before push")
	support.Write(t, filepath.Join(subB, "y.txt"), "y2\n")
	support.Git(t, subB, "commit", "-qam", "b: change")
	support.Git(t, subB, "push", "-q", "origin", "main")
	reB := support.CloneRepo(t, s, s.TreeURL("d1", "b"))
	require.Equal(t, []string{"y.txt"}, strings.Fields(support.Git(t, reB, "ls-files")), "B scoped")
	require.Equal(t, "y2\n", support.ReadFile(t, filepath.Join(reB, "y.txt")), "B content")

	// A unchanged by B's lifecycle.
	require.Equal(t, []string{"x.txt"}, strings.Fields(support.Git(t, reA, "ls-files")), "A still scoped")

	// The root weave sees each subtree exactly once, at its own path.
	root := support.CloneRepo(t, s, s.RepoURL("d1"))
	require.Equal(t, []string{"README.md", "a/x.txt", "b/y.txt"}, strings.Fields(support.Git(t, root, "ls-files")), "root shape")
	require.Equal(t, "x2\n", support.ReadFile(t, filepath.Join(root, "a", "x.txt")), "root sees A")
	require.Equal(t, "y2\n", support.ReadFile(t, filepath.Join(root, "b", "y.txt")), "root sees B")

	// Mixed-scope push (client creates the anchor path inside its scoped
	// clone) is CONTENT, faithfully projected (xviewTree's mixed-scope
	// rule): the subtree view serves the commit verbatim, the root weave
	// nests it under its own path — and nothing leaks across scopes.
	mix := support.CloneRepo(t, s, s.TreeURL("d1", "b"))
	support.Write(t, filepath.Join(mix, "b", "y.txt"), "y3\n")
	support.Git(t, mix, "add", ".")
	support.Git(t, mix, "commit", "-qm", "b: nested")
	support.Git(t, mix, "push", "-q", "origin", "main")
	reMix := support.CloneRepo(t, s, s.TreeURL("d1", "b"))
	require.Equal(t, []string{"b/y.txt", "y.txt"}, strings.Fields(support.Git(t, reMix, "ls-files")), "mixed-scope view is verbatim")
	require.Equal(t, "y3\n", support.ReadFile(t, filepath.Join(reMix, "b", "y.txt")), "nested content")
	root2 := support.CloneRepo(t, s, s.RepoURL("d1"))
	require.Equal(t, []string{"README.md", "a/x.txt", "b/b/y.txt", "b/y.txt"}, strings.Fields(support.Git(t, root2, "ls-files")), "root nests mixed scope")
	require.Equal(t, []string{"x.txt"}, strings.Fields(support.Git(t, reA, "ls-files")), "A untouched by B's mixed push")
	support.FsckClean(t, reMix)
	support.FsckClean(t, root2)
}
