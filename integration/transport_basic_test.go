// Goal: basic transport — root and subtree clone roundtrips, ref operations,
// and incremental push/pull between clones (T1–T4).
package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- T1 root repo push → clone roundtrip ---
func TestRootCloneRoundtrip(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t1")
	want := headOf(t, src)

	dst := cloneRepo(t, s, s.repoURL("t1"))
	assert.Equal(t, want, headOf(t, dst), "clone HEAD")
	assert.Equal(t, 1, countCommits(t, dst), "提交数")
	assert.Equal(t, "base\n", readFile(t, filepath.Join(dst, "lib", "nested", "SHARED.txt")))
	fsckClean(t, dst)
}

// --- T2 subtree clone: worktree = subdirectory content, original hash enters subtree history ---
func TestSubtreeCloneUnifiedDAG(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t2")
	_ = src // v4: subtree appears as a projection chain (different SHA); src only seeds

	sub := cloneRepo(t, s, s.treeURL("t2", "lib/nested"))
	// Worktree root = subdirectory content.
	assert.Equal(t, "base\n", readFile(t, filepath.Join(sub, "SHARED.txt")), "子树工作树内容")
	_, err := os.Stat(filepath.Join(sub, "README.md"))
	require.Error(t, err, "子树工作树不应包含父级文件 README.md")
	// v4 timeline-first (docs/01 §12): subtree history = this path's timeline (projection
	// chain, SHA differs from the root side — allowed); content matches the root (blob equality).
	assert.NotEmpty(t, blobOf(t, sub, "SHARED.txt"))
	assert.Equal(t, blobOf(t, src, "lib/nested/SHARED.txt"), blobOf(t, sub, "SHARED.txt"), "子树与源 blob 不一致")
	// Core CI-efficiency assertion: subtree clone carries only subtree scale (far fewer objects than the full repo).
	subObjects := countInPack(t, sub)
	fullObjects := countInPack(t, cloneRepo(t, s, s.repoURL("t2")))
	assert.Less(t, subObjects, fullObjects, "子树 clone 对象数应远小于全仓")
	fsckClean(t, sub)
}

// --- T3 ref operations: create / update / delete branch ---
func TestRefOperations(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t3")

	// Create branch.
	git(t, src, "checkout", "-qb", "feature")
	write(t, filepath.Join(src, "feat.txt"), "f1\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t3: feature commit")
	git(t, src, "push", "-q", s.repoURL("t3"), "feature")
	require.Contains(t, git(t, "", "ls-remote", s.repoURL("t3")), "refs/heads/feature", "ls-remote 缺 feature")

	// Update branch.
	write(t, filepath.Join(src, "feat.txt"), "f2\n")
	git(t, src, "commit", "-qam", "t3: feature update")
	git(t, src, "push", "-q", s.repoURL("t3"), "feature")

	// Delete branch (all-zero new → server CAS zeroing path).
	git(t, src, "push", "-q", s.repoURL("t3"), ":feature")
	require.NotContains(t, git(t, "", "ls-remote", s.repoURL("t3")), "refs/heads/feature",
		"删除分支后 ls-remote 仍有 feature")
}

// --- T4 incremental push + pull from another clone ---
func TestIncrementalPushPull(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t4")

	a := cloneRepo(t, s, s.repoURL("t4"))
	b := cloneRepo(t, s, s.repoURL("t4"))

	write(t, filepath.Join(a, "inc.txt"), "inc\n")
	git(t, a, "add", "-A")
	git(t, a, "commit", "-qm", "t4: incremental")
	git(t, a, "push", "-q", "origin", "main")

	out := git(t, b, "pull")
	require.True(t, strings.Contains(out, "Fast-forward") || strings.Contains(out, "Already up to date"),
		"pull 输出异常:\n%s", out)
	require.True(t, hasCommit(t, b, headOf(t, a)), "b 未见增量提交")
}
