// Goal: git protocol compatibility matrix — protocol version matrix,
// partial clone, and shallow clone semantics against reference
// traffic from git http-backend (T13–T16, docs/04 §9).
package integration

import (
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
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t13")
	seedHead := headOf(t, src)

	for _, ver := range []string{"0", "2"} {
		dst := t.TempDir()
		git(t, "", "-c", "protocol.version="+ver, "clone", "-q", s.repoURL("t13"), dst)
		assert.Equal(t, seedHead, headOf(t, dst), "protocol v%s clone 头不一致", ver)
		fsckClean(t, dst)
	}

	// Incremental negotiation under forced v2 downgrades to the v1 terminal-ACK path: new commits arrive via fetch.
	git(t, src, "commit", "-q", "--allow-empty", "-m", "t13: advance")
	git(t, src, "push", "-q", s.repoURL("t13"), "main")
	dst := t.TempDir()
	git(t, "", "clone", "-q", s.repoURL("t13"), dst)
	write(t, filepath.Join(src, "README.md"), "readme-v2\n")
	git(t, src, "commit", "-qam", "t13: more")
	git(t, src, "push", "-q", s.repoURL("t13"), "main")
	git(t, dst, "-c", "protocol.version=2", "fetch", "-q", "origin")
	require.Equal(t, headOf(t, src), strings.TrimSpace(git(t, dst, "rev-parse", "origin/main")),
		"v2 强制增量 fetch 未到达新提交")

	// Subtree URLs also work under forced v2.
	sub := t.TempDir()
	git(t, "", "-c", "protocol.version=2", "clone", "-q", s.treeURL("t13", "lib/nested"), sub)
	require.Equal(t, "base\n", readFile(t, filepath.Join(sub, "SHARED.txt")), "v2 子树 clone 内容")
	fsckClean(t, sub)
}

// --- T14 partial clone: filter advertised and honored (docs/04 §4) ---
// blob:none: the clone pack carries commits+trees only; the first working-tree
// materialization lazy-fetches the missing blobs (promisor semantics — a lazy
// fetch is a plain want of the missing OID, and wants are exempt from the
// filter and from have-exclusion); later fetches keep sending a filter line,
// which the serve path parses and honors every round. tree:0: commits only.
func TestPartialCloneFilter(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t14")
	full := cloneRepo(t, s, s.repoURL("t14"))
	wantObjects := countInPack(t, full)

	// blob:none, no checkout: the initial pack stays blob-free.
	dst := t.TempDir()
	out, _ := gitTry("", "clone", "-q", "--no-checkout", "--filter=blob:none", s.repoURL("t14"), dst)
	require.NotContains(t, out, "filtering not recognized by server", "filter 能力应已广告并被接受")
	assert.Less(t, countInPack(t, dst), wantObjects, "blob:none 初始对象数应少于全量克隆")
	// Working-tree materialization lazy-fetches the blobs on demand.
	git(t, dst, "checkout", "main")
	assert.Equal(t, "readme-v1\n", readFile(t, filepath.Join(dst, "README.md")), "惰性取回后的工作树内容")
	fsckClean(t, dst)
	// Later fetches under promisor config (requests carry a filter line) stay intact.
	git(t, dst, "fetch", "-q", "origin")
	assert.GreaterOrEqual(t, countInPack(t, dst), wantObjects, "惰性取回后应补齐 blob 对象")

	// tree:0: commits only.
	dst2 := t.TempDir()
	gitTry("", "clone", "-q", "--no-checkout", "--filter=tree:0", s.repoURL("t14"), dst2)
	assert.Less(t, countInPack(t, dst2), wantObjects, "tree:0 对象数应少于全量克隆")
	git(t, dst2, "fetch", "-q", "origin")

	// An unsupported filter is answered with ERR, mirroring upload-pack.c.
	dst3 := t.TempDir()
	out, _ = gitTry("", "clone", "-q", "--filter=blob:limit=1k", s.repoURL("t14"), dst3)
	require.Contains(t, out, "unsupported spec", "不支持的 filter 应以 ERR 拒绝")}

// --- T15 shallow clone (--depth): server-side shallowing (upload-pack deepen protocol) ---
// Reference-traffic parity (git 2.50 http-backend, docs/04 §9): every response
// to a deepen request starts with a shallow section (shallow lines for the new
// boundary + unshallow lines for client boundaries now completed + flush; an
// empty boundary is a bare flush); the negotiation round carries only the
// shallow section (no NAK); the done round adds ACK/NAK + a pack with parent
// edges cut at the boundary (boundary commits keep their parent headers; the
// client grafts via .git/shallow).
func TestShallowClone(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t15")
	// c2 lands inside lib/nested (gives the subtree projection ≥2 commits so
	// shallowing semantics are observable).
	write(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "base\nline-2\n")
	git(t, src, "commit", "-qam", "t15: c2")
	write(t, filepath.Join(src, "lib", "a.txt"), "a-v3\n")
	git(t, src, "commit", "-qam", "t15: c3")
	git(t, src, "push", "-q", s.repoURL("t15"), "main")
	tip := headOf(t, src)

	// depth 1: tip only; boundary is the tip itself.
	d1 := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "1", s.repoURL("t15"), d1)
	require.Equal(t, 1, countCommits(t, d1), "depth1 提交数")
	require.Equal(t, tip, strings.TrimSpace(readFile(t, filepath.Join(d1, ".git", "shallow"))), "depth1 浅边界")
	require.Equal(t, "readme-v1\n", readFile(t, filepath.Join(d1, "README.md")), "depth1 工作树")
	fsckClean(t, d1)

	// depth 2: tip + direct parent; boundary is the parent.
	d2 := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "2", s.repoURL("t15"), d2)
	require.Equal(t, 2, countCommits(t, d2), "depth2 提交数")
	fsckClean(t, d2)

	// Over-deep: full history, not a shallow repo (empty boundary = bare
	// flush section, accepted by the client as full).
	d9 := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "99", s.repoURL("t15"), d9)
	require.Equal(t, 3, countCommits(t, d9), "depth99 提交数")
	_, err := os.Stat(filepath.Join(d9, ".git", "shallow"))
	require.Error(t, err, "depth99 不应有浅边界文件")
	fsckClean(t, d9)

	// Subtree URL shallowing: projected history truncated by depth; unshallow
	// runs in projection-OID space.
	sub := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "1", s.treeURL("t15", "lib/nested"), sub)
	require.Equal(t, 1, countCommits(t, sub), "子树 depth1 提交数")
	require.Equal(t, "base\nline-2\n", readFile(t, filepath.Join(sub, "SHARED.txt")), "子树 depth1 工作树")
	git(t, sub, "fetch", "-q", "--unshallow", "origin")
	require.Equal(t, 2, countCommits(t, sub), "子树 unshallow 后提交数（投影全历史）")
	require.Equal(t, "false", strings.TrimSpace(git(t, sub, "rev-parse", "--is-shallow-repository")),
		"子树 unshallow 后仍为浅仓")
	fsckClean(t, sub)
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
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t16")
	// The boundary commit must have a parent (per reference traffic: git
	// auto-removes shallow entries whose parent chain is complete — a root
	// commit as boundary gets cleaned immediately, hiding the semantics).
	write(t, filepath.Join(src, "README.md"), "readme-v2\n")
	git(t, src, "commit", "-qam", "t16: c2")
	git(t, src, "push", "-q", s.repoURL("t16"), "main")
	boundary := headOf(t, src) // c2: shallow boundary candidate

	d := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "1", s.repoURL("t16"), d)

	// Server advances two more commits; plain fetch from the shallow clone:
	// history stops at the old boundary c2, shallow file unchanged.
	write(t, filepath.Join(src, "README.md"), "readme-v3\n")
	git(t, src, "commit", "-qam", "t16: c3")
	write(t, filepath.Join(src, "README.md"), "readme-v4\n")
	git(t, src, "commit", "-qam", "t16: c4")
	git(t, src, "push", "-q", s.repoURL("t16"), "main")

	git(t, d, "fetch", "-q", "origin")
	require.Equal(t, headOf(t, src), strings.TrimSpace(git(t, d, "rev-parse", "origin/main")),
		"浅仓 fetch 未到新 tip")
	git(t, d, "merge", "-q", "--ff-only", "origin/main")
	require.Equal(t, 3, countCommits(t, d), "普通 fetch 后提交数（c4,c3,旧边界 c2）")
	require.Equal(t, boundary, strings.TrimSpace(readFile(t, filepath.Join(d, ".git", "shallow"))),
		"普通 fetch 后浅边界应不变")
	fsckClean(t, d)

	// Deepen to 2: boundary moves one level down (c3 becomes the new
	// boundary; below c2 is still missing).
	git(t, d, "fetch", "-q", "--depth=2", "origin")
	require.Equal(t, 2, countCommits(t, d), "加深 2 后提交数")
	fsckClean(t, d)

	// unshallow: client reports its boundary + deepen ∞; the server's
	// unshallow echo clears the shallow file.
	git(t, d, "fetch", "-q", "--unshallow", "origin")
	require.Equal(t, 4, countCommits(t, d), "unshallow 后提交数（全历史）")
	require.Equal(t, "false", strings.TrimSpace(git(t, d, "rev-parse", "--is-shallow-repository")),
		"unshallow 后仍为浅仓")
	fsckClean(t, d)
}
