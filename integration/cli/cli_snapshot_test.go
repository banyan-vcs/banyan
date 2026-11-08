// cli_snapshot_test.go — T91: banyan snapshot, the thin workspace's
// checkpoint/restore (docs/06). Purely local: dirty bytes are captured
// verbatim, apply restores the snapshot-time state byte for byte (dirty files
// from the snapshot mirror, clean files from the cache — with the server as
// backfill when the cache was pruned), apply refuses dirty trees without
// --force, and the base-moved note warns that pull would overwrite a restored
// state that sits on a moved base.
package cli

import (
	"github.com/banyan-vcs/banyan/integration/support"

	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLISnapshot(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "snap") // README.md + lib/a.txt + lib/nested/SHARED.txt

	root := t.TempDir()
	ws := filepath.Join(root, "ws")

	out, errOut, code := runCLI(t, "clone", s.RepoURL("snap"), ws)
	require.Equal(t, 0, code, "clone 失败\nstdout:%s\nstderr:%s", out, errOut)

	// ① clean tree → nothing to snapshot.
	out, _, code = runCLI(t, "-C", ws, "snapshot")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "nothing to snapshot")

	// ② dirty state (M + D + ??) → default-named checkpoint; the working
	// tree is NOT reverted (no stash-style side effect).
	support.Write(t, filepath.Join(ws, "README.md"), "readme-dirty\n")
	support.Write(t, filepath.Join(ws, "new.txt"), "new\n")
	require.NoError(t, os.Remove(filepath.Join(ws, "lib", "a.txt")))
	out, errOut, code = runCLI(t, "-C", ws, "snapshot", "-m", "wip readme")
	require.Equal(t, 0, code, "snapshot save 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, out, "snapshotted snap-1")
	assert.Contains(t, out, "1 modified, 1 deleted, 1 untracked")
	assert.Equal(t, "readme-dirty\n", support.ReadFile(t, filepath.Join(ws, "README.md")))
	assert.Equal(t, "new\n", support.ReadFile(t, filepath.Join(ws, "new.txt")))
	assert.NoFileExists(t, filepath.Join(ws, "lib", "a.txt"))

	// ③ ls lists the checkpoint with its message.
	out, _, code = runCLI(t, "-C", ws, "snapshot", "ls")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "snap-1")
	assert.Contains(t, out, "wip readme")

	// ④ apply refuses a dirty tree (exit 1) — restore must not clobber.
	support.Write(t, filepath.Join(ws, "README.md"), "readme-dirtier\n")
	out, errOut, code = runCLI(t, "-C", ws, "snapshot", "apply", "snap-1")
	assert.Equal(t, 1, code, "脏树上 apply 必须退出码 1\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, errOut, "conflict")

	// ⑤ --force restores exactly the snapshot-time state: the dirtier edit
	// is replaced by the captured bytes, the deleted tracked file is
	// resurrected from the cache, the untracked file comes back.
	out, errOut, code = runCLI(t, "-C", ws, "snapshot", "apply", "snap-1", "--force")
	require.Equal(t, 0, code, "force apply 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Equal(t, "readme-dirty\n", support.ReadFile(t, filepath.Join(ws, "README.md")))
	assert.Equal(t, "new\n", support.ReadFile(t, filepath.Join(ws, "new.txt")))
	assert.Equal(t, "a-v1\n", support.ReadFile(t, filepath.Join(ws, "lib", "a.txt")))
	out, _, _ = runCLI(t, "-C", ws, "status")
	assert.Contains(t, out, "M  README.md")
	assert.Contains(t, out, "?? new.txt")
	assert.NotContains(t, out, "\nclean\n")

	// ⑥ land the restored state, prune the cache, then apply the snapshot
	// again on a clean tree: clean-at-snapshot files must come back via the
	// server backfill, and the base-moved note must fire (the manifest
	// regresses to the snapshot's older view).
	out, errOut, code = runCLI(t, "-C", ws, "commit", "-m", "snap state", "--name", "cli", "--email", "cli@banyan.dev")
	require.Equal(t, 0, code, "commit 失败\nstdout:%s\nstderr:%s", out, errOut)
	require.NoError(t, os.RemoveAll(filepath.Join(ws, ".banyan", "cache")))
	out, errOut, code = runCLI(t, "-C", ws, "snapshot", "apply", "snap-1")
	require.Equal(t, 0, code, "干净树上 apply 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Equal(t, "base\n", support.ReadFile(t, filepath.Join(ws, "lib", "nested", "SHARED.txt")), "缓存回填应从服务端取回干净文件")
	assert.Equal(t, "readme-dirty\n", support.ReadFile(t, filepath.Join(ws, "README.md")))
	assert.Contains(t, errOut, "base moved")

	// ⑦ drop removes the checkpoint; apply on a dropped name is exit 2.
	out, errOut, code = runCLI(t, "-C", ws, "snapshot", "drop", "snap-1")
	require.Equal(t, 0, code, "drop 失败\nstdout:%s\nstderr:%s", out, errOut)
	out, _, _ = runCLI(t, "-C", ws, "snapshot", "ls")
	assert.Contains(t, out, "no snapshots")
	out, errOut, code = runCLI(t, "-C", ws, "snapshot", "apply", "snap-1")
	assert.Equal(t, 2, code, "不存在的快照应退出码 2\nstdout:%s\nstderr:%s", out, errOut)
}
