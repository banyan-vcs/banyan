// T43: subtree force-push revival. Git's wire protocol cannot signal
// "force" — the client-side check is all force bypasses, and old is always
// the advertised tip. A client that fetches, rewinds locally (reset --hard
// to an old commit, or a history rewrite that drops peers' commits) and
// pushes --force therefore presents old=curTip: the staleness guards see a
// consecutive push and the timeline CAS would silently rewind the directory
// under peers' landed content. The new head must carry the current tip's
// content forward — by ancestry (fast-forward), identical X content, or the
// current X tree recorded in the pushed chain (rebase-recovery rewrite).
package protocol

import (
	"github.com/banyan-vcs/banyan/integration/support"

	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revive rewinds dir to the seed commit, optionally adds one own commit on
// top, and force-pushes. The force flag only lifts the client-side
// ancestry check — the wire update still carries old=advertised tip.
func revive(t *testing.T, dir, seed string, addOwn bool) string {
	t.Helper()
	support.Git(t, dir, "reset", "-q", "--hard", seed)
	if addOwn {
		support.Write(t, filepath.Join(dir, "c.txt"), "by-c\n")
		support.Git(t, dir, "add", "-A")
		support.Git(t, dir, "commit", "-qm", "c rewinds")
	}
	out, err := support.GitTry(dir, "push", "--force", "origin", "main")
	if err != nil {
		out += "\nerr: " + err.Error()
	}
	t.Logf("force push output:\n%s", out)
	return out
}

func TestForcePushRevivalRejected(t *testing.T) {
	support.NeedGit(t)

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
			var s *support.Server
			if tc.converge {
				s = newConvergeServer(t)
			} else {
				s = support.NewServer(t)
			}
			support.SeedRepo(t, s, "t43")
			a := support.CloneRepo(t, s, s.TreeURL("t43", "lib"))
			seed := support.HeadOf(t, a) // t0: the seed tip, before any peer lands

			// Two peers land content: t0 → t1 → t2.
			support.AppendLine(t, filepath.Join(a, "a.txt"), "by-a\n")
			support.Git(t, a, "commit", "-qam", "a edits a.txt")
			support.Git(t, a, "push", "-q", "origin", "main")

			b := support.CloneRepo(t, s, s.TreeURL("t43", "lib"))
			support.Write(t, filepath.Join(b, "b.txt"), "by-b\n")
			support.Git(t, b, "add", "-A")
			support.Git(t, b, "commit", "-qm", "b adds b.txt")
			support.Git(t, b, "push", "-q", "origin", "main")

			// The attacker fetches t2, rewinds to t0 (dropping both peers'
			// content), and force-pushes. old=advertised tip=t2 either way.
			c := support.CloneRepo(t, s, s.TreeURL("t43", "lib"))
			require.Equal(t, "a-v1\nby-a\n", support.ReadFile(t, filepath.Join(c, "a.txt")), "前置：A 的内容已可见")
			out := revive(t, c, seed, tc.addOwn)

			require.Contains(t, out, "rejected", "回滚形态的 force push 必须被服务端拒绝:\n%s", out)
			require.Contains(t, out, "non-fast-forward", "拒绝原因应为内容回滚:\n%s", out)

			// The directory timeline never rewound: a fresh clone still sees
			// both peers' content, whether or not the attacker added own work.
			fresh := support.CloneRepo(t, s, s.TreeURL("t43", "lib"))
			assert.Equal(t, "a-v1\nby-a\n", support.ReadFile(t, filepath.Join(fresh, "a.txt")), "A 的内容被回魂")
			assert.Equal(t, "by-b\n", support.ReadFile(t, filepath.Join(fresh, "b.txt")), "B 的内容被回魂")
			assert.NoFileExists(t, filepath.Join(fresh, "c.txt"), "被拒 push 的内容不应出现")
			support.FsckClean(t, fresh)
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
	support.NeedGit(t)
	s := newConvergeServer(t)
	support.SeedRepo(t, s, "t43r")

	a := support.CloneRepo(t, s, s.TreeURL("t43r", "lib"))
	c := support.CloneRepo(t, s, s.TreeURL("t43r", "lib"))

	// c commits its own work on the seed tip but does not push yet.
	support.Write(t, filepath.Join(c, "c.txt"), "by-c\n")
	support.Git(t, c, "add", "-A")
	support.Git(t, c, "commit", "-qm", "c adds c.txt")

	// a lands a1 (t0 → t1). c's chain is stale-divergent; a plain push is
	// client-refused, so c force-pushes: old=advertised t1, new=c's chain.
	support.AppendLine(t, filepath.Join(a, "a.txt"), "by-a\n")
	support.Git(t, a, "commit", "-qam", "a edits a.txt")
	support.Git(t, a, "push", "-q", "origin", "main")

	out, err := support.GitTry(c, "push", "--force", "origin", "main")
	require.Error(t, err, "陈旧链 force push（丢 A 内容）未被拒:\n%s", out)
	require.Contains(t, out, "rejected", "拒绝形态:\n%s", out)

	// The legitimate route: rebase onto the current tip, push normally.
	out, err = support.GitTry(c, "pull", "-q", "--rebase", "origin", "main")
	require.NoError(t, err, "rebase 失败:\n%s", out)
	out, err = support.GitTry(c, "push", "origin", "main")
	require.NoError(t, err, "rebase 后正常 push 应通过:\n%s", out)

	fresh := support.CloneRepo(t, s, s.TreeURL("t43r", "lib"))
	assert.Equal(t, "a-v1\nby-a\n", support.ReadFile(t, filepath.Join(fresh, "a.txt")), "A 的内容应在")
	assert.Equal(t, "by-c\n", support.ReadFile(t, filepath.Join(fresh, "c.txt")), "C 的工作应在")
	support.FsckClean(t, fresh)
}

// Root-view force push (docs/04 §6b): the root repo is the verbatim-chain
// compatibility layer, so the push itself stays git-native — no rollback
// guard refuses it. But the woven view cannot rewind either: materialized
// content is monotonic across the /.woven fold (a rewound root-local chain
// loses the race against what the weave already remembers), and directory
// timelines are untouched by construction. Force push rewrites root-local
// storage history only; no visible content ever revives.
func TestRootForcePushViewMonotonic(t *testing.T) {
	support.NeedGit(t)
	s := newConvergeServer(t)
	support.SeedRepo(t, s, "t43root")

	// Directory-side content lands via the subtree view (the guarded path).
	a := support.CloneRepo(t, s, s.TreeURL("t43root", "lib"))
	support.AppendLine(t, filepath.Join(a, "a.txt"), "by-a\n")
	support.Git(t, a, "commit", "-qam", "a edits a.txt")
	support.Git(t, a, "push", "-q", "origin", "main")

	// Root-local content lands via a root push, then rewinds by force.
	r := support.CloneRepo(t, s, s.RepoURL("t43root"))
	seed := support.HeadOf(t, r)
	support.Write(t, filepath.Join(r, "README.md"), "readme-v2\n")
	support.Git(t, r, "commit", "-qam", "r2")
	support.Git(t, r, "push", "-q", "origin", "main")
	support.Git(t, r, "reset", "-q", "--hard", seed)

	out, err := support.GitTry(r, "push", "--force", "origin", "main")
	require.NoError(t, err, "root force push 应保持 git 原生语义（verbatim 模型）:\n%s", out)

	// The woven view keeps every landed change: neither root-local nor
	// directory content rewinds. Root force push is accepted (git-native)
	// but cannot revive anything users can see.
	freshRoot := support.CloneRepo(t, s, s.RepoURL("t43root"))
	assert.Equal(t, "readme-v2\n", support.ReadFile(t, filepath.Join(freshRoot, "README.md")),
		"根视图内容不得被 root force push 回魂（织入单调性）")
	assert.Equal(t, "a-v1\nby-a\n", support.ReadFile(t, filepath.Join(freshRoot, "lib", "a.txt")),
		"目录时间线内容不得被 root 回魂波及")
	freshLib := support.CloneRepo(t, s, s.TreeURL("t43root", "lib"))
	assert.Equal(t, "a-v1\nby-a\n", support.ReadFile(t, filepath.Join(freshLib, "a.txt")), "子树视图内容应稳定")
	support.FsckClean(t, freshRoot)
	support.FsckClean(t, freshLib)
}

// --- T44: opt-in force push (repo-level switch) with woven-view rebuild ---
// The switch is an explicit administrative opt-in: off (default) the T43
// rollback guard stands; on, a rewind-shaped subtree push lands, the /.woven
// family resets, and every ancestor scope re-weaves from the new source
// chains — the rewound directory's commits vanish from parent/root history
// (rebuild), while independent timelines below it survive untouched.

func setForcePush(t *testing.T, s *support.Server, repo string, enabled bool) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, s.URL+"/api/v1/repos/"+repo+"/force-push",
		strings.NewReader(`{"enabled":`+strconv.FormatBool(enabled)+`}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "force-push 开关设置")
}

func TestForcePushOptIn(t *testing.T) {
	support.NeedGit(t)
	s := newConvergeServer(t)
	support.SeedRepo(t, s, "t44")

	// Two scopes of subtree activity: lib (to be rewound) and lib/nested
	// (an independent timeline that must survive the rewind).
	a := support.CloneRepo(t, s, s.TreeURL("t44", "lib"))
	seed := support.HeadOf(t, a)
	support.AppendLine(t, filepath.Join(a, "a.txt"), "by-a\n")
	support.Git(t, a, "commit", "-qam", "a edits a.txt")
	support.Git(t, a, "push", "-q", "origin", "main")

	n := support.CloneRepo(t, s, s.TreeURL("t44", "lib/nested"))
	support.AppendLine(t, filepath.Join(n, "SHARED.txt"), "by-n\n")
	support.Git(t, n, "commit", "-qam", "n adds nested")
	support.Git(t, n, "push", "-q", "origin", "main")

	// Switch OFF (default): the rewind is refused — T43 semantics unchanged.
	c := support.CloneRepo(t, s, s.TreeURL("t44", "lib"))
	out := revive(t, c, seed, false)
	require.Contains(t, out, "rejected", "开关关闭时回滚 force push 必须仍被拒:\n%s", out)

	// Opt in via the management RPC; GET reflects it.
	setForcePush(t, s, "t44", true)
	resp, err := http.Get(s.URL + "/api/v1/repos/t44/force-push")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"enabled":true`, "GET 应回报开关状态: %s", body)

	// Retry the rewind: it lands, and the woven views rebuild.
	out = revive(t, c, seed, false)
	require.NotContains(t, out, "rejected", "开关开启后回滚 force push 应落地:\n%s", out)
	require.Contains(t, out, "forced update", "落地形态:\n%s", out)

	// Subtree view: A's change gone (this timeline rewound); the independent
	// nested timeline survives (timeline boundary = rewind boundary).
	freshLib := support.CloneRepo(t, s, s.TreeURL("t44", "lib"))
	assert.Equal(t, "a-v1\n", support.ReadFile(t, filepath.Join(freshLib, "a.txt")), "lib 时间线回滚应生效")
	assert.Equal(t, "base\nby-n\n", support.ReadFile(t, filepath.Join(freshLib, "nested", "SHARED.txt")),
		"下层独立时间线内容必须保留")

	// Root view rebuilt: A's woven commit is GONE from root history, content
	// rewound, nested's weave preserved.
	freshRoot := support.CloneRepo(t, s, s.RepoURL("t44"))
	assert.Equal(t, "a-v1\n", support.ReadFile(t, filepath.Join(freshRoot, "lib", "a.txt")), "根视图内容应回滚")
	rootLog := support.Git(t, freshRoot, "log", "--format=%s")
	assert.NotContains(t, rootLog, "a edits a.txt", "根视图历史应重建（被回滚提交的织入体消失）")
	assert.Contains(t, rootLog, "n adds nested", "下层时间线织入应保留")
	support.FsckClean(t, freshRoot)
	support.FsckClean(t, freshLib)

	// Post-rewind collaboration flows: a normal push builds on the rewound
	// state with no guard friction.
	support.AppendLine(t, filepath.Join(freshLib, "a.txt"), "after-rewind\n")
	support.Git(t, freshLib, "commit", "-qam", "after rewind")
	support.Git(t, freshLib, "push", "-q", "origin", "main")
	after := support.CloneRepo(t, s, s.TreeURL("t44", "lib"))
	assert.Equal(t, "a-v1\nafter-rewind\n", support.ReadFile(t, filepath.Join(after, "a.txt")), "回滚后协作应恢复")
	support.FsckClean(t, after)

	// Switch back off: the guard re-arms (runtime toggle, no restart).
	setForcePush(t, s, "t44", false)
	out = revive(t, after, seed, false)
	require.Contains(t, out, "rejected", "开关关闭后守卫应重新生效:\n%s", out)
}
