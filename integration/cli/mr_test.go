// cli_mr_test.go — the mr command family end to end (docs/client/cli.md
// §3.10): the thin workspace drives the lifecycle — open (answering the
// shareable /-/merge_requests URL), ls with the state filter, show with
// live mergeability and the landed record, close/reopen, merge —
// cross-verified by real git (the advertised main IS the OID the CLI
// prints). The dev plane's single identity pins the author-self-approval
// refusal, and the argument/lookup failures pin the exit codes.
package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
)

func TestCLIMR(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	seed := support.SeedRepo(t, s, "mrc") // README.md + lib/a.txt + lib/nested/SHARED.txt on main

	// feat: one real commit ahead, through the git face.
	support.Git(t, seed, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(seed, "feat.txt"), "feat work\n")
	support.Git(t, seed, "add", "-A")
	support.Git(t, seed, "commit", "-qm", "feat")
	support.Git(t, seed, "push", "-q", s.RepoURL("mrc"), "feat")

	ws := filepath.Join(t.TempDir(), "ws")
	out, errOut, code := runCLI(t, "clone", s.RepoURL("mrc"), ws)
	require.Equal(t, 0, code, "clone 失败\nstdout:%s\nstderr:%s", out, errOut)

	// open answers the IID and the shareable web URL.
	out, errOut, code = runCLI(t, "-C", ws, "mr", "open", "feat", "main", "add", "feat", "work")
	require.Equal(t, 0, code, "mr open 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, out, "!1")
	assert.Contains(t, out, s.URL+"/r/mrc/-/merge_requests/1", "open 应答可分享 URL")

	// ls lists it; the state filters narrow it.
	out, _, code = runCLI(t, "-C", ws, "mr", "ls")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "!1")
	assert.Contains(t, out, "feat → main")
	out, _, code = runCLI(t, "-C", ws, "mr", "ls", "open")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "add feat work")

	// show answers live mergeability.
	out, _, code = runCLI(t, "-C", ws, "mr", "show", "1")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "mergeable: no conflicts")

	// The dev plane has one identity — the author cannot self-approve.
	out, errOut, code = runCLI(t, "-C", ws, "mr", "approve", "1")
	assert.Equal(t, 2, code, "自批应退出码 2\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, errOut, "author")

	// close hides it from the open list, blocks the merge; reopen restores.
	out, errOut, code = runCLI(t, "-C", ws, "mr", "close", "1")
	require.Equal(t, 0, code, "mr close 失败\nstdout:%s\nstderr:%s", out, errOut)
	out, _, _ = runCLI(t, "-C", ws, "mr", "ls", "open")
	assert.NotContains(t, out, "!1", "closed MR 不在 open 列表")
	out, _, _ = runCLI(t, "-C", ws, "mr", "ls", "closed")
	assert.Contains(t, out, "!1")
	out, errOut, code = runCLI(t, "-C", ws, "mr", "merge", "1")
	assert.Equal(t, 2, code, "closed MR 合并应退出码 2\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, errOut, "closed")
	out, errOut, code = runCLI(t, "-C", ws, "mr", "reopen", "1")
	require.Equal(t, 0, code, "mr reopen 失败\nstdout:%s\nstderr:%s", out, errOut)

	// merge lands; the printed OID is what the advertised main resolves to.
	out, errOut, code = runCLI(t, "-C", ws, "mr", "merge", "1")
	require.Equal(t, 0, code, "mr merge 失败\nstdout:%s\nstderr:%s", out, errOut)
	mergeOID := ""
	for _, ln := range strings.Split(out, "\n") {
		if fields := strings.Fields(ln); len(fields) == 4 && fields[1] == "merged" && fields[2] == "as" {
			mergeOID = fields[3]
		}
	}
	require.NotEmpty(t, mergeOID, "merge 应打印落地的 merge commit: %s", out)
	remote := support.Git(t, t.TempDir(), "ls-remote", s.RepoURL("mrc"), "refs/heads/main")
	assert.Contains(t, remote, mergeOID, "通告头必须等于记录的 merge commit")
	g := support.CloneRepo(t, s, s.RepoURL("mrc"))
	assert.Equal(t, "feat work\n", support.ReadFile(t, filepath.Join(g, "feat.txt")), "合并后的 clone 应携带 feat 内容")

	// show renders the landed record; ls agrees on the state.
	out, _, code = runCLI(t, "-C", ws, "mr", "show", "1")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "merged as "+mergeOID)
	assert.Contains(t, out, "by local")
	out, _, _ = runCLI(t, "-C", ws, "mr", "ls", "merged")
	assert.Contains(t, out, "!1")

	// Argument and lookup failures: usage exits 2, an unknown MR too.
	for _, args := range [][]string{
		{"mr"},
		{"mr", "bogus"},
		{"mr", "show", "abc"},
		{"mr", "merge"},
		{"mr", "show", "99"},
	} {
		out, errOut, code = runCLI(t, append([]string{"-C", ws}, args...)...)
		assert.Equal(t, 2, code, "mr %v 应退出码 2\nstdout:%s\nstderr:%s", args, out, errOut)
		assert.NotEmpty(t, errOut)
	}
}
