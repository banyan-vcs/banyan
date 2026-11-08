// sync_test.go — T93: banyan sync, the explicit subtree→root alignment
// (docs/02 §12 true-divergence rule, docs/06 §3.9). The server replays the
// subtree timeline's local-only commits onto the root's current projection,
// advances the recorded fork point (Base), and CASes the head; the CLI then
// materializes via the ordinary pull. Cross-verified by a real git clone:
// both sides' changes present, every subject exactly once, fsck clean.
//
// True divergence fixture note: every SEQUENTIAL entrance is guarded away
// (thin commit → base-moved; stale subtree push → the projection guard; root
// push → the woven-tip race guard, T8 ②) — the state only exists when a root
// push lands between a subtree push's guard check and its CAS. The fixtures
// simulate that race deterministically: the objects ride an aux-branch push,
// then the root timeline is CASed directly (see racedRoot).
package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
)

// racedRoot lands seedDir's HEAD commit on the repo's root timeline although
// a subtree local already moved the woven tip — the TOCTOU shape the wire
// guards cannot close. The commit's objects ride an aux-branch push (a fresh
// ref is always accepted), the guarded main ref is then CASed directly and
// the aux timeline dropped.
func racedRoot(t *testing.T, meta meta.MetaStore, seedDir, repo, url string) {
	t.Helper()
	git(t, seedDir, "push", "-q", url, "main:refs/heads/raced-aux")
	rootTL := model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: "main"}
	auxTL := model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: "raced-aux"}
	st, merr := meta.Timeline(context.Background(), rootTL)
	require.NoError(t, merr)
	newHead := strings.TrimSpace(git(t, seedDir, "rev-parse", "HEAD"))
	require.NoError(t, meta.CASHead(context.Background(), rootTL, st.Head, model.OID(newHead)))
	require.NoError(t, meta.CASHead(context.Background(), auxTL, model.OID(newHead), ""))
}

func TestSyncSubtree(t *testing.T) {
	needGit(t)
	s, meta, _ := newServerPlane(t)
	seed := seedRepo(t, s, "syn") // README.md + lib/a.txt + lib/nested/SHARED.txt
	ctx := context.Background()
	libTL := model.Timeline{Repo: "syn", Anchor: "/lib", Name: "main"}
	rootTL := model.Timeline{Repo: "syn", Anchor: model.RootAnchor, Name: "main"}

	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	out, errOut, code := runCLI(t, "clone", s.treeURL("syn", "lib"), ws)
	require.Equal(t, 0, code, "子树 clone 失败\nstdout:%s\nstderr:%s", out, errOut)

	// ① local commit creates the /lib timeline (parent = projection).
	write(t, filepath.Join(ws, "nested", "SHARED.txt"), "local\n")
	out, errOut, code = runCLI(t, "-C", ws, "commit", "-m", "local: shared", "--name", "cli", "--email", "cli@banyan.dev")
	require.Equal(t, 0, code, "local commit 失败\nstdout:%s\nstderr:%s", out, errOut)

	// ② the race: a root peer's lib/a.txt commit lands on the root timeline
	// although the woven tip already carries the local change.
	write(t, filepath.Join(seed, "lib", "a.txt"), "root\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "peer: a")
	racedRoot(t, meta, seed, "syn", s.repoURL("syn"))

	// ③ sync: one local commit replayed onto the projection, then pulled.
	out, errOut, code = runCLI(t, "-C", ws, "sync")
	require.Equal(t, 0, code, "sync 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, out, "rebased 1 commit")
	assert.Equal(t, "root\n", readFile(t, filepath.Join(ws, "a.txt")), "sync 应物化根侧变更")
	assert.Equal(t, "local\n", readFile(t, filepath.Join(ws, "nested", "SHARED.txt")), "本地变更应保留")
	out, _, _ = runCLI(t, "-C", ws, "status")
	assert.Contains(t, out, "clean")

	// ④ the fork point advanced to the root head at sync time (the weave's
	// local zone shrinks to exactly the rebased locals).
	rootState, err := meta.Timeline(ctx, rootTL)
	require.NoError(t, err)
	libState, err := meta.Timeline(ctx, libTL)
	require.NoError(t, err)
	require.Equal(t, rootState.Head, libState.Base, "Base 应推进到同步时刻的根头")

	// ⑤ root-view cross-verification: both sides present, every subject
	// exactly once, fsck clean.
	g := cloneRepo(t, s, s.repoURL("syn"))
	assert.Equal(t, "root\n", readFile(t, filepath.Join(g, "lib", "a.txt")))
	assert.Equal(t, "local\n", readFile(t, filepath.Join(g, "lib", "nested", "SHARED.txt")))
	require.Equal(t, 3, countCommits(t, g))
	subjects := git(t, g, "log", "--format=%s")
	assert.Equal(t, 1, strings.Count(subjects, "local: shared"), "subject 应恰出现一次:\n%s", subjects)
	assert.Equal(t, 1, strings.Count(subjects, "peer: a"))
	assert.Equal(t, 1, strings.Count(subjects, "seed: initial syn"))
	fsckClean(t, g)

	// ⑥ the absorbed-content twin: a peer pulls the woven view (carrying the
	// local change into root-local history), commits outside lib, pushes —
	// the /lib projection advanced to include the local content as root
	// content; sync must recognize the state as already consistent.
	git(t, seed, "pull", "--rebase", "-q", s.repoURL("syn"), "main")
	write(t, filepath.Join(seed, "README.md"), "readme-2\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "peer: readme")
	git(t, seed, "push", "-q", s.repoURL("syn"), "main")
	out, errOut, code = runCLI(t, "-C", ws, "sync")
	require.Equal(t, 0, code, "吸收孪生 sync 失败\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, out, "nothing to align")
	out, _, code = runCLI(t, "-C", ws, "pull")
	require.Equal(t, 0, code)
	assert.Equal(t, "local\n", readFile(t, filepath.Join(ws, "nested", "SHARED.txt")), "子树工作区不应被扰动")

	// ⑦ conflict: both sides edit the same file (via the same race) — the
	// replay refuses, the timeline and Base stay untouched.
	write(t, filepath.Join(seed, "lib", "nested", "SHARED.txt"), "root-x\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "peer: shared x")
	preLocal, err := meta.Timeline(ctx, libTL)
	require.NoError(t, err)
	racedRoot(t, meta, seed, "syn", s.repoURL("syn"))
	out, errOut, code = runCLI(t, "-C", ws, "sync")
	assert.Equal(t, 1, code, "真分歧冲突应退出码 1\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, errOut, "conflict")
	assert.Contains(t, errOut, "SHARED.txt", "冲突路径应具名")
	postLocal, err := meta.Timeline(ctx, libTL)
	require.NoError(t, err)
	assert.Equal(t, preLocal.Head, postLocal.Head, "冲突后本地时间线头不得移动")
	assert.Equal(t, preLocal.Base, postLocal.Base, "冲突后 Base 不得移动")

	// ⑧ merge commits in the local chain: honest 400 (exit 2), not a conflict.
	s3, meta3, _ := newServerPlane(t)
	seed3 := seedRepo(t, s3, "synmerge")
	ws3 := filepath.Join(root, "ws3")
	out, errOut, code = runCLI(t, "clone", s3.treeURL("synmerge", "lib"), ws3)
	require.Equal(t, 0, code, out+errOut)
	write(t, filepath.Join(ws3, "a.txt"), "first\n")
	out, errOut, code = runCLI(t, "-C", ws3, "commit", "-m", "first", "--name", "cli", "--email", "cli@banyan.dev")
	require.Equal(t, 0, code, out+errOut)
	// The subtree view is at the local "first" commit; a git peer builds a
	// genuine two-parent merge on top and pushes it into the timeline.
	sub := cloneRepo(t, s3, s3.treeURL("synmerge", "lib"))
	git(t, sub, "checkout", "-q", "-b", "side")
	write(t, filepath.Join(sub, "side.txt"), "side\n")
	git(t, sub, "add", "-A")
	git(t, sub, "commit", "-qm", "side")
	git(t, sub, "checkout", "-q", "main")
	write(t, filepath.Join(sub, "other.txt"), "other\n")
	git(t, sub, "add", "-A")
	git(t, sub, "commit", "-qm", "other")
	git(t, sub, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	git(t, sub, "push", "-q", "origin", "main")
	// The race moves root inside lib; sync must reject the shape honestly.
	write(t, filepath.Join(seed3, "lib", "a.txt"), "root-m\n")
	git(t, seed3, "add", "-A")
	git(t, seed3, "commit", "-qm", "peer: a")
	racedRoot(t, meta3, seed3, "synmerge", s3.repoURL("synmerge"))
	out, errOut, code = runCLI(t, "-C", ws3, "sync")
	assert.Equal(t, 2, code, "merge 链应退出码 2（不支持的形状，非冲突）\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, errOut, "linear chains only")

	// ⑨ a root workspace has nothing to sync (exit 2).
	rootWs := filepath.Join(root, "rootws")
	out, errOut, code = runCLI(t, "clone", s.repoURL("syn"), rootWs)
	require.Equal(t, 0, code, out+errOut)
	out, errOut, code = runCLI(t, "-C", rootWs, "sync")
	assert.Equal(t, 2, code, "根工作区应退出码 2\nstdout:%s\nstderr:%s", out, errOut)
	assert.Contains(t, errOut, "root workspace")
}
