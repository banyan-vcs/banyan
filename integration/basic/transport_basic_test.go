// Goal: basic transport — root and subtree clone roundtrips, ref operations,
// and incremental push/pull between clones (T1–T4).
package basic

import (
	"github.com/banyan-vcs/banyan/integration/support"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- T1 root repo push → clone roundtrip ---
func TestRootCloneRoundtrip(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t1")
	want := support.HeadOf(t, src)

	dst := support.CloneRepo(t, s, s.RepoURL("t1"))
	assert.Equal(t, want, support.HeadOf(t, dst), "clone HEAD")
	assert.Equal(t, 1, support.CountCommits(t, dst), "提交数")
	assert.Equal(t, "base\n", support.ReadFile(t, filepath.Join(dst, "lib", "nested", "SHARED.txt")))
	support.FsckClean(t, dst)
}

// --- T2 subtree clone: worktree = subdirectory content, original hash enters subtree history ---
func TestSubtreeCloneUnifiedDAG(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t2")
	_ = src // v4: subtree appears as a projection chain (different SHA); src only seeds

	sub := support.CloneRepo(t, s, s.TreeURL("t2", "lib/nested"))
	// Worktree root = subdirectory content.
	assert.Equal(t, "base\n", support.ReadFile(t, filepath.Join(sub, "SHARED.txt")), "子树工作树内容")
	_, err := os.Stat(filepath.Join(sub, "README.md"))
	require.Error(t, err, "子树工作树不应包含父级文件 README.md")
	// v4 timeline-first (docs/design/data-model.md §12): subtree history = this path's timeline (projection
	// chain, SHA differs from the root side — allowed); content matches the root (blob equality).
	assert.NotEmpty(t, support.BlobOf(t, sub, "SHARED.txt"))
	assert.Equal(t, support.BlobOf(t, src, "lib/nested/SHARED.txt"), support.BlobOf(t, sub, "SHARED.txt"), "子树与源 blob 不一致")
	// Core CI-efficiency assertion: subtree clone carries only subtree scale (far fewer objects than the full repo).
	subObjects := support.CountInPack(t, sub)
	fullObjects := support.CountInPack(t, support.CloneRepo(t, s, s.RepoURL("t2")))
	assert.Less(t, subObjects, fullObjects, "子树 clone 对象数应远小于全仓")
	support.FsckClean(t, sub)
}

// --- T3 ref operations: create / update / delete branch ---
func TestRefOperations(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t3")

	// Create branch.
	support.Git(t, src, "checkout", "-qb", "feature")
	support.Write(t, filepath.Join(src, "feat.txt"), "f1\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "t3: feature commit")
	support.Git(t, src, "push", "-q", s.RepoURL("t3"), "feature")
	require.Contains(t, support.Git(t, "", "ls-remote", s.RepoURL("t3")), "refs/heads/feature", "ls-remote 缺 feature")

	// Update branch.
	support.Write(t, filepath.Join(src, "feat.txt"), "f2\n")
	support.Git(t, src, "commit", "-qam", "t3: feature update")
	support.Git(t, src, "push", "-q", s.RepoURL("t3"), "feature")

	// Delete branch (all-zero new → server CAS zeroing path).
	support.Git(t, src, "push", "-q", s.RepoURL("t3"), ":feature")
	require.NotContains(t, support.Git(t, "", "ls-remote", s.RepoURL("t3")), "refs/heads/feature",
		"删除分支后 ls-remote 仍有 feature")
}

// --- T4 incremental push + pull from another clone ---
func TestIncrementalPushPull(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t4")

	a := support.CloneRepo(t, s, s.RepoURL("t4"))
	b := support.CloneRepo(t, s, s.RepoURL("t4"))

	support.Write(t, filepath.Join(a, "inc.txt"), "inc\n")
	support.Git(t, a, "add", "-A")
	support.Git(t, a, "commit", "-qm", "t4: incremental")
	support.Git(t, a, "push", "-q", "origin", "main")

	out := support.Git(t, b, "pull")
	require.True(t, strings.Contains(out, "Fast-forward") || strings.Contains(out, "Already up to date"),
		"pull 输出异常:\n%s", out)
	require.True(t, support.HasCommit(t, b, support.HeadOf(t, a)), "b 未见增量提交")
}
