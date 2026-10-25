// T43: subtree force-push revival. Git's wire protocol cannot signal
// "force" — the client-side check is all force bypasses, and old is always
// the advertised tip. A client that fetches, rewinds locally (reset --hard
// to an old commit, or a history rewrite that drops peers' commits) and
// pushes --force therefore presents old=curTip: the staleness guards see a
// consecutive push and the timeline CAS would silently rewind the directory
// under peers' landed content. The new head must carry the current tip's
// content forward — by ancestry (fast-forward), identical X content, or the
// current X tree recorded in the pushed chain (rebase-recovery rewrite).
package integration

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revive rewinds dir to the seed commit, optionally adds one own commit on
// top, and force-pushes. The force flag only lifts the client-side
// ancestry check — the wire update still carries old=advertised tip.
func revive(t *testing.T, dir, seed string, addOwn bool) string {
	t.Helper()
	git(t, dir, "reset", "-q", "--hard", seed)
	if addOwn {
		write(t, filepath.Join(dir, "c.txt"), "by-c\n")
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-qm", "c rewinds")
	}
	out, err := gitTry(dir, "push", "--force", "origin", "main")
	if err != nil {
		out += "\nerr: " + err.Error()
	}
	t.Logf("force push output:\n%s", out)
	return out
}

func TestForcePushRevivalRejected(t *testing.T) {
	needGit(t)

	for _, tc := range []struct {
		name    string
		converge bool
		addOwn  bool
	}{
		{"adopt-plain-rewind", false, false},
		{"adopt-rewind-plus-own", false, true},
		{"converge-plain-rewind", true, false},
		{"converge-rewind-plus-own", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s *server
			if tc.converge {
				s = newConvergeServer(t)
			} else {
				s = newServer(t)
			}
			seedRepo(t, s, "t43")
			a := cloneRepo(t, s, s.treeURL("t43", "lib"))
			seed := headOf(t, a) // t0: the seed tip, before any peer lands

			// Two peers land content: t0 → t1 → t2.
			appendLine(t, filepath.Join(a, "a.txt"), "by-a\n")
			git(t, a, "commit", "-qam", "a edits a.txt")
			git(t, a, "push", "-q", "origin", "main")

			b := cloneRepo(t, s, s.treeURL("t43", "lib"))
			write(t, filepath.Join(b, "b.txt"), "by-b\n")
			git(t, b, "add", "-A")
			git(t, b, "commit", "-qm", "b adds b.txt")
			git(t, b, "push", "-q", "origin", "main")

			// The attacker fetches t2, rewinds to t0 (dropping both peers'
			// content), and force-pushes. old=advertised tip=t2 either way.
			c := cloneRepo(t, s, s.treeURL("t43", "lib"))
			require.Equal(t, "a-v1\nby-a\n", readFile(t, filepath.Join(c, "a.txt")), "前置：A 的内容已可见")
			out := revive(t, c, seed, tc.addOwn)

			require.Contains(t, out, "rejected", "回滚形态的 force push 必须被服务端拒绝:\n%s", out)
			require.Contains(t, out, "non-fast-forward", "拒绝原因应为内容回滚:\n%s", out)

			// The directory timeline never rewound: a fresh clone still sees
			// both peers' content, whether or not the attacker added own work.
			fresh := cloneRepo(t, s, s.treeURL("t43", "lib"))
			assert.Equal(t, "a-v1\nby-a\n", readFile(t, filepath.Join(fresh, "a.txt")), "A 的内容被回魂")
			assert.Equal(t, "by-b\n", readFile(t, filepath.Join(fresh, "b.txt")), "B 的内容被回魂")
			assert.NoFileExists(t, filepath.Join(fresh, "c.txt"), "被拒 push 的内容不应出现")
			fsckClean(t, fresh)
		})
	}
}

// The rejection must not over-block: a diverged client whose plain push is
// client-refused ("fetch first") may reach for --force. Its wire update
// carries old=advertised tip — the client's true base is erased — so under
// convergence the fast path would CAS the stale chain straight in, dropping
// the peer's content without the merge train ever seeing it. The rollback
// guard refuses; the legitimate route (pull --rebase, push) still flows.
func TestForcePushRevivalRecovery(t *testing.T) {
	needGit(t)
	s := newConvergeServer(t)
	seedRepo(t, s, "t43r")

	a := cloneRepo(t, s, s.treeURL("t43r", "lib"))
	c := cloneRepo(t, s, s.treeURL("t43r", "lib"))

	// c commits its own work on the seed tip but does not push yet.
	write(t, filepath.Join(c, "c.txt"), "by-c\n")
	git(t, c, "add", "-A")
	git(t, c, "commit", "-qm", "c adds c.txt")

	// a lands a1 (t0 → t1). c's chain is stale-divergent; a plain push is
	// client-refused, so c force-pushes: old=advertised t1, new=c's chain.
	appendLine(t, filepath.Join(a, "a.txt"), "by-a\n")
	git(t, a, "commit", "-qam", "a edits a.txt")
	git(t, a, "push", "-q", "origin", "main")

	out, err := gitTry(c, "push", "--force", "origin", "main")
	require.Error(t, err, "陈旧链 force push（丢 A 内容）未被拒:\n%s", out)
	require.Contains(t, out, "rejected", "拒绝形态:\n%s", out)

	// The legitimate route: rebase onto the current tip, push normally.
	out, err = gitTry(c, "pull", "-q", "--rebase", "origin", "main")
	require.NoError(t, err, "rebase 失败:\n%s", out)
	out, err = gitTry(c, "push", "origin", "main")
	require.NoError(t, err, "rebase 后正常 push 应通过:\n%s", out)

	fresh := cloneRepo(t, s, s.treeURL("t43r", "lib"))
	assert.Equal(t, "a-v1\nby-a\n", readFile(t, filepath.Join(fresh, "a.txt")), "A 的内容应在")
	assert.Equal(t, "by-c\n", readFile(t, filepath.Join(fresh, "c.txt")), "C 的工作应在")
	fsckClean(t, fresh)
}

// Root-view force push (docs/04 §6b): the root repo is the verbatim-chain
// compatibility layer, so the push itself stays git-native — no rollback
// guard refuses it. But the woven view cannot rewind either: materialized
// content is monotonic across the /.woven fold (a rewound root-local chain
// loses the race against what the weave already remembers), and directory
// timelines are untouched by construction. Force push rewrites root-local
// storage history only; no visible content ever revives.
func TestRootForcePushViewMonotonic(t *testing.T) {
	needGit(t)
	s := newConvergeServer(t)
	seedRepo(t, s, "t43root")

	// Directory-side content lands via the subtree view (the guarded path).
	a := cloneRepo(t, s, s.treeURL("t43root", "lib"))
	appendLine(t, filepath.Join(a, "a.txt"), "by-a\n")
	git(t, a, "commit", "-qam", "a edits a.txt")
	git(t, a, "push", "-q", "origin", "main")

	// Root-local content lands via a root push, then rewinds by force.
	r := cloneRepo(t, s, s.repoURL("t43root"))
	seed := headOf(t, r)
	write(t, filepath.Join(r, "README.md"), "readme-v2\n")
	git(t, r, "commit", "-qam", "r2")
	git(t, r, "push", "-q", "origin", "main")
	git(t, r, "reset", "-q", "--hard", seed)

	out, err := gitTry(r, "push", "--force", "origin", "main")
	require.NoError(t, err, "root force push 应保持 git 原生语义（verbatim 模型）:\n%s", out)

	// The woven view keeps every landed change: neither root-local nor
	// directory content rewinds. Root force push is accepted (git-native)
	// but cannot revive anything users can see.
	freshRoot := cloneRepo(t, s, s.repoURL("t43root"))
	assert.Equal(t, "readme-v2\n", readFile(t, filepath.Join(freshRoot, "README.md")),
		"根视图内容不得被 root force push 回魂（织入单调性）")
	assert.Equal(t, "a-v1\nby-a\n", readFile(t, filepath.Join(freshRoot, "lib", "a.txt")),
		"目录时间线内容不得被 root 回魂波及")
	freshLib := cloneRepo(t, s, s.treeURL("t43root", "lib"))
	assert.Equal(t, "a-v1\nby-a\n", readFile(t, filepath.Join(freshLib, "a.txt")), "子树视图内容应稳定")
	fsckClean(t, freshRoot)
	fsckClean(t, freshLib)
}
