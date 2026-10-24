// cli_workspace_test.go — T22: the thin-workspace CLI end to end (docs/05):
// minimal clone (no .git, no history), offline status, server-side commit
// assembly, cross-verified by a REAL git client (fresh clone + fsck), pull
// as a server-side tree-diff, remote log/cat, and the base-moved conflict.
// T22b: the same loop on a subtree workspace (/tree/<dir>).
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/cli"
	"github.com/banyan-vcs/banyan/internal/rpc"
)

// runCLI runs the thin client in-process; returns stdout, stderr and the exit code.
func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run(args, &out, &errb)
	return out.String(), errb.String(), code
}

func TestCLIThinWorkspace(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seed := seedRepo(t, s, "ws") // README.md + lib/a.txt + lib/nested/SHARED.txt

	root := t.TempDir()
	ws := filepath.Join(root, "ws")

	// ① minimal clone: files materialize, no .git, no history downloaded.
	out, errOut, code := runCLI(t, "clone", s.repoURL("ws"), ws)
	require.Equal(t, 0, code, "clone 退出码非 0\nstdout:%s\nstderr:%s", out, errOut)
	assert.Equal(t, "readme-v1\n", readFile(t, filepath.Join(ws, "README.md")))
	assert.Equal(t, "a-v1\n", readFile(t, filepath.Join(ws, "lib", "a.txt")))
	assert.NoFileExists(t, filepath.Join(ws, ".git"), "薄工作区不应有 .git")
	assert.FileExists(t, filepath.Join(ws, ".banyan", "manifest.json"))
	numCached := func() int {
		entries, _ := os.ReadDir(filepath.Join(ws, ".banyan", "cache"))
		return len(entries)
	}()
	assert.Less(t, numCached, 10, "微型仓库缓存对象应为个位数（blob+树，无提交闭包）")

	// ② offline status: clean.
	out, _, code = runCLI(t, "-C", ws, "status")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "clean")

	// ③ edits: modify / add / delete, then status lists them.
	write(t, filepath.Join(ws, "README.md"), "readme-v2\n")
	write(t, filepath.Join(ws, "top.txt"), "top\n")
	require.NoError(t, os.Remove(filepath.Join(ws, "lib", "a.txt")))
	out, _, _ = runCLI(t, "-C", ws, "status")
	assert.Contains(t, out, "M  README.md")
	assert.Contains(t, out, "D  lib/a.txt")
	assert.Contains(t, out, "?? top.txt")

	// ④ server-side commit assembly.
	out, errOut, code = runCLI(t, "-C", ws, "commit", "-m", "cli: v2", "--name", "cli", "--email", "cli@banyan.dev")
	require.Equal(t, 0, code, "commit 失败\nstdout:%s\nstderr:%s", out, errOut)

	// ⑤ cross-verification by a real git client: fresh clone sees the change.
	g := cloneRepo(t, s, s.repoURL("ws"))
	assert.Equal(t, "readme-v2\n", readFile(t, filepath.Join(g, "README.md")))
	assert.Equal(t, "top\n", readFile(t, filepath.Join(g, "top.txt")))
	assert.NoFileExists(t, filepath.Join(g, "lib", "a.txt"))
	assert.Equal(t, "cli: v2", strings.TrimSpace(git(t, g, "log", "-1", "--format=%s")))
	assert.Equal(t, 2, countCommits(t, g))
	require.NotContains(t, git(t, g, "fsck", "--strict"), "error", "fsck 应无错误")
	require.NotContains(t, git(t, g, "fsck", "--strict"), "missing")

	// ⑥ remote log / cat: zero download, hashes match the git view.
	out, errOut, code = runCLI(t, "-C", ws, "log", "-n", "10")
	require.Equal(t, 0, code, "log 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, out, "cli: v2")
	assert.Contains(t, out, "seed: initial ws")
	out, errOut, code = runCLI(t, "-C", ws, "cat", "HEAD:README.md")
	require.Equal(t, 0, code, "cat 失败\nstdout:%q\nstderr:%q", out, errOut)
	assert.Equal(t, "readme-v2\n", out)
	out, _, _ = runCLI(t, "-C", ws, "cat", "README.md")
	assert.Equal(t, "readme-v2\n", out, "无 rev 前缀应等价 HEAD")

	// ⑦ pull: a git peer pushes; the CLI downloads only the difference.
	write(t, filepath.Join(seed, "lib", "nested", "SHARED.txt"), "shared-v2\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "peer: shared v2")
	git(t, seed, "pull", "--rebase", "-q", s.repoURL("ws"), "main") // server holds the CLI commit
	git(t, seed, "push", "-q", s.repoURL("ws"), "main")
	out, errOut, code = runCLI(t, "-C", ws, "pull")
	require.Equal(t, 0, code, "pull 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Equal(t, "shared-v2\n", readFile(t, filepath.Join(ws, "lib", "nested", "SHARED.txt")))
	out, _, _ = runCLI(t, "-C", ws, "status")
	assert.Contains(t, out, "clean")

	// ⑧ base-moved conflict: the peer advances the head again; the stale
	// workspace commit is rejected with exit 1, pull resolves it.
	write(t, filepath.Join(seed, "README.md"), "readme-peer-v3\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "peer: readme v3")
	git(t, seed, "pull", "--rebase", "-q", s.repoURL("ws"), "main")
	git(t, seed, "push", "-q", s.repoURL("ws"), "main")
	write(t, filepath.Join(ws, "README.md"), "readme-local\n")
	out, errOut, code = runCLI(t, "-C", ws, "commit", "-m", "stale", "--name", "cli", "--email", "cli@banyan.dev")
	assert.Equal(t, 1, code, "过期 base 必须退出码 1（stdout:%s stderr:%s）", out, errOut)
	assert.Contains(t, errOut, "conflict")
	// The failed commit left a local modification: restore to the committed
	// state (thin pull refuses dirty trees, docs/05 §3.1), then pull.
	write(t, filepath.Join(ws, "README.md"), "readme-v2\n")
	out, errOut, code = runCLI(t, "-C", ws, "pull")
	require.Equal(t, 0, code, "冲突后 pull 应成功\nstdout:%s\nstderr:%s", out, errOut)
	assert.Equal(t, "readme-peer-v3\n", readFile(t, filepath.Join(ws, "README.md")))
	write(t, filepath.Join(ws, "README.md"), "readme-local-v2\n")
	out, errOut, code = runCLI(t, "-C", ws, "commit", "-m", "cli: v4", "--name", "cli", "--email", "cli@banyan.dev")
	require.Equal(t, 0, code, "pull 后重提交通知失败\nstdout:%s\nstderr:%s", out, errOut)
	g2 := cloneRepo(t, s, s.repoURL("ws"))
	assert.Equal(t, "readme-local-v2\n", readFile(t, filepath.Join(g2, "README.md")))

	// ⑨ refs listing (branch + annotated tag pushed by git) and repo ls.
	git(t, seed, "tag", "-a", "v1.0", "-m", "release")
	git(t, seed, "push", "-q", s.repoURL("ws"), "v1.0")
	out, _, code = runCLI(t, "-C", ws, "branch")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "main")
	out, _, code = runCLI(t, "-C", ws, "tag")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "v1.0")
	out, _, code = runCLI(t, "-C", ws, "repo", "ls")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "ws")
}

func TestCLISubtreeWorkspace(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "sub")

	root := t.TempDir()
	ws := filepath.Join(root, "nested")

	// Clone the /tree/lib virtual repo: the workspace root IS the subtree.
	out, errOut, code := runCLI(t, "clone", s.treeURL("sub", "lib"), ws)
	require.Equal(t, 0, code, "子树 clone 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Equal(t, "a-v1\n", readFile(t, filepath.Join(ws, "a.txt")))
	assert.Equal(t, "base\n", readFile(t, filepath.Join(ws, "nested", "SHARED.txt")))
	assert.NoFileExists(t, filepath.Join(ws, "README.md"), "子树工作区只含子目录内容")
	assert.NoFileExists(t, filepath.Join(ws, ".git"))

	// Commit inside the subtree timeline; the ROOT view picks it up via weave.
	write(t, filepath.Join(ws, "a.txt"), "a-v2\n")
	out, errOut, code = runCLI(t, "-C", ws, "commit", "-m", "subtree: a v2", "--name", "cli", "--email", "cli@banyan.dev")
	require.Equal(t, 0, code, "子树 commit 失败\nstdout:%s\nstderr:%s", out, errOut)

	g := cloneRepo(t, s, s.repoURL("sub"))
	assert.Equal(t, "a-v2\n", readFile(t, filepath.Join(g, "lib", "a.txt")), "根视图应织入子树提交")
	assert.Equal(t, "readme-v1\n", readFile(t, filepath.Join(g, "README.md")))
	assert.Contains(t, git(t, g, "log", "--format=%s"), "subtree: a v2")

	// Subtree log defaults to the directory's own history.
	out, _, code = runCLI(t, "-C", ws, "log", "-n", "5")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "subtree: a v2")

	// A root-repo peer push still flows into the subtree workspace via pull.
	seed := filepath.Join(t.TempDir(), "peer")
	git(t, "", "clone", "-q", s.repoURL("sub"), seed)
	write(t, filepath.Join(seed, "lib", "nested", "SHARED.txt"), "root-peer\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "root peer change")
	git(t, seed, "push", "-q", "origin", "main")
	out, errOut, code = runCLI(t, "-C", ws, "pull")
	require.Equal(t, 0, code, "子树 pull 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Equal(t, "root-peer\n", readFile(t, filepath.Join(ws, "nested", "SHARED.txt")))
}

// --- T33 fetch-trees binary framing (docs/10 §3.3; docs/12 P1-2) ---
// The thin CLI negotiates "format":"bin" — tree/blob bytes travel as binary
// records instead of JSON base64 (a flat +33%). The JSON response stays for
// compatibility. Both framings must be semantically identical, and the
// binary one measurably smaller.
func TestFetchTreesBinary(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t33")
	write(t, filepath.Join(src, "lib", "bulk.txt"), strings.Repeat("payload line for framing test\n", 512))
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t33: bulk")
	git(t, src, "push", "-q", s.repoURL("t33"), "main")

	fetch := func(format string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"repo":"t33","format":%q}`, format)
		resp, err := http.Post(s.URL+"/api/v1/fetch-trees", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return len(raw), resp.Header.Get("Content-Type")
	}
	binLen, binCT := fetch("bin")
	jsonLen, _ := fetch("")

	require.Contains(t, binCT, "application/x-banyan-fetch-trees", "bin 请求应返回二进制 content-type")
	assert.Less(t, binLen, jsonLen, "二进制帧应小于 JSON base64（%d vs %d）", binLen, jsonLen)

	// Semantics: parse both, compare the object sets.
	resp, err := http.Post(s.URL+"/api/v1/fetch-trees", "application/json",
		strings.NewReader(`{"repo":"t33","format":"bin"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	branch, head, treeOID, full, pending, trees, blobs, perr := rpc.ParseFetchBinary(resp.Body)
	require.NoError(t, perr)

	var jr struct {
		Branch string            `json:"branch"`
		Head   string            `json:"head"`
		Tree   string            `json:"tree"`
		Full   bool              `json:"full"`
		Trees  map[string][]byte `json:"trees"`
		Blobs  map[string][]byte `json:"blobs"`
		Pending []string         `json:"pending"`
	}
	jresp, err := http.Post(s.URL+"/api/v1/fetch-trees", "application/json", strings.NewReader(`{"repo":"t33"}`))
	require.NoError(t, err)
	defer jresp.Body.Close()
	require.NoError(t, json.NewDecoder(jresp.Body).Decode(&jr))

	assert.Equal(t, jr.Branch, branch)
	assert.Equal(t, jr.Head, head)
	assert.Equal(t, jr.Tree, treeOID)
	assert.Equal(t, jr.Full, full)
	assert.Equal(t, jr.Pending, pending)
	assert.Equal(t, jr.Trees, trees, "树集合应与 JSON 完全一致")
	assert.Equal(t, jr.Blobs, blobs, "blob 集合应与 JSON 完全一致")
	assert.NotEmpty(t, trees)

	// The real CLI consumes the binary path end to end: clone materializes
	// through fetchTrees with format=bin (in-process runner, same as T22).
	ws := filepath.Join(t.TempDir(), "ws")
	stdout, stderr, code := runCLI(t, "clone", s.repoURL("t33"), ws)
	require.Zero(t, code, "clone 失败: %s %s", stdout, stderr)
	assert.Equal(t, strings.Repeat("payload line for framing test\n", 512),
		readFile(t, filepath.Join(ws, "lib", "bulk.txt")), "CLI 二进制路径物化内容")
}
