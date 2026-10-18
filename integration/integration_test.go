// Package integration codifies six rounds of manual testing (MVP → unified DAG
// → negotiation → cross-view stress → multi-user collaboration) into automated
// regression integration tests (docs/07-testing.md).
//
// Shape: httptest runs the full server in-process (MemoryMetaStore +
// LocalShardStore + gitwire + rpc, same wiring as cmd/banyan) with the real git
// CLI as client — exactly the manual verification path (protocol-level bugs only
// surface with real clients). All repos are tiny (ms-level push/clone); the go-git
// 4043-commit reload run remains a manual scenario (README quick start).
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// --- Test infrastructure ---

type server struct {
	*httptest.Server
}

func newServer(t *testing.T) *server {
	t.Helper()
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &server{Server: srv}
}

func (s *server) repoURL(name string) string { return s.URL + "/r/" + name + ".git" }
func (s *server) treeURL(name, sub string) string {
	return s.URL + "/r/" + name + ".git/tree/" + sub
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI not available")
	}
}

// git runs a real git command; failure is Fatal (output included in the error).
// Uniformly injects core.autocrlf=false (kills Windows CRLF noise) and a fixed identity.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitTry(dir, args...)
	if err != nil {
		t.Fatalf("git %v (dir=%s) 失败: %v\n%s", args, dir, err, out)
	}
	return out
}

// gitTry tolerates failure (for asserting rejection behavior); returns combined output.
func gitTry(dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "user.name=it", "-c", "user.email=it@banyan.dev",
	}, args...)
	cmd := exec.Command("git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedRepo builds a tiny repo and pushes it to the server (triggers auto-creation):
// README.md + lib/a.txt + lib/nested/SHARED.txt, single commit.
func seedRepo(t *testing.T, s *server, name string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(dir, "README.md"), "readme-v1\n")
	write(t, filepath.Join(dir, "lib", "a.txt"), "a-v1\n")
	write(t, filepath.Join(dir, "lib", "nested", "SHARED.txt"), "base\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "seed: initial "+name)
	git(t, dir, "push", "-q", s.repoURL(name), "main")
	return dir
}

func cloneRepo(t *testing.T, s *server, rawurl string) string {
	t.Helper()
	dst := t.TempDir()
	git(t, "", "clone", "-q", rawurl, dst)
	return dst
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

func countCommits(t *testing.T, dir string) int {
	t.Helper()
	out := strings.TrimSpace(git(t, dir, "rev-list", "--count", "HEAD"))
	var n int
	fmt.Sscanf(out, "%d", &n)
	return n
}

func hasCommit(t *testing.T, dir, hash string) bool {
	t.Helper()
	out := git(t, dir, "log", "--format=%H")
	return strings.Contains(out, hash)
}

func blobOf(t *testing.T, dir, relPath string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD:"+relPath))
}

func fsckClean(t *testing.T, dir string) {
	t.Helper()
	out := git(t, dir, "fsck", "--no-dangling")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("fsck 不干净: %s", out)
	}
}

// --- T1 root repo push → clone roundtrip ---
func TestRootCloneRoundtrip(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t1")
	want := headOf(t, src)

	dst := cloneRepo(t, s, s.repoURL("t1"))
	if got := headOf(t, dst); got != want {
		t.Fatalf("clone HEAD = %s, want %s", got, want)
	}
	if n := countCommits(t, dst); n != 1 {
		t.Fatalf("提交数 = %d, want 1", n)
	}
	if got := readFile(t, filepath.Join(dst, "lib", "nested", "SHARED.txt")); got != "base\n" {
		t.Fatalf("文件内容 = %q", got)
	}
	fsckClean(t, dst)
}

// --- T2 subtree clone: worktree = subdirectory content, original hash enters subtree history ---
func TestSubtreeCloneUnifiedDAG(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t2")
	_ = src // v4: subtree appears as a projection chain (different SHA); src only seeds

	sub := cloneRepo(t, s, s.treeURL("t2", "lib/nested"))
	// Worktree root = subdirectory content.
	if got := readFile(t, filepath.Join(sub, "SHARED.txt")); got != "base\n" {
		t.Fatalf("子树工作树内容 = %q", got)
	}
	if _, err := os.Stat(filepath.Join(sub, "README.md")); err == nil {
		t.Fatal("子树工作树不应包含父级文件 README.md")
	}
	// v4 timeline-first (docs/01 §12): subtree history = this path's timeline (projection
	// chain, SHA differs from the root side — allowed); content matches the root (blob equality).
	if got := blobOf(t, sub, "SHARED.txt"); got == "" || got != blobOf(t, src, "lib/nested/SHARED.txt") {
		t.Fatalf("子树 blob(%s) 与源(%s)不一致", got, blobOf(t, src, "lib/nested/SHARED.txt"))
	}
	// Core CI-efficiency assertion: subtree clone carries only subtree scale (far fewer objects than the full repo).
	subObjects := countInPack(t, sub)
	fullObjects := countInPack(t, cloneRepo(t, s, s.repoURL("t2")))
	if subObjects >= fullObjects {
		t.Fatalf("子树 clone 对象数(%d)应远小于全仓(%d)", subObjects, fullObjects)
	}
	fsckClean(t, sub)
}

// countInPack returns the repo's local object count (CI efficiency metric; on small
// repos git splits packs into loose objects by default — sum of count + in-pack).
func countInPack(t *testing.T, dir string) int {
	t.Helper()
	out := git(t, dir, "count-objects", "-v")
	total := 0
	for _, line := range strings.Split(out, "\n") {
		for _, key := range []string{"count:", "in-pack:"} {
			if strings.HasPrefix(line, key) {
				n := 0
				fmt.Sscanf(strings.TrimSpace(line), key+" %d", &n)
				total += n
			}
		}
	}
	if total == 0 {
		t.Fatalf("count-objects 解析失败: %s", out)
	}
	return total
}

// --- T3 ref operations: create / update / delete branch ---
func TestRefOperations(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t3")

	// Create branch.
	git(t, src, "checkout", "-qb", "feature")
	write(t, filepath.Join(src, "feat.txt"), "f1\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t3: feature commit")
	git(t, src, "push", "-q", s.repoURL("t3"), "feature")
	if out := git(t, "", "ls-remote", s.repoURL("t3")); !strings.Contains(out, "refs/heads/feature") {
		t.Fatalf("ls-remote 缺 feature:\n%s", out)
	}

	// Update branch.
	write(t, filepath.Join(src, "feat.txt"), "f2\n")
	git(t, src, "commit", "-qam", "t3: feature update")
	git(t, src, "push", "-q", s.repoURL("t3"), "feature")

	// Delete branch (all-zero new → server CAS zeroing path).
	git(t, src, "push", "-q", s.repoURL("t3"), ":feature")
	if out := git(t, "", "ls-remote", s.repoURL("t3")); strings.Contains(out, "refs/heads/feature") {
		t.Fatalf("删除分支后 ls-remote 仍有 feature:\n%s", out)
	}
}

// --- T4 incremental push + pull from another clone ---
func TestIncrementalPushPull(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t4")

	a := cloneRepo(t, s, s.repoURL("t4"))
	b := cloneRepo(t, s, s.repoURL("t4"))

	write(t, filepath.Join(a, "inc.txt"), "inc\n")
	git(t, a, "add", "-A")
	git(t, a, "commit", "-qm", "t4: incremental")
	git(t, a, "push", "-q", "origin", "main")

	out := git(t, b, "pull")
	if !strings.Contains(out, "Fast-forward") && !strings.Contains(out, "Already up to date") {
		t.Fatalf("pull 输出异常:\n%s", out)
	}
	if !hasCommit(t, b, headOf(t, a)) {
		t.Fatal("b 未见增量提交")
	}
}

// --- T5 subtree push adoption: merge parents / C enters root at same hash / file placement ---
func TestSubtreePushAdoption(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t5")

	sub := cloneRepo(t, s, s.treeURL("t5", "lib/nested"))
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "line-1\n")
	git(t, sub, "commit", "-qam", "t5: sub first")
	git(t, sub, "push", "-q", "origin", "main")
	cHash := headOf(t, sub)

	// Root view: merge commit (two parents) + C appears at the same hash + worktree placement.
	root := cloneRepo(t, s, s.repoURL("t5"))
	if !hasCommit(t, root, cHash) {
		t.Fatalf("根历史缺少子树提交 %s（hash 一致性破坏）", cHash)
	}
	out := git(t, root, "log", "--format=%s", "-3")
	if !strings.Contains(out, "Merge subtree '/lib/nested'") {
		t.Fatalf("根 log 缺收编 merge:\n%s", out)
	}
	if got := readFile(t, filepath.Join(root, "lib", "nested", "SHARED.txt")); got != "base\nline-1\n" {
		t.Fatalf("根工作树文件 = %q", got)
	}
	// v4: the parent view presents the same change as a projection (SHA differs from C,
	// allowed) — assert content arrival (blob equal across three views, below), not hash.
	parent := cloneRepo(t, s, s.treeURL("t5", "lib"))
	if got := readFile(t, filepath.Join(parent, "nested", "SHARED.txt")); got != "base\nline-1\n" {
		t.Fatalf("父级视图内容 = %q", got)
	}
	// Blob hash identical across three views.
	bSub, bPar, bRoot := blobOf(t, sub, "SHARED.txt"),
		blobOf(t, parent, "nested/SHARED.txt"), blobOf(t, root, "lib/nested/SHARED.txt")
	if bSub != bPar || bSub != bRoot || bSub == "" {
		t.Fatalf("blob 不一致: sub=%s par=%s root=%s", bSub, bPar, bRoot)
	}
	fsckClean(t, sub)
	fsckClean(t, parent)
	fsckClean(t, root)
}

// --- T7 consecutive subtree pushes (no pull) + own pull = up to date ---
// Regression: before the resolveViewTip fix the second push was always rejected
// (the advertised tip was a synthetic thin commit, not the adopted C).
func TestConsecutiveSubtreePushes(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t7")

	sub := cloneRepo(t, s, s.treeURL("t7", "lib/nested"))
	for i := 1; i <= 3; i++ {
		appendLine(t, filepath.Join(sub, "SHARED.txt"), fmt.Sprintf("c%d\n", i))
		git(t, sub, "commit", "-qam", fmt.Sprintf("t7: consecutive %d", i))
		git(t, sub, "push", "-q", "origin", "main") // any rejection is Fatal
	}
	// After push the server tip = the just-pushed commit → own pull should be up to date.
	if out := git(t, sub, "pull"); !strings.Contains(out, "Already up to date") {
		t.Fatalf("自身 pull 应为 up to date:\n%s", out)
	}
}

// --- T8 non-interference parallelism + in-path mutual exclusion (two sides of the docs/01 §12 core goal) ---
//
//	v4 semantics: a subtree view tip moves only with [this path's content] —
//	root-side edits outside X leave the tip unmoved, so a stale subtree push
//	succeeds without pull (parallel iteration never interferes); edits inside
//	X advance the tip, and stale pushes are rejected (lost-update guard; recovery pull --rebase → push).
func TestCrossViewNonInterference(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t8")

	root := cloneRepo(t, s, s.repoURL("t8"))
	sub := cloneRepo(t, s, s.treeURL("t8", "lib/nested"))

	// ① Non-interference: root edits outside X (lib/a.txt); the stale subtree push passes directly.
	write(t, filepath.Join(root, "lib", "a.txt"), "a-v2\n")
	git(t, root, "commit", "-qam", "t8: root advances (outside X)")
	git(t, root, "push", "-q", "origin", "main")

	appendLine(t, filepath.Join(sub, "SHARED.txt"), "parallel-ok\n")
	git(t, sub, "commit", "-qam", "t8: sub parallel (unrelated root change)")
	git(t, sub, "push", "-q", "origin", "main") // no pull: non-interference

	root2 := cloneRepo(t, s, s.repoURL("t8"))
	if got := readFile(t, filepath.Join(root2, "lib", "a.txt")); got != "a-v2\n" {
		t.Fatalf("根侧变更丢失: %q", got)
	}
	if got := readFile(t, filepath.Join(root2, "lib", "nested", "SHARED.txt")); !strings.Contains(got, "parallel-ok") {
		t.Fatalf("子树变更未达根: %q", got)
	}

	// ② In-path mutual exclusion: root edits inside X; the stale subtree push must be rejected → recovery flow.
	sub2 := cloneRepo(t, s, s.treeURL("t8", "lib/nested"))
	root2edit := cloneRepo(t, s, s.repoURL("t8"))
	write(t, filepath.Join(root2edit, "lib", "nested", "SHARED.txt"), "base\nroot-inside-X\n")
	git(t, root2edit, "commit", "-qam", "t8: root edits inside X")
	git(t, root2edit, "push", "-q", "origin", "main")

	appendLine(t, filepath.Join(sub2, "SHARED.txt"), "divergent\n")
	git(t, sub2, "commit", "-qam", "t8: sub2 stale inside X")
	out, err := gitTry(sub2, "push", "origin", "main")
	if err == nil || !strings.Contains(out, "rejected") {
		t.Fatalf("X 内陈旧 push 未被拒: err=%v\n%s", err, out)
	}
		// Real conflict resolved explicitly (docs/01 §12: the server does no content merge).
	if out, err := gitTry(sub2, "pull", "--rebase"); err != nil && !strings.Contains(out, "CONFLICT") {
		t.Fatalf("rebase 失败但非冲突: %v\n%s", err, out)
	}
	write(t, filepath.Join(sub2, "SHARED.txt"), "base\nroot-inside-X\ndivergent\n(resolved)\n")
	git(t, sub2, "add", "-A")
	git(t, sub2, "-c", "core.editor=true", "rebase", "--continue")
	git(t, sub2, "push", "-q", "origin", "main")
	root3 := cloneRepo(t, s, s.repoURL("t8"))
	if got := readFile(t, filepath.Join(root3, "lib", "nested", "SHARED.txt")); !strings.Contains(got, "divergent") {
		t.Fatalf("恢复后子树变更未达根: %q", got)
	}
}

// --- T9 stale clone pull (v1 have negotiation) + second pull ---
// Regression: final-state ACK semantics — negotiation deadlock (remote hung up)
// and bad pack header both hit this path; own pull after push must not refetch.
func TestNegotiationPullStaleClone(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t9")

	a := cloneRepo(t, s, s.repoURL("t9")) // stale clone holding all objects
	b := cloneRepo(t, s, s.repoURL("t9"))

	write(t, filepath.Join(b, "neg.txt"), "neg\n")
	git(t, b, "add", "-A")
	git(t, b, "commit", "-qm", "t9: advance from b")
	git(t, b, "push", "-q", "origin", "main")

	// a's pull goes through have negotiation (ACK/NAK final-state semantics).
	if out := git(t, a, "pull"); !strings.Contains(out, "Fast-forward") {
		t.Fatalf("过期克隆 pull 未快进:\n%s", out)
	}
	if headOf(t, a) != headOf(t, b) {
		t.Fatal("pull 后 head 不一致")
	}
	// Second pull: up to date.
	if out := git(t, a, "pull"); !strings.Contains(out, "Already up to date") {
		t.Fatalf("二次 pull 应 up to date:\n%s", out)
	}
}

// --- T10 multi-user collaboration: same file, root/parent/subtree three views, five serial cross rounds, zero conflicts ---
// Regression: before view tips were chained, merge-base landed on the repo-scope root
// commit → add/add phantom conflict on path X (even with fully serial edits). Discipline: pull → edit → push.
func TestMultiUserSameFileCollaboration(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t10")

	R := cloneRepo(t, s, s.repoURL("t10"))            // carol：根
	P := cloneRepo(t, s, s.treeURL("t10", "lib"))     // bob：父级
	S := cloneRepo(t, s, s.treeURL("t10", "lib/nested")) // alice：子树

	rounds := []struct {
		dir, rel, line, msg string
	}{
		{S, "SHARED.txt", "line-A by alice\n", "t10 r1: alice"},
		{P, "nested/SHARED.txt", "line-B by bob\n", "t10 r2: bob"},
		{S, "SHARED.txt", "line-C by alice\n", "t10 r3: alice"},
		{R, "lib/nested/SHARED.txt", "line-D by carol\n", "t10 r4: carol"},
		{P, "nested/SHARED.txt", "line-E by bob\n", "t10 r5: bob"},
	}
	var hashes []string
	for _, r := range rounds {
		// Pull first: zero-conflict assertion (on conflict git exits non-zero with CONFLICT in output).
		out, err := gitTry(r.dir, "pull")
		if err != nil || strings.Contains(out, "CONFLICT") {
			t.Fatalf("[%s] pull 冲突/失败（串行协作不应冲突）: err=%v\n%s", r.msg, err, out)
		}
		appendLine(t, filepath.Join(r.dir, filepath.FromSlash(r.rel)), r.line)
		git(t, r.dir, "commit", "-qam", r.msg)
		out, err = gitTry(r.dir, "push", "origin", "main")
		if err != nil || strings.Contains(out, "rejected") {
			t.Fatalf("[%s] push 被拒/失败（串行协作不应被拒）: err=%v\n%s", r.msg, err, out)
		}
		hashes = append(hashes, headOf(t, r.dir))
	}

	// Wrap-up: everyone pulls, then assert convergence.
	for _, d := range []string{R, P, S} {
		if out, err := gitTry(d, "pull"); err != nil || strings.Contains(out, "CONFLICT") {
			t.Fatalf("收尾 pull 失败: err=%v\n%s", err, out)
		}
	}
	bS, bP, bR := blobOf(t, S, "SHARED.txt"),
		blobOf(t, P, "nested/SHARED.txt"), blobOf(t, R, "lib/nested/SHARED.txt")
	if bS == "" || bS != bP || bS != bR {
		t.Fatalf("三视图 blob 不一致: S=%s P=%s R=%s", bS, bP, bR)
	}
	if got := readFile(t, filepath.Join(S, "SHARED.txt")); !strings.Contains(got, "line-E by bob") {
		t.Fatalf("最终内容缺最后一行: %q", got)
	}
	// v4 consistency (docs/01 §12): every collaboration commit appears in the root view
	// at [original hash] (adoption keeps the second parent / direct root pushes land there
	// natively) — SHA stays stable and traceable; other views present the same change as
	// projections (different SHA, explicitly allowed); content consistency is guaranteed by the three-view blob hash assertion above.
	for i, h := range hashes {
		if !hasCommit(t, R, h) {
			t.Fatalf("协作提交 %d（%s）未出现在根视图（收编第二父应保留原 hash）\nroot chain:\n%s",
				i+1, h[:9], git(t, R, "log", "--oneline", "-12"))
		}
	}
	// The author's own view also keeps the original hash via identity mapping (r1/r3=alice→S).
	for _, i := range []int{0, 2} {
		if !hasCommit(t, S, hashes[i]) {
			t.Fatalf("协作提交 %d（%s）未出现在作者自己的子树视图（身份映射应保留原 hash）", i+1, hashes[i][:9])
		}
	}
	for _, d := range []string{R, P, S} {
		fsckClean(t, d)
	}
}

// --- T11 /api/v1/log path filter (X-view normalization + hash-consistent view) ---
func TestFilteredLogRPC(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t11")

	// Root commit touches README only (not lib).
	root := cloneRepo(t, s, s.repoURL("t11"))
	write(t, filepath.Join(root, "README.md"), "readme-v2\n")
	git(t, root, "commit", "-qam", "t11: readme only")
	git(t, root, "push", "-q", "origin", "main")

	// Subtree commit touches lib/nested/SHARED.txt.
	sub := cloneRepo(t, s, s.treeURL("t11", "lib/nested"))
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "x\n")
	git(t, sub, "commit", "-qam", "t11: sub touches shared")
	git(t, sub, "push", "-q", "origin", "main")

	q := url.Values{"repo": {"t11"}, "path": {"/lib/nested"}, "limit": {"50"}}
	resp, err := http.Get(s.URL + "/api/v1/log?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Entries []struct {
			OID     string `json:"oid"`
			Subject string `json:"subject"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var sawSeed, sawSub, sawReadmeOnly bool
	for _, e := range body.Entries {
		switch {
		case strings.HasPrefix(e.Subject, "seed: initial"):
			sawSeed = true
		case strings.HasPrefix(e.Subject, "t11: sub touches shared"):
			sawSub = true
		case strings.HasPrefix(e.Subject, "t11: readme only"):
			sawReadmeOnly = true // must not appear: this commit never touched the path
		}
	}
	if !sawSeed || !sawSub {
		t.Fatalf("过滤 log 缺少预期提交: seed=%v sub=%v (entries=%d)", sawSeed, sawSub, len(body.Entries))
	}
	if sawReadmeOnly {
		t.Fatal("过滤 log 不应包含未触碰该路径的提交（t11: readme only）")
	}
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
func rawPush(t *testing.T, s *server, rawurl, old, newOID, ref string) string {
	t.Helper()
	cmd := pktLine(fmt.Sprintf("%s %s %s\x00report-status", old, newOID, ref)) + "0000"
	req, err := http.NewRequest("POST", rawurl+"/git-receive-pack", strings.NewReader(cmd))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// uploadObjects uploads the objects of a commit from dir into the root repo
// (via a temporary branch) without touching root main. Precondition for
// rawPush: the objects must exist server-side.
func uploadObjects(t *testing.T, s *server, repoName, dir, commitish string) {
	t.Helper()
	git(t, dir, "push", "-q", s.repoURL(repoName), commitish+":refs/heads/upload")
	git(t, dir, "push", "-q", s.repoURL(repoName), ":refs/heads/upload") // delete after use
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
	needGit(t)

	// ---- reject path ----
	s := newServer(t)
	seedRepo(t, s, "t8br")
	s1 := cloneRepo(t, s, s.treeURL("t8br", "lib/nested"))
	s2 := cloneRepo(t, s, s.treeURL("t8br", "lib/nested"))
	t0 := strings.TrimSpace(git(t, s2, "rev-parse", "origin/main"))

	appendLine(t, filepath.Join(s1, "SHARED.txt"), "line-1\n")
	git(t, s1, "commit", "-qam", "t8br: s1 wins")
	git(t, s1, "push", "-q", "origin", "main")

	appendLine(t, filepath.Join(s2, "SHARED.txt"), "line-2-divergent\n")
	git(t, s2, "commit", "-qam", "t8br: s2 stale")
	c2 := headOf(t, s2)
	uploadObjects(t, s, "t8br", s2, c2)

	// Race window: s2's old stays at t0 (s1 landed after the advertisement) → must reject.
	report := rawPush(t, s, s.treeURL("t8br", "lib/nested"), t0, c2, "refs/heads/main")
	if !strings.Contains(report, "ng refs/heads/main") || !strings.Contains(report, "non-fast-forward") {
		t.Fatalf("竞态窗口陈旧 push 未被服务端拒绝:\n%s", report)
	}
	// s1's content must not be silently rolled back.
	fresh := cloneRepo(t, s, s.treeURL("t8br", "lib/nested"))
	if got := readFile(t, filepath.Join(fresh, "SHARED.txt")); !strings.Contains(got, "line-1") {
		t.Fatalf("胜者内容被静默回滚: %q", got)
	}

	// ---- allow path (separate repo; race with X content unchanged) ----
	s2srv := newServer(t)
	seedRepo(t, s2srv, "t8ba")
	sub := cloneRepo(t, s2srv, s2srv.treeURL("t8ba", "lib/nested"))
	t0a := strings.TrimSpace(git(t, sub, "rev-parse", "origin/main"))

	// Root side edits README (outside X=lib/nested); root main advances.
	root := cloneRepo(t, s2srv, s2srv.repoURL("t8ba"))
	write(t, filepath.Join(root, "README.md"), "readme-v2\n")
	git(t, root, "commit", "-qam", "t8ba: root edits readme")
	git(t, root, "push", "-q", "origin", "main")

	// Stale (advertised at t0a) subtree push: X-view content unchanged → content-level FF passes.
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "line-9\n")
	git(t, sub, "commit", "-qam", "t8ba: sub after unrelated root change")
	c9 := headOf(t, sub)
	uploadObjects(t, s2srv, "t8ba", sub, c9)
	report = rawPush(t, s2srv, s2srv.treeURL("t8ba", "lib/nested"), t0a, c9, "refs/heads/main")
	if !strings.Contains(report, "ok refs/heads/main") {
		t.Fatalf("无关根侧变更不应阻塞陈旧子树 push（X 内容未变应放行）:\n%s", report)
	}
	// Convergence: the root worktree holds both README v2 and the new subtree content.
	root2 := cloneRepo(t, s2srv, s2srv.repoURL("t8ba"))
	if got := readFile(t, filepath.Join(root2, "README.md")); got != "readme-v2\n" {
		t.Fatalf("根 README = %q", got)
	}
	if got := readFile(t, filepath.Join(root2, "lib", "nested", "SHARED.txt")); !strings.Contains(got, "line-9") {
		t.Fatalf("根工作树缺子树新内容: %q", got)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// --- T13 protocol matrix: explicit v0 / forced v2 (server is v1-only, client downgrades per spec) ---
// git ≥2.26 defaults fetch to v2 — every other case's clone/fetch/pull already
// covers the "client tries v2 → server replies v0 advertisement → downgrade"
// path implicitly; this case pins the explicit behavior contract for both
// versions across clone / incremental negotiation / subtree URLs (git 2.50).
func TestProtocolVersionMatrix(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t13")
	seedHead := headOf(t, src)

	for _, ver := range []string{"0", "2"} {
		dst := t.TempDir()
		git(t, "", "-c", "protocol.version="+ver, "clone", "-q", s.repoURL("t13"), dst)
		if headOf(t, dst) != seedHead {
			t.Fatalf("protocol v%s clone 头不一致", ver)
		}
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
	if got := strings.TrimSpace(git(t, dst, "rev-parse", "origin/main")); got != headOf(t, src) {
		t.Fatalf("v2 强制增量 fetch 未到达新提交: %s", got)
	}

	// Subtree URLs also work under forced v2.
	sub := t.TempDir()
	git(t, "", "-c", "protocol.version=2", "clone", "-q", s.treeURL("t13", "lib/nested"), sub)
	if got := readFile(t, filepath.Join(sub, "SHARED.txt")); got != "base\n" {
		t.Fatalf("v2 子树 clone 内容 = %q", got)
	}
	fsckClean(t, sub)
}

// --- T14 partial clone: filter capability not advertised → client warns and falls back to full clone ---
// Behavior contract (git 2.50): stderr contains "filtering not recognized by
// server, ignoring"; the clone succeeds with an object set identical to a full
// clone; git still records remote.origin.promisor (harmless — all objects
// present) and later fetches keep sending a filter line (uploadpack ignores
// filter lines explicitly), without breakage.
func TestPartialCloneFilterFallback(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t14")
	full := cloneRepo(t, s, s.repoURL("t14"))
	wantObjects := countInPack(t, full)

	for _, filter := range []string{"blob:none", "tree:0"} {
		dst := t.TempDir()
		out, _ := gitTry("", "clone", "--filter="+filter, s.repoURL("t14"), dst)
		if !strings.Contains(out, "filtering not recognized by server, ignoring") {
			t.Fatalf("filter=%s 应回退并警告:\n%s", filter, out)
		}
		if n := countInPack(t, dst); n != wantObjects {
			t.Fatalf("filter=%s 回退后对象不完整: %d != 全量 %d", filter, n, wantObjects)
		}
		if got := readFile(t, filepath.Join(dst, "README.md")); got != "readme-v1\n" {
			t.Fatalf("filter=%s 工作树内容 = %q", filter, got)
		}
		fsckClean(t, dst)
		// Later fetches under promisor config (requests carry a filter line) stay intact.
		git(t, dst, "fetch", "-q", "origin")
	}
}

// --- T15 shallow clone (--depth): server-side shallowing (upload-pack deepen protocol) ---
// Reference-traffic parity (git 2.50 http-backend, docs/03 §9): every response
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
	if n := countCommits(t, d1); n != 1 {
		t.Fatalf("depth1 提交数 = %d, want 1", n)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(d1, ".git", "shallow"))); got != tip {
		t.Fatalf("depth1 浅边界 = %s, want tip %s", got, tip)
	}
	if got := readFile(t, filepath.Join(d1, "README.md")); got != "readme-v1\n" {
		t.Fatalf("depth1 工作树 = %q", got)
	}
	fsckClean(t, d1)

	// depth 2: tip + direct parent; boundary is the parent.
	d2 := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "2", s.repoURL("t15"), d2)
	if n := countCommits(t, d2); n != 2 {
		t.Fatalf("depth2 提交数 = %d, want 2", n)
	}
	fsckClean(t, d2)

	// Over-deep: full history, not a shallow repo (empty boundary = bare
	// flush section, accepted by the client as full).
	d9 := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "99", s.repoURL("t15"), d9)
	if n := countCommits(t, d9); n != 3 {
		t.Fatalf("depth99 提交数 = %d, want 3", n)
	}
	if _, err := os.Stat(filepath.Join(d9, ".git", "shallow")); !os.IsNotExist(err) {
		t.Fatal("depth99 不应有浅边界文件")
	}
	fsckClean(t, d9)

	// Subtree URL shallowing: projected history truncated by depth; unshallow
	// runs in projection-OID space.
	sub := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "1", s.treeURL("t15", "lib/nested"), sub)
	if n := countCommits(t, sub); n != 1 {
		t.Fatalf("子树 depth1 提交数 = %d, want 1", n)
	}
	if got := readFile(t, filepath.Join(sub, "SHARED.txt")); got != "base\nline-2\n" {
		t.Fatalf("子树 depth1 工作树 = %q", got)
	}
	git(t, sub, "fetch", "-q", "--unshallow", "origin")
	if n := countCommits(t, sub); n != 2 {
		t.Fatalf("子树 unshallow 后提交数 = %d, want 2（投影全历史）", n)
	}
	if out := strings.TrimSpace(git(t, sub, "rev-parse", "--is-shallow-repository")); out != "false" {
		t.Fatal("子树 unshallow 后仍为浅仓")
	}
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
	if got := strings.TrimSpace(git(t, d, "rev-parse", "origin/main")); got != headOf(t, src) {
		t.Fatalf("浅仓 fetch 未到新 tip: %s", got)
	}
	git(t, d, "merge", "-q", "--ff-only", "origin/main")
	if n := countCommits(t, d); n != 3 {
		t.Fatalf("普通 fetch 后提交数 = %d, want 3（c4,c3,旧边界 c2）", n)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(d, ".git", "shallow"))); got != boundary {
		t.Fatalf("普通 fetch 后浅边界 = %s, want 不变 %s", got, boundary)
	}
	fsckClean(t, d)

	// Deepen to 2: boundary moves one level down (c3 becomes the new
	// boundary; below c2 is still missing).
	git(t, d, "fetch", "-q", "--depth=2", "origin")
	if n := countCommits(t, d); n != 2 {
		t.Fatalf("加深 2 后提交数 = %d, want 2", n)
	}
	fsckClean(t, d)

	// unshallow: client reports its boundary + deepen ∞; the server's
	// unshallow echo clears the shallow file.
	git(t, d, "fetch", "-q", "--unshallow", "origin")
	if n := countCommits(t, d); n != 4 {
		t.Fatalf("unshallow 后提交数 = %d, want 4（全历史）", n)
	}
	if out := strings.TrimSpace(git(t, d, "rev-parse", "--is-shallow-repository")); out != "false" {
		t.Fatal("unshallow 后仍为浅仓")
	}
	fsckClean(t, d)
}
