// cli_stack_test.go — T92: banyan stack, the local changeset queue (docs/client/cli.md
// §3.8). Path-scoped push captures dirty paths (bytes in the stack blob
// store) and reverts them locally; commit lands entries oldest-first through
// the ordinary changeset RPC — cross-verified by a real git clone — and a
// moved base fails the first landing with exit 1 while untouched entries
// survive verbatim; pull + retry lands them on the fresh base (path-level
// replay needs no rebase).
package cli

import (
	"github.com/banyan-vcs/banyan/integration/support"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIStack(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	seed := support.SeedRepo(t, s, "stk") // README.md + lib/a.txt + lib/nested/SHARED.txt

	root := t.TempDir()
	ws := filepath.Join(root, "ws")

	out, errOut, code := runCLI(t, "clone", s.RepoURL("stk"), ws)
	require.Equal(t, 0, code, "clone 失败\nstdout:%s\nstderr:%s", out, errOut)

	// ① path-scoped push: only README.md is captured and reverted; the other
	// dirty paths stay in the working tree.
	support.Write(t, filepath.Join(ws, "README.md"), "readme-a\n")
	support.Write(t, filepath.Join(ws, "lib", "nested", "SHARED.txt"), "shared-b\n")
	support.Write(t, filepath.Join(ws, "top.txt"), "top\n")
	out, errOut, code = runCLI(t, "-C", ws, "stack", "push", "-m", "step one: readme", "README.md")
	require.Equal(t, 0, code, "stack push 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, out, "stacked #1")
	assert.Equal(t, "readme-v1\n", support.ReadFile(t, filepath.Join(ws, "README.md")), "选中路径应被回退")
	assert.Equal(t, "shared-b\n", support.ReadFile(t, filepath.Join(ws, "lib", "nested", "SHARED.txt")), "未选路径不动")

	// ② push the rest (no path args = whole dirty set) → clean tree.
	out, errOut, code = runCLI(t, "-C", ws, "stack", "push", "-m", "step two: shared + top")
	require.Equal(t, 0, code, "stack push 全量失败\nstdout:%s\nstderr:%s", out, errOut)
	out, _, _ = runCLI(t, "-C", ws, "status")
	assert.Contains(t, out, "clean")

	// ③ ls: two entries, oldest first.
	out, _, code = runCLI(t, "-C", ws, "stack", "ls")
	require.Equal(t, 0, code)
	assert.Less(t, strings.Index(out, "#1"), strings.Index(out, "#2"), "oldest first")
	assert.Contains(t, out, "step one: readme")

	// ④ pop: the top entry returns to the working tree, entry leaves the stack.
	out, errOut, code = runCLI(t, "-C", ws, "stack", "pop")
	require.Equal(t, 0, code, "pop 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, out, "popped #2")
	assert.Equal(t, "shared-b\n", support.ReadFile(t, filepath.Join(ws, "lib", "nested", "SHARED.txt")))
	assert.Equal(t, "top\n", support.ReadFile(t, filepath.Join(ws, "top.txt")))
	out, _, _ = runCLI(t, "-C", ws, "stack", "ls")
	assert.NotContains(t, out, "#2", "pop 后条目应离栈")

	// ⑤ re-push the popped work (untracked file included), then land all.
	out, errOut, code = runCLI(t, "-C", ws, "stack", "push", "-m", "step two again")
	require.Equal(t, 0, code, "re-push 失败\nstdout:%s\nstderr:%s", out, errOut)
	out, errOut, code = runCLI(t, "-C", ws, "stack", "commit")
	require.Equal(t, 0, code, "stack commit 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, out, "landed #1")
	assert.Contains(t, out, "landed #3")
	assert.Contains(t, out, "stack: 0 left")
	out, _, _ = runCLI(t, "-C", ws, "status")
	assert.Contains(t, out, "clean")

	// ⑥ cross-verify with a real git clone: both entries landed in order, the
	// untracked file became a tracked file, fsck clean.
	g := support.CloneRepo(t, s, s.RepoURL("stk"))
	assert.Equal(t, "readme-a\n", support.ReadFile(t, filepath.Join(g, "README.md")))
	assert.Equal(t, "shared-b\n", support.ReadFile(t, filepath.Join(g, "lib", "nested", "SHARED.txt")))
	assert.Equal(t, "top\n", support.ReadFile(t, filepath.Join(g, "top.txt")))
	require.Equal(t, 3, support.CountCommits(t, g))
	assert.Equal(t, "step two again", strings.TrimSpace(support.Git(t, g, "log", "-1", "--format=%s")))
	assert.Equal(t, "step one: readme", strings.TrimSpace(support.Git(t, g, "log", "--format=%s", "-1", "HEAD~1")))
	support.FsckClean(t, g)
	blobs, _ := os.ReadDir(filepath.Join(ws, ".banyan", "stack-blobs"))
	assert.Empty(t, blobs, "全部落地后栈 blob 库应被回收")

	// ⑦ a peer moves the head; the next stack commit conflicts on its first
	// landing (exit 1) and the entry survives verbatim. (The peer syncs
	// BEFORE editing — its commit must sit on the landed stack state.)
	support.Git(t, seed, "pull", "--rebase", "-q", s.RepoURL("stk"), "main")
	support.Write(t, filepath.Join(seed, "README.md"), "readme-peer\n")
	support.Git(t, seed, "add", "-A")
	support.Git(t, seed, "commit", "-qm", "peer: readme")
	support.Git(t, seed, "push", "-q", s.RepoURL("stk"), "main")
	support.Write(t, filepath.Join(ws, "README.md"), "readme-local\n")
	out, errOut, code = runCLI(t, "-C", ws, "stack", "push", "-m", "step three")
	require.Equal(t, 0, code, "push step three 失败\nstdout:%s\nstderr:%s", out, errOut)
	out, errOut, code = runCLI(t, "-C", ws, "stack", "commit")
	assert.Equal(t, 1, code, "过期 base 首落必须退出码 1\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, errOut, "conflict")
	out, _, _ = runCLI(t, "-C", ws, "stack", "ls")
	assert.Contains(t, out, "step three", "冲突后未落地条目应原样保留")

	// ⑧ pull onto the fresh base, retry — path-level replay needs no rebase.
	out, errOut, code = runCLI(t, "-C", ws, "pull")
	require.Equal(t, 0, code, "pull 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Equal(t, "readme-peer\n", support.ReadFile(t, filepath.Join(ws, "README.md")))
	out, errOut, code = runCLI(t, "-C", ws, "stack", "commit")
	require.Equal(t, 0, code, "pull 后重试应全落地\nstdout:%s\nstderr:%s", out, errOut)
	g2 := support.CloneRepo(t, s, s.RepoURL("stk"))
	assert.Equal(t, "readme-local\n", support.ReadFile(t, filepath.Join(g2, "README.md")))
	require.Equal(t, 5, support.CountCommits(t, g2))
	support.FsckClean(t, g2)
}
