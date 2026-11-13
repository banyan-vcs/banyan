// Goal: git protocol compatibility matrix — protocol version matrix,
// partial clone, and shallow clone semantics against reference
// traffic from git http-backend (T13–T16, docs/gateway/git-compat.md §9).
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

// --- T13 protocol matrix: explicit v0 / forced v2 (v2 is native since the
// protocol v2 command set landed — this case now pins that BOTH version
// paths produce identical results; the v2 wire format itself is pinned in
// protocol_v2_test.go) ---
// git ≥2.26 defaults fetch to v2; every other case's clone/fetch/pull
// exercises v2 by default. This case pins the explicit v0/v2 behavior
// contract across clone / incremental negotiation / subtree URLs (git 2.50).
func TestProtocolVersionMatrix(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t13")
	seedHead := support.HeadOf(t, src)

	for _, ver := range []string{"0", "2"} {
		dst := t.TempDir()
		support.Git(t, "", "-c", "protocol.version="+ver, "clone", "-q", s.RepoURL("t13"), dst)
		assert.Equal(t, seedHead, support.HeadOf(t, dst), "protocol v%s clone 头不一致", ver)
		support.FsckClean(t, dst)
	}

	// Incremental negotiation under forced v2 downgrades to the v1 terminal-ACK path: new commits arrive via fetch.
	support.Git(t, src, "commit", "-q", "--allow-empty", "-m", "t13: advance")
	support.Git(t, src, "push", "-q", s.RepoURL("t13"), "main")
	dst := t.TempDir()
	support.Git(t, "", "clone", "-q", s.RepoURL("t13"), dst)
	support.Write(t, filepath.Join(src, "README.md"), "readme-v2\n")
	support.Git(t, src, "commit", "-qam", "t13: more")
	support.Git(t, src, "push", "-q", s.RepoURL("t13"), "main")
	support.Git(t, dst, "-c", "protocol.version=2", "fetch", "-q", "origin")
	require.Equal(t, support.HeadOf(t, src), strings.TrimSpace(support.Git(t, dst, "rev-parse", "origin/main")),
		"v2 强制增量 fetch 未到达新提交")

	// Subtree URLs also work under forced v2.
	sub := t.TempDir()
	support.Git(t, "", "-c", "protocol.version=2", "clone", "-q", s.TreeURL("t13", "lib/nested"), sub)
	require.Equal(t, "base\n", support.ReadFile(t, filepath.Join(sub, "SHARED.txt")), "v2 子树 clone 内容")
	support.FsckClean(t, sub)
}

// --- T14 partial clone: filter advertised and honored (docs/gateway/git-compat.md §4) ---
// blob:none: the clone pack carries commits+trees only; the first working-tree
// materialization lazy-fetches the missing blobs (promisor semantics — a lazy
// fetch is a plain want of the missing OID, and wants are exempt from the
// filter and from have-exclusion); later fetches keep sending a filter line,
// which the serve path parses and honors every round. tree:0: commits only.
func TestPartialCloneFilter(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "t14")
	full := support.CloneRepo(t, s, s.RepoURL("t14"))
	wantObjects := support.CountInPack(t, full)

	// blob:none, no checkout: the initial pack stays blob-free.
	dst := t.TempDir()
	out, _ := support.GitTry("", "clone", "-q", "--no-checkout", "--filter=blob:none", s.RepoURL("t14"), dst)
	require.NotContains(t, out, "filtering not recognized by server", "filter 能力应已广告并被接受")
	assert.Less(t, support.CountInPack(t, dst), wantObjects, "blob:none 初始对象数应少于全量克隆")
	// Working-tree materialization lazy-fetches the blobs on demand.
	support.Git(t, dst, "checkout", "main")
	assert.Equal(t, "readme-v1\n", support.ReadFile(t, filepath.Join(dst, "README.md")), "惰性取回后的工作树内容")
	support.FsckClean(t, dst)
	// Later fetches under promisor config (requests carry a filter line) stay intact.
	support.Git(t, dst, "fetch", "-q", "origin")
	assert.GreaterOrEqual(t, support.CountInPack(t, dst), wantObjects, "惰性取回后应补齐 blob 对象")

	// tree:0: commits only.
	dst2 := t.TempDir()
	support.GitTry("", "clone", "-q", "--no-checkout", "--filter=tree:0", s.RepoURL("t14"), dst2)
	assert.Less(t, support.CountInPack(t, dst2), wantObjects, "tree:0 对象数应少于全量克隆")
	support.Git(t, dst2, "fetch", "-q", "origin")

	// An unsupported filter is answered with ERR, mirroring upload-pack.c.
	dst3 := t.TempDir()
	out, _ = support.GitTry("", "clone", "-q", "--filter=blob:limit=1k", s.RepoURL("t14"), dst3)
	require.Contains(t, out, "unsupported spec", "不支持的 filter 应以 ERR 拒绝")}

// --- T15 shallow clone (--depth): server-side shallowing (upload-pack deepen protocol) ---
// Reference-traffic parity (git 2.50 http-backend, docs/gateway/git-compat.md §9): every response
// to a deepen request starts with a shallow section (shallow lines for the new
// boundary + unshallow lines for client boundaries now completed + flush; an
// empty boundary is a bare flush); the negotiation round carries only the
// shallow section (no NAK); the done round adds ACK/NAK + a pack with parent
// edges cut at the boundary (boundary commits keep their parent headers; the
// client grafts via .git/shallow).
func TestShallowClone(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t15")
	// c2 lands inside lib/nested (gives the subtree projection ≥2 commits so
	// shallowing semantics are observable).
	support.Write(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "base\nline-2\n")
	support.Git(t, src, "commit", "-qam", "t15: c2")
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-v3\n")
	support.Git(t, src, "commit", "-qam", "t15: c3")
	support.Git(t, src, "push", "-q", s.RepoURL("t15"), "main")
	tip := support.HeadOf(t, src)

	// depth 1: tip only; boundary is the tip itself.
	d1 := t.TempDir()
	support.Git(t, "", "clone", "-q", "--depth", "1", s.RepoURL("t15"), d1)
	require.Equal(t, 1, support.CountCommits(t, d1), "depth1 提交数")
	require.Equal(t, tip, strings.TrimSpace(support.ReadFile(t, filepath.Join(d1, ".git", "shallow"))), "depth1 浅边界")
	require.Equal(t, "readme-v1\n", support.ReadFile(t, filepath.Join(d1, "README.md")), "depth1 工作树")
	support.FsckClean(t, d1)

	// depth 2: tip + direct parent; boundary is the parent.
	d2 := t.TempDir()
	support.Git(t, "", "clone", "-q", "--depth", "2", s.RepoURL("t15"), d2)
	require.Equal(t, 2, support.CountCommits(t, d2), "depth2 提交数")
	support.FsckClean(t, d2)

	// Over-deep: full history, not a shallow repo (empty boundary = bare
	// flush section, accepted by the client as full).
	d9 := t.TempDir()
	support.Git(t, "", "clone", "-q", "--depth", "99", s.RepoURL("t15"), d9)
	require.Equal(t, 3, support.CountCommits(t, d9), "depth99 提交数")
	_, err := os.Stat(filepath.Join(d9, ".git", "shallow"))
	require.Error(t, err, "depth99 不应有浅边界文件")
	support.FsckClean(t, d9)

	// Subtree URL shallowing: projected history truncated by depth; unshallow
	// runs in projection-OID space.
	sub := t.TempDir()
	support.Git(t, "", "clone", "-q", "--depth", "1", s.TreeURL("t15", "lib/nested"), sub)
	require.Equal(t, 1, support.CountCommits(t, sub), "子树 depth1 提交数")
	require.Equal(t, "base\nline-2\n", support.ReadFile(t, filepath.Join(sub, "SHARED.txt")), "子树 depth1 工作树")
	support.Git(t, sub, "fetch", "-q", "--unshallow", "origin")
	require.Equal(t, 2, support.CountCommits(t, sub), "子树 unshallow 后提交数（投影全历史）")
	require.Equal(t, "false", strings.TrimSpace(support.Git(t, sub, "rev-parse", "--is-shallow-repository")),
		"子树 unshallow 后仍为浅仓")
	support.FsckClean(t, sub)
}

// --- T16 shallow-repo increments: fetch (boundary reported) / deepen / unshallow ---
// Semantics per reference traffic (git 2.50 http-backend): a plain fetch from
// a shallow clone carries no deepen — the client wants the new tip, reports
// its current boundary, and sends haves; the server sends no shallow section
// and cuts the pack at the client boundary (history stops at the old tip, the
// shallow file is unchanged). Deepening sends the boundary + deepen N; the
// server answers with a shallow <new-boundary> + unshallow <completed>
// section. Unshallow = deepen ∞; the unshallow echo clears the shallow file.
func TestShallowFetchDeepen(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t16")
	// The boundary commit must have a parent (per reference traffic: git
	// auto-removes shallow entries whose parent chain is complete — a root
	// commit as boundary gets cleaned immediately, hiding the semantics).
	support.Write(t, filepath.Join(src, "README.md"), "readme-v2\n")
	support.Git(t, src, "commit", "-qam", "t16: c2")
	support.Git(t, src, "push", "-q", s.RepoURL("t16"), "main")
	boundary := support.HeadOf(t, src) // c2: shallow boundary candidate

	d := t.TempDir()
	support.Git(t, "", "clone", "-q", "--depth", "1", s.RepoURL("t16"), d)

	// Server advances two more commits; plain fetch from the shallow clone:
	// history stops at the old boundary c2, shallow file unchanged.
	support.Write(t, filepath.Join(src, "README.md"), "readme-v3\n")
	support.Git(t, src, "commit", "-qam", "t16: c3")
	support.Write(t, filepath.Join(src, "README.md"), "readme-v4\n")
	support.Git(t, src, "commit", "-qam", "t16: c4")
	support.Git(t, src, "push", "-q", s.RepoURL("t16"), "main")

	support.Git(t, d, "fetch", "-q", "origin")
	require.Equal(t, support.HeadOf(t, src), strings.TrimSpace(support.Git(t, d, "rev-parse", "origin/main")),
		"浅仓 fetch 未到新 tip")
	support.Git(t, d, "merge", "-q", "--ff-only", "origin/main")
	require.Equal(t, 3, support.CountCommits(t, d), "普通 fetch 后提交数（c4,c3,旧边界 c2）")
	require.Equal(t, boundary, strings.TrimSpace(support.ReadFile(t, filepath.Join(d, ".git", "shallow"))),
		"普通 fetch 后浅边界应不变")
	support.FsckClean(t, d)

	// Deepen to 2: boundary moves one level down (c3 becomes the new
	// boundary; below c2 is still missing).
	support.Git(t, d, "fetch", "-q", "--depth=2", "origin")
	require.Equal(t, 2, support.CountCommits(t, d), "加深 2 后提交数")
	support.FsckClean(t, d)

	// unshallow: client reports its boundary + deepen ∞; the server's
	// unshallow echo clears the shallow file.
	support.Git(t, d, "fetch", "-q", "--unshallow", "origin")
	require.Equal(t, 4, support.CountCommits(t, d), "unshallow 后提交数（全历史）")
	require.Equal(t, "false", strings.TrimSpace(support.Git(t, d, "rev-parse", "--is-shallow-repository")),
		"unshallow 后仍为浅仓")
	support.FsckClean(t, d)
}
