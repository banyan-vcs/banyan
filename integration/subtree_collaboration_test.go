// Goal: subtree collaboration semantics — linear-weave adoption, consecutive
// pushes, non-interference parallelism vs in-path mutual exclusion, multi-user
// same-file collaboration, and multi-scope arrival stability (T5/T7/T8/T10/T18).
package integration

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- T5 subtree push: linear-weave root view / parent view sees it / file placement ---
// v5 (docs/02 §13): the subtree push advances ONLY its own timeline; the root
// view weaves the commit in linearly (same subject, different SHA — allowed),
// no merge scaffolding; the parent view (lib) also sees the change via its
// scoped weave.
func TestSubtreePushAdoption(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t5")

	sub := cloneRepo(t, s, s.treeURL("t5", "lib/nested"))
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "line-1\n")
	git(t, sub, "commit", "-qam", "t5: sub first")
	git(t, sub, "push", "-q", "origin", "main")

	// Root view: subject woven in (SHA differs from C — v5 allows), content
	// placed, and the history is LINEAR (zero merge commits).
	root := cloneRepo(t, s, s.repoURL("t5"))
	require.Contains(t, git(t, root, "log", "--format=%s"), "t5: sub first", "根 log 缺子树提交 subject")
	require.Empty(t, strings.TrimSpace(git(t, root, "log", "--merges", "--format=%s")), "根历史应线性无 merge")
	require.Equal(t, "base\nline-1\n", readFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")), "根工作树文件")
	// The parent view (lib) sees the descendant change via its scoped weave.
	parent := cloneRepo(t, s, s.treeURL("t5", "lib"))
	assert.Equal(t, "base\nline-1\n", readFile(t, filepath.Join(parent, "nested", "SHARED.txt")), "父级视图内容")
	assert.Contains(t, git(t, parent, "log", "--format=%s"), "t5: sub first", "父级视图 log 缺子树提交 subject")
	// Blob hash identical across three views.
	bSub, bPar, bRoot := blobOf(t, sub, "SHARED.txt"),
		blobOf(t, parent, "nested/SHARED.txt"), blobOf(t, root, "lib/nested/SHARED.txt")
	require.NotEmpty(t, bSub, "blob 缺失")
	assert.Equal(t, bSub, bPar, "sub/parent blob 不一致")
	assert.Equal(t, bSub, bRoot, "sub/root blob 不一致")
	fsckClean(t, sub)
	fsckClean(t, parent)
	fsckClean(t, root)
}

// --- T7 consecutive subtree pushes (no pull) + own pull = up to date ---
// Regression: before the resolveViewTip fix the second push was always rejected
// (the advertised tip was a synthetic thin commit, not the adopted C).
func TestConsecutiveSubtreePushes(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t7")

	sub := cloneRepo(t, s, s.treeURL("t7", "lib/nested"))
	for i := 1; i <= 3; i++ {
		appendLine(t, filepath.Join(sub, "SHARED.txt"), fmt.Sprintf("c%d\n", i))
		git(t, sub, "commit", "-qam", fmt.Sprintf("t7: consecutive %d", i))
		git(t, sub, "push", "-q", "origin", "main") // any rejection is Fatal
	}
	// After push the server tip = the just-pushed commit → own pull should be up to date.
	require.Contains(t, git(t, sub, "pull"), "Already up to date", "自身 pull 应为 up to date")
}

// --- T8 non-interference parallelism + in-path mutual exclusion (two sides of the docs/02 §12 core goal) ---
//
//	v4 semantics: a subtree view tip moves only with [this path's content] —
//	root-side edits outside X leave the tip unmoved, so a stale subtree push
//	succeeds without pull (parallel iteration never interferes); edits inside
//	X advance the tip, and stale pushes are rejected (lost-update guard; recovery pull --rebase → push).
func TestCrossViewNonInterference(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t8")

	root := cloneRepo(t, s, s.repoURL("t8"))
	sub := cloneRepo(t, s, s.treeURL("t8", "lib/nested"))

	// ① Non-interference: root edits outside X (lib/a.txt); the stale subtree push passes directly.
	write(t, filepath.Join(root, "lib", "a.txt"), "a-v2\n")
	git(t, root, "commit", "-qam", "t8: root advances (outside X)")
	git(t, root, "push", "-q", "origin", "main")

	appendLine(t, filepath.Join(sub, "SHARED.txt"), "parallel-ok\n")
	git(t, sub, "commit", "-qam", "t8: sub parallel (unrelated root change)")
	git(t, sub, "push", "-q", "origin", "main") // no pull: non-interference

	root2 := cloneRepo(t, s, s.repoURL("t8"))
	assert.Equal(t, "a-v2\n", readFile(t, filepath.Join(root2, "lib", "a.txt")), "根侧变更丢失")
	assert.Contains(t, readFile(t, filepath.Join(root2, "lib", "nested", "SHARED.txt")), "parallel-ok", "子树变更未达根")

	// ② In-path mutual exclusion: root edits inside X; the stale subtree push must be rejected → recovery flow.
	sub2 := cloneRepo(t, s, s.treeURL("t8", "lib/nested"))
	root2edit := cloneRepo(t, s, s.repoURL("t8"))
	write(t, filepath.Join(root2edit, "lib", "nested", "SHARED.txt"), "base\nroot-inside-X\n")
	git(t, root2edit, "commit", "-qam", "t8: root edits inside X")
	git(t, root2edit, "push", "-q", "origin", "main")

	appendLine(t, filepath.Join(sub2, "SHARED.txt"), "divergent\n")
	git(t, sub2, "commit", "-qam", "t8: sub2 stale inside X")
	out, err := gitTry(sub2, "push", "origin", "main")
	require.Error(t, err, "X 内陈旧 push 未被拒:\n%s", out)
	require.Contains(t, out, "rejected", "X 内陈旧 push 未被拒")
	// Real conflict resolved explicitly (docs/02 §12: the server does no content merge).
	out, err = gitTry(sub2, "pull", "--rebase")
	if err != nil {
		require.Contains(t, out, "CONFLICT", "rebase 失败但非冲突: %v", err)
	}
	write(t, filepath.Join(sub2, "SHARED.txt"), "base\nroot-inside-X\ndivergent\n(resolved)\n")
	git(t, sub2, "add", "-A")
	git(t, sub2, "-c", "core.editor=true", "rebase", "--continue")
	git(t, sub2, "push", "-q", "origin", "main")
	root3 := cloneRepo(t, s, s.repoURL("t8"))
	require.Contains(t, readFile(t, filepath.Join(root3, "lib", "nested", "SHARED.txt")), "divergent",
		"恢复后子树变更未达根")
}

// --- T10 multi-user collaboration: same file, root/parent/subtree three views, five serial cross rounds, zero conflicts ---
// Regression: before view tips were chained, merge-base landed on the repo-scope root
// commit → add/add phantom conflict on path X (even with fully serial edits). Discipline: pull → edit → push.
func TestMultiUserSameFileCollaboration(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t10")

	R := cloneRepo(t, s, s.repoURL("t10"))               // carol：根
	P := cloneRepo(t, s, s.treeURL("t10", "lib"))        // bob：父级
	S := cloneRepo(t, s, s.treeURL("t10", "lib/nested")) // alice：子树

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
		out, err := gitTry(r.dir, "pull")
		require.NoError(t, err, "[%s] pull 失败（串行协作不应冲突）\n%s", r.msg, out)
		require.NotContains(t, out, "CONFLICT", "[%s] pull 冲突（串行协作不应冲突）", r.msg)
		appendLine(t, filepath.Join(r.dir, filepath.FromSlash(r.rel)), r.line)
		git(t, r.dir, "commit", "-qam", r.msg)
		out, err = gitTry(r.dir, "push", "origin", "main")
		require.NoError(t, err, "[%s] push 失败（串行协作不应被拒）\n%s", r.msg, out)
		require.NotContains(t, out, "rejected", "[%s] push 被拒（串行协作不应被拒）", r.msg)
		hashes = append(hashes, headOf(t, r.dir))
	}

	// Wrap-up: everyone pulls, then assert convergence.
	for _, d := range []string{R, P, S} {
		out, err := gitTry(d, "pull")
		require.NoError(t, err, "收尾 pull 失败(%s)\nLOCAL:%s\nREMOTE:%s", d,
			git(t, d, "log", "--oneline", "-3"), git(t, d, "log", "--oneline", "-3", "origin/main"))
		require.NotContains(t, out, "CONFLICT", "收尾 pull 冲突(%s)", d)
	}
	bS, bP, bR := blobOf(t, S, "SHARED.txt"),
		blobOf(t, P, "nested/SHARED.txt"), blobOf(t, R, "lib/nested/SHARED.txt")
	require.NotEmpty(t, bS, "blob 缺失")
	assert.Equal(t, bS, bP, "三视图 blob 不一致 (S/P)")
	assert.Equal(t, bS, bR, "三视图 blob 不一致 (S/R)")
	require.Contains(t, readFile(t, filepath.Join(S, "SHARED.txt")), "line-E by bob", "最终内容缺最后一行")
	// v5 consistency (docs/02 §13): the root view is a linear weave — every
	// collaboration commit appears exactly once by SUBJECT; subtree-scope
	// commits enter as rewrites (SHA allowed to differ, T5 semantics), root
	// pushes land verbatim; content consistency is guaranteed by the
	// three-view blob hash assertion above.
	rootLog := git(t, R, "log", "--format=%s")
	for _, r := range rounds {
		assert.Equal(t, 1, strings.Count(rootLog, r.msg+"\n"), "根视图 subject %q 出现次数", r.msg)
	}
	assert.Empty(t, strings.TrimSpace(git(t, R, "log", "--merges", "--format=%s")), "根历史应线性无 merge")
	// The author's own view also keeps the original hash via identity mapping (r1/r3=alice→S).
	for _, i := range []int{0, 2} {
		assert.True(t, hasCommit(t, S, hashes[i]),
			"协作提交 %d（%s）未出现在作者自己的子树视图（身份映射应保留原 hash）", i+1, hashes[i][:9])
	}
	fsckClean(t, R)
	fsckClean(t, P)
	fsckClean(t, S)
}

// --- T18 sequential multi-scope arrivals: each commit folds exactly once ---
// Demo-shaped flow (docs/02 §13): alternating arrivals across scopes must
// not duplicate subjects nor churn advertised tips. The rewrite-space
// exclusion (prevWoven ancestry) could never see graft sources — their
// originals never enter it, only their rewrites do — so every
// materialization re-folded them; per-source woven markers (source space)
// close that. kind-2 ancestor entries no-op on the parent X-diff, not on
// the current view tree (which misjudges forks and rolls X back).
func TestSequentialMultiScopeStability(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t18")

	nested := cloneRepo(t, s, s.treeURL("t18", "lib/nested"))
	lib := cloneRepo(t, s, s.treeURL("t18", "lib"))
	root := cloneRepo(t, s, s.repoURL("t18"))
	for _, d := range []string{nested, lib, root} {
		git(t, d, "config", "pull.rebase", "true")
	}

	// n1 → root pull → l1 → root pull → r1 (outside X) → n2 without pull.
	appendLine(t, filepath.Join(nested, "SHARED.txt"), "n1\n")
	git(t, nested, "commit", "-qam", "n1")
	git(t, nested, "push", "-q", "origin", "main")
	git(t, root, "pull", "-q", "origin", "main")
	// The lib view legitimately moved (nested ⊂ lib), so lib pulls before
	// pushing — git-equivalent fetch-first, not cross-directory locking.
	git(t, lib, "pull", "-q", "origin", "main")

	appendLine(t, filepath.Join(lib, "a.txt"), "l1\n")
	git(t, lib, "commit", "-qam", "l1")
	git(t, lib, "push", "-q", "origin", "main")
	git(t, root, "pull", "-q", "origin", "main")

	write(t, filepath.Join(root, "README.md"), "r1\n")
	git(t, root, "commit", "-qam", "r1")
	git(t, root, "push", "-q", "origin", "main")

	appendLine(t, filepath.Join(nested, "SHARED.txt"), "n2\n")
	git(t, nested, "commit", "-qam", "n2")
	out, err := gitTry(nested, "push", "origin", "main")
	require.NoError(t, err, "子树无 pull push 被拒（非干扰语义破坏）\n%s", out)
	git(t, root, "pull", "-q", "origin", "main")

	// Every subject exactly once, linear history, content intact.
	log := git(t, root, "log", "--format=%s")
	for _, subj := range []string{"n1", "l1", "r1", "n2"} {
		assert.Equal(t, 1, strings.Count(log, subj+"\n"), "subject %q 出现次数（重复折叠）", subj)
	}
	assert.Empty(t, strings.TrimSpace(git(t, root, "log", "--merges", "--format=%s")), "根历史应线性无 merge")
	assert.Equal(t, "base\nn1\nn2\n", readFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")), "根视图 SHARED")
	assert.Equal(t, "a-v1\nl1\n", readFile(t, filepath.Join(root, "lib", "a.txt")), "根视图 a.txt")

	// Advertised root tip must not churn across other views' materializations.
	remote := s.repoURL("t18")
	h1 := git(t, root, "ls-remote", remote, "refs/heads/main")
	git(t, lib, "pull", "-q", "origin", "main")
	h2 := git(t, root, "ls-remote", remote, "refs/heads/main")
	require.Contains(t, h1, "refs/heads/main", "ls-remote 输出异常:\n%s", h1)
	require.Equal(t, h1, h2, "根 tip 在其他视图物化间漂移")

	// All three views converge to identical content.
	git(t, nested, "pull", "-q", "origin", "main")
	assert.Equal(t, "base\nn1\nn2\n", readFile(t, filepath.Join(nested, "SHARED.txt")), "nested 视图 SHARED")
	assert.Equal(t, "a-v1\nl1\n", readFile(t, filepath.Join(lib, "a.txt")), "lib 视图 a.txt")
	fsckClean(t, nested)
	fsckClean(t, lib)
	fsckClean(t, root)

	// Rebase recovery, then continue pushing: after resolving an in-X
	// divergence the client's chain holds a cross-view rewrite of the
	// projection PLUS newer local content — ahead of, not stale against,
	// the root side. The projection-side guard must admit it (docs/02 §13).
	root2 := cloneRepo(t, s, s.repoURL("t18"))
	appendLine(t, filepath.Join(root2, "lib", "nested", "SHARED.txt"), "root-in-X\n")
	git(t, root2, "commit", "-qam", "root-in-X")
	git(t, root2, "push", "-q", "origin", "main")

	appendLine(t, filepath.Join(nested, "SHARED.txt"), "stale\n")
	git(t, nested, "commit", "-qam", "nested-stale")
	out, err = gitTry(nested, "push", "origin", "main")
	require.Error(t, err, "X 内陈旧 push 未被拒:\n%s", out)
	require.Contains(t, out, "rejected", "X 内陈旧 push 未被拒")
	// Same-file divergence: the rebase conflicts; resolve explicitly (the
	// server does no content merge, docs/02 §12).
	out, err = gitTry(nested, "pull", "-q", "--rebase", "origin", "main")
	if err != nil {
		require.Contains(t, out, "CONFLICT", "rebase 失败但非冲突: %v", err)
	}
	write(t, filepath.Join(nested, "SHARED.txt"), "base\nn1\nn2\nroot-in-X\nstale\n")
	git(t, nested, "add", "-A")
	git(t, nested, "-c", "core.editor=true", "rebase", "--continue")
	out, err = gitTry(nested, "push", "origin", "main")
	require.NoError(t, err, "恢复后 push 被拒（投影侧守卫误判领先客户端）\n%s", out)
	// And the very next round-trip still flows.
	appendLine(t, filepath.Join(nested, "SHARED.txt"), "after-recovery\n")
	git(t, nested, "commit", "-qam", "after-recovery")
	out, err = gitTry(nested, "push", "origin", "main")
	require.NoError(t, err, "恢复后下一轮 push 被拒\n%s", out)
	git(t, root, "pull", "-q", "origin", "main")
	require.Equal(t, "base\nn1\nn2\nroot-in-X\nstale\nafter-recovery\n",
		readFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")), "根视图最终 SHARED")
	fsckClean(t, nested)
	fsckClean(t, root)
}

// --- T52 (D1 post-mortem): cross-scope isolation around own-scope pushes.
// D1 was registered (2026-10-26, skip-guarded) as a scoped-weave defect
// after the review surface surfaced nested diff paths and nested checkouts.
// Investigation traced it to the review test HELPER writing the
// anchor-nested path inside a scoped clone (sub/services/auth/token.go) —
// a mixed-scope commit that the views project FAITHFULLY (xviewTree's
// mixed-scope rule): the subtree view serves it verbatim and the root weave
// nests it at services/auth/services/auth/. The engine was correct; the
// defect was the helper. This test now pins the real contract — an
// own-scope push followed by sibling re-materializations and pushes keeps
// every view scoped, content-exact, and mutually isolated.
func TestSubtreeViewCrossScopeIsolation(t *testing.T) {
	needGit(t)
	s := newServer(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(dir, "README.md"), "r\n")
	write(t, filepath.Join(dir, "a", "x.txt"), "x1\n")
	write(t, filepath.Join(dir, "b", "y.txt"), "y1\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "seed")
	git(t, dir, "push", "-q", s.repoURL("d1"), "main")

	// Push to A, then re-materialize A (a fresh clone).
	subA := cloneRepo(t, s, s.treeURL("d1", "a"))
	write(t, filepath.Join(subA, "x.txt"), "x2\n")
	git(t, subA, "commit", "-qam", "a: change")
	git(t, subA, "push", "-q", "origin", "main")
	reA := cloneRepo(t, s, s.treeURL("d1", "a"))
	require.Equal(t, []string{"x.txt"}, strings.Fields(git(t, reA, "ls-files")), "A scoped")
	require.Equal(t, "x2\n", readFile(t, filepath.Join(reA, "x.txt")), "A content")

	// B must still push and stay scoped — exact name, not just count.
	subB := cloneRepo(t, s, s.treeURL("d1", "b"))
	require.Equal(t, []string{"y.txt"}, strings.Fields(git(t, subB, "ls-files")), "B scoped before push")
	write(t, filepath.Join(subB, "y.txt"), "y2\n")
	git(t, subB, "commit", "-qam", "b: change")
	git(t, subB, "push", "-q", "origin", "main")
	reB := cloneRepo(t, s, s.treeURL("d1", "b"))
	require.Equal(t, []string{"y.txt"}, strings.Fields(git(t, reB, "ls-files")), "B scoped")
	require.Equal(t, "y2\n", readFile(t, filepath.Join(reB, "y.txt")), "B content")

	// A unchanged by B's lifecycle.
	require.Equal(t, []string{"x.txt"}, strings.Fields(git(t, reA, "ls-files")), "A still scoped")

	// The root weave sees each subtree exactly once, at its own path.
	root := cloneRepo(t, s, s.repoURL("d1"))
	require.Equal(t, []string{"README.md", "a/x.txt", "b/y.txt"}, strings.Fields(git(t, root, "ls-files")), "root shape")
	require.Equal(t, "x2\n", readFile(t, filepath.Join(root, "a", "x.txt")), "root sees A")
	require.Equal(t, "y2\n", readFile(t, filepath.Join(root, "b", "y.txt")), "root sees B")
	fsckClean(t, reA)
	fsckClean(t, root)
}
