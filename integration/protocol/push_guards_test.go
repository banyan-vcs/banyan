// Goal: server-side staleness guards and negotiation — v1 have negotiation
// final-state ACK semantics, and the protocol-level race-window protection
// behind sameXView (T8b/T9).
package protocol

import (
	"github.com/banyan-vcs/banyan/integration/support"

	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- T9 stale clone pull (v1 have negotiation) + second pull ---
// Regression: final-state ACK semantics — negotiation deadlock (remote hung up)
// and bad pack header both hit this path; own pull after push must not refetch.
func TestNegotiationPullStaleClone(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t9")

	a := support.CloneRepo(t, s, s.RepoURL("t9")) // stale clone holding all objects
	b := support.CloneRepo(t, s, s.RepoURL("t9"))

	support.Write(t, filepath.Join(b, "neg.txt"), "neg\n")
	support.Git(t, b, "add", "-A")
	support.Git(t, b, "commit", "-qm", "t9: advance from b")
	support.Git(t, b, "push", "-q", "origin", "main")

	// a's pull goes through have negotiation (ACK/NAK final-state semantics).
	require.Contains(t, support.Git(t, a, "pull"), "Fast-forward", "过期克隆 pull 未快进")
	assert.Equal(t, support.HeadOf(t, b), support.HeadOf(t, a), "pull 后 head 不一致")
	// Second pull: up to date.
	require.Contains(t, support.Git(t, a, "pull"), "Already up to date", "二次 pull 应 up to date")
}

// --- T8b server-side sameXView: force pushes bypass client checks, so the server must judge ---

// pktLine encodes one pkt-line (protocol test helper).
func pktLine(s string) string {
	return fmt.Sprintf("%04x", len(s)+4) + s
}

// rawPush hand-crafts a receive-pack POST (protocol-level, deterministic
// race-window simulation): another push landed after the client took the
// advertisement, so old is the stale advertised value. The pack section is
// empty (objects must already exist server-side, see uploadObjects).
// Returns the raw report-status body.
func rawPush(t *testing.T, s *support.Server, rawurl, old, newOID, ref string) string {
	t.Helper()
	cmd := pktLine(fmt.Sprintf("%s %s %s\x00report-status", old, newOID, ref)) + "0000"
	req, err := http.NewRequest("POST", rawurl+"/git-receive-pack", strings.NewReader(cmd))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// uploadObjects uploads the objects of a commit from dir into the root repo
// (via a temporary branch) without touching root main. Precondition for
// rawPush: the objects must exist server-side.
func uploadObjects(t *testing.T, s *support.Server, repoName, dir, commitish string) {
	t.Helper()
	support.Git(t, dir, "push", "-q", s.RepoURL(repoName), commitish+":refs/heads/upload")
	support.Git(t, dir, "push", "-q", s.RepoURL(repoName), ":refs/heads/upload") // delete after use
}

// --- T8b race-window stale-push protection (server-side sameXView) ---
//
// Key protocol fact (verified by audit): the old value a git push sends is
// always the current advertised value — force only lifts the client-side
// ancestry check, it does not change old. Hence sameXView's real line of
// the force path but the race window (another push lands between the client's
// advertisement fetch and its POST → old ≠ curTip) and non-conforming clients.
// This case constructs that window deterministically via rawPush:
//
//	reject path: after X content diverges, a push with stale old must be
//	rejected (else the whole subtree silently rolls back, losing the peer's
//	commits);
//	allow path: root-side changes outside X leave the X view unchanged —
//	stale old passes (unrelated changes must not block collaboration).
func TestRaceWindowStalePushProtection(t *testing.T) {
	support.NeedGit(t)

	// ---- reject path ----
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t8br")
	s1 := support.CloneRepo(t, s, s.TreeURL("t8br", "lib/nested"))
	s2 := support.CloneRepo(t, s, s.TreeURL("t8br", "lib/nested"))
	t0 := strings.TrimSpace(support.Git(t, s2, "rev-parse", "origin/main"))

	support.AppendLine(t, filepath.Join(s1, "SHARED.txt"), "line-1\n")
	support.Git(t, s1, "commit", "-qam", "t8br: s1 wins")
	support.Git(t, s1, "push", "-q", "origin", "main")

	support.AppendLine(t, filepath.Join(s2, "SHARED.txt"), "line-2-divergent\n")
	support.Git(t, s2, "commit", "-qam", "t8br: s2 stale")
	c2 := support.HeadOf(t, s2)
	uploadObjects(t, s, "t8br", s2, c2)

	// Race window: s2's old stays at t0 (s1 landed after the advertisement) → must reject.
	report := rawPush(t, s, s.TreeURL("t8br", "lib/nested"), t0, c2, "refs/heads/main")
	require.Contains(t, report, "ng refs/heads/main", "竞态窗口陈旧 push 未被服务端拒绝:\n%s", report)
	require.Contains(t, report, "non-fast-forward", "竞态窗口陈旧 push 未被服务端拒绝")
	// s1's content must not be silently rolled back.
	fresh := support.CloneRepo(t, s, s.TreeURL("t8br", "lib/nested"))
	require.Contains(t, support.ReadFile(t, filepath.Join(fresh, "SHARED.txt")), "line-1", "胜者内容被静默回滚")

	// ---- allow path (separate repo; race with X content unchanged) ----
	s2srv := support.NewServer(t)
	support.SeedRepo(t, s2srv, "t8ba")
	sub := support.CloneRepo(t, s2srv, s2srv.TreeURL("t8ba", "lib/nested"))
	t0a := strings.TrimSpace(support.Git(t, sub, "rev-parse", "origin/main"))

	// Root side edits README (outside X=lib/nested); root main advances.
	root := support.CloneRepo(t, s2srv, s2srv.RepoURL("t8ba"))
	support.Write(t, filepath.Join(root, "README.md"), "readme-v2\n")
	support.Git(t, root, "commit", "-qam", "t8ba: root edits readme")
	support.Git(t, root, "push", "-q", "origin", "main")

	// Stale (advertised at t0a) subtree push: X-view content unchanged → content-level FF passes.
	support.AppendLine(t, filepath.Join(sub, "SHARED.txt"), "line-9\n")
	support.Git(t, sub, "commit", "-qam", "t8ba: sub after unrelated root change")
	c9 := support.HeadOf(t, sub)
	uploadObjects(t, s2srv, "t8ba", sub, c9)
	report = rawPush(t, s2srv, s2srv.TreeURL("t8ba", "lib/nested"), t0a, c9, "refs/heads/main")
	require.Contains(t, report, "ok refs/heads/main",
		"无关根侧变更不应阻塞陈旧子树 push（X 内容未变应放行）:\n%s", report)
	// Convergence: the root worktree holds both README v2 and the new subtree content.
	root2 := support.CloneRepo(t, s2srv, s2srv.RepoURL("t8ba"))
	assert.Equal(t, "readme-v2\n", support.ReadFile(t, filepath.Join(root2, "README.md")), "根 README")
	assert.Contains(t, support.ReadFile(t, filepath.Join(root2, "lib", "nested", "SHARED.txt")), "line-9", "根工作树缺子树新内容")
}
