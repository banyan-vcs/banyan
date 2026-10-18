// Package integration 将六轮手工测试（MVP → 统一 DAG → 协商 → 交叉
// 压测 → 多人协作）固化为可自动回归的集成测试（docs/07-testing.md）。
//
// 形态：httptest 进程内起完整服务端（MemoryMetaStore + LocalShardStore
// + gitwire + rpc，与 cmd/banyand 同装配），外调真实 git CLI 作为客户端
// ——与手工验证路径完全一致（协议级 bug 只能被真实客户端暴露）。
// 全部用小仓库（毫秒级 push/clone），go-git 4043 提交级重载验证保留为
// 手工场景（README 快速开始）。
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

// --- 测试基建 ---

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

// git 执行真实 git 命令，失败即 Fatal（输出带在错误信息里）。
// 统一注入 core.autocrlf=false（消除 Windows CRLF 噪音）与固定身份。
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitTry(dir, args...)
	if err != nil {
		t.Fatalf("git %v (dir=%s) 失败: %v\n%s", args, dir, err, out)
	}
	return out
}

// gitTry 允许失败（用于断言拒绝类行为），返回合并输出。
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

// seedRepo 构造小仓库并 push 到服务端（触发自动建仓）：
// README.md + lib/a.txt + lib/nested/SHARED.txt，单提交。
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

// --- T1 根仓库 push → clone 往返 ---
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

// --- T2 子树 clone：工作树=子目录内容，原始 hash 进入子树历史 ---
func TestSubtreeCloneUnifiedDAG(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t2")
	_ = src // v4：子树以投影链呈现（SHA 不同），src 仅用于种子

	sub := cloneRepo(t, s, s.treeURL("t2", "lib/nested"))
	// 工作树根 = 子目录内容。
	if got := readFile(t, filepath.Join(sub, "SHARED.txt")); got != "base\n" {
		t.Fatalf("子树工作树内容 = %q", got)
	}
	if _, err := os.Stat(filepath.Join(sub, "README.md")); err == nil {
		t.Fatal("子树工作树不应包含父级文件 README.md")
	}
	// v4 时间线优先（docs/01 §12）：子树历史 = 本路径时间线（投影链，
	// SHA 与根侧不同——允许），内容与根一致（blob 相等）。
	if got := blobOf(t, sub, "SHARED.txt"); got == "" || got != blobOf(t, src, "lib/nested/SHARED.txt") {
		t.Fatalf("子树 blob(%s) 与源(%s)不一致", got, blobOf(t, src, "lib/nested/SHARED.txt"))
	}
	// CI 效率核心断言：子树 clone 只承担子树规模（对象数远小于全仓）。
	subObjects := countInPack(t, sub)
	fullObjects := countInPack(t, cloneRepo(t, s, s.repoURL("t2")))
	if subObjects >= fullObjects {
		t.Fatalf("子树 clone 对象数(%d)应远小于全仓(%d)", subObjects, fullObjects)
	}
	fsckClean(t, sub)
}

// countInPack 返回仓库本地对象数（CI 效率指标；小仓库的 pack 会被
// git 默认拆成松散对象——count + in-pack 之和）。
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

// --- T3 ref 操作：新建 / 更新 / 删除分支 ---
func TestRefOperations(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t3")

	// 新建分支。
	git(t, src, "checkout", "-qb", "feature")
	write(t, filepath.Join(src, "feat.txt"), "f1\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t3: feature commit")
	git(t, src, "push", "-q", s.repoURL("t3"), "feature")
	if out := git(t, "", "ls-remote", s.repoURL("t3")); !strings.Contains(out, "refs/heads/feature") {
		t.Fatalf("ls-remote 缺 feature:\n%s", out)
	}

	// 更新分支。
	write(t, filepath.Join(src, "feat.txt"), "f2\n")
	git(t, src, "commit", "-qam", "t3: feature update")
	git(t, src, "push", "-q", s.repoURL("t3"), "feature")

	// 删除分支（全零 new → 服务端 CAS 归零路径）。
	git(t, src, "push", "-q", s.repoURL("t3"), ":feature")
	if out := git(t, "", "ls-remote", s.repoURL("t3")); strings.Contains(out, "refs/heads/feature") {
		t.Fatalf("删除分支后 ls-remote 仍有 feature:\n%s", out)
	}
}

// --- T4 增量 push + 另一克隆 pull ---
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

// --- T5 子树 push 收编：merge 双亲 / C 同 hash 进根 / 文件落位 ---
func TestSubtreePushAdoption(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t5")

	sub := cloneRepo(t, s, s.treeURL("t5", "lib/nested"))
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "line-1\n")
	git(t, sub, "commit", "-qam", "t5: sub first")
	git(t, sub, "push", "-q", "origin", "main")
	cHash := headOf(t, sub)

	// 根视图：merge commit（双亲）+ C 以同 hash 出现 + 工作树落位。
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
	// v4：父级视图以投影呈现同一变更（SHA 与 C 不同，允许）——
	// 断言内容到达（blob 在三视图一致，见下）而非 hash。
	parent := cloneRepo(t, s, s.treeURL("t5", "lib"))
	if got := readFile(t, filepath.Join(parent, "nested", "SHARED.txt")); got != "base\nline-1\n" {
		t.Fatalf("父级视图内容 = %q", got)
	}
	// 三视图 blob hash 一致。
	bSub, bPar, bRoot := blobOf(t, sub, "SHARED.txt"),
		blobOf(t, parent, "nested/SHARED.txt"), blobOf(t, root, "lib/nested/SHARED.txt")
	if bSub != bPar || bSub != bRoot || bSub == "" {
		t.Fatalf("blob 不一致: sub=%s par=%s root=%s", bSub, bPar, bRoot)
	}
	fsckClean(t, sub)
	fsckClean(t, parent)
	fsckClean(t, root)
}

// --- T7 子树连续 push（不 pull）+ 自身 pull = up to date ---
// 回归：resolveViewTip 修复前，第二次 push 必被拒（广告 tip 是合成
// 薄提交而非被收编的 C）。
func TestConsecutiveSubtreePushes(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t7")

	sub := cloneRepo(t, s, s.treeURL("t7", "lib/nested"))
	for i := 1; i <= 3; i++ {
		appendLine(t, filepath.Join(sub, "SHARED.txt"), fmt.Sprintf("c%d\n", i))
		git(t, sub, "commit", "-qam", fmt.Sprintf("t7: consecutive %d", i))
		git(t, sub, "push", "-q", "origin", "main") // 任一次被拒即 Fatal
	}
	// push 后服务端 tip = 客户端刚推的提交 → 自身 pull 应 up to date。
	if out := git(t, sub, "pull"); !strings.Contains(out, "Already up to date") {
		t.Fatalf("自身 pull 应为 up to date:\n%s", out)
	}
}

// --- T8 非干扰并行 + 路径内互斥（docs/01 §12 核心目标的两面）---
//
//	v4 语义：子树视图 tip 只随【本路径内容】移动——根侧改 X 之外的
//	文件不移动子树 tip，陈旧子树 push 无需 pull 直接成功（大规模
//	并行迭代互不干扰）；根侧改 X 之内则子树 tip 前移，陈旧 push 被
//	拒（防丢失更新，恢复流 pull --rebase → push）。
func TestCrossViewNonInterference(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t8")

	root := cloneRepo(t, s, s.repoURL("t8"))
	sub := cloneRepo(t, s, s.treeURL("t8", "lib/nested"))

	// ① 非干扰：根改 X 之外（lib/a.txt），陈旧子树 push 直接过。
	write(t, filepath.Join(root, "lib", "a.txt"), "a-v2\n")
	git(t, root, "commit", "-qam", "t8: root advances (outside X)")
	git(t, root, "push", "-q", "origin", "main")

	appendLine(t, filepath.Join(sub, "SHARED.txt"), "parallel-ok\n")
	git(t, sub, "commit", "-qam", "t8: sub parallel (unrelated root change)")
	git(t, sub, "push", "-q", "origin", "main") // 无 pull：非干扰

	root2 := cloneRepo(t, s, s.repoURL("t8"))
	if got := readFile(t, filepath.Join(root2, "lib", "a.txt")); got != "a-v2\n" {
		t.Fatalf("根侧变更丢失: %q", got)
	}
	if got := readFile(t, filepath.Join(root2, "lib", "nested", "SHARED.txt")); !strings.Contains(got, "parallel-ok") {
		t.Fatalf("子树变更未达根: %q", got)
	}

	// ② 路径内互斥：根改 X 之内，陈旧子树 push 必须被拒 → 恢复流。
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
	// 真冲突显式解决（docs/01 §12：服务端不做内容合并）。
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

// --- T9 过期克隆 pull（v1 have 协商）+ 二次 pull ---
// 回归：终态 ACK 语义——协商死锁（remote hung up）与 bad pack header
// 均出在这条路径；且 push 后自身 pull 不应重新拉取。
func TestNegotiationPullStaleClone(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t9")

	a := cloneRepo(t, s, s.repoURL("t9")) // 有全部对象的过期克隆
	b := cloneRepo(t, s, s.repoURL("t9"))

	write(t, filepath.Join(b, "neg.txt"), "neg\n")
	git(t, b, "add", "-A")
	git(t, b, "commit", "-qm", "t9: advance from b")
	git(t, b, "push", "-q", "origin", "main")

	// a 的 pull 走 have 协商（ACK/NAK 终态语义）。
	if out := git(t, a, "pull"); !strings.Contains(out, "Fast-forward") {
		t.Fatalf("过期克隆 pull 未快进:\n%s", out)
	}
	if headOf(t, a) != headOf(t, b) {
		t.Fatal("pull 后 head 不一致")
	}
	// 二次 pull：up to date。
	if out := git(t, a, "pull"); !strings.Contains(out, "Already up to date") {
		t.Fatalf("二次 pull 应 up to date:\n%s", out)
	}
}

// --- T10 多人协作：同一文件、根/父/子三视图、五轮串行交叉，零冲突 ---
// 回归：视图 tip 链式化前，merge-base 落在全仓作用域根提交 → X 路径
// add/add 伪冲突（即使编辑完全串行）。协作纪律：pull → 改 → push。
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
		// 先 pull：零冲突断言（冲突时 git 退出码非零且输出含 CONFLICT）。
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

	// 收尾：全员 pull 后断言收敛。
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
	// v4 一致性（docs/01 §12）：每个协作提交以【原 hash】出现在根视图
	//（收编保留第二父 / 根直推天然在）——SHA 稳定可追溯；其他视图以
	// 投影呈现同一变更（SHA 不同，明确允许），内容一致性由上方三视图
	// blob hash 断言保证。
	for i, h := range hashes {
		if !hasCommit(t, R, h) {
			t.Fatalf("协作提交 %d（%s）未出现在根视图（收编第二父应保留原 hash）\nroot chain:\n%s",
				i+1, h[:9], git(t, R, "log", "--oneline", "-12"))
		}
	}
	// 作者自己的视图经身份映射同样保留原 hash（r1/r3=alice→S）。
	for _, i := range []int{0, 2} {
		if !hasCommit(t, S, hashes[i]) {
			t.Fatalf("协作提交 %d（%s）未出现在作者自己的子树视图（身份映射应保留原 hash）", i+1, hashes[i][:9])
		}
	}
	for _, d := range []string{R, P, S} {
		fsckClean(t, d)
	}
}

// --- T11 /api/v1/log 路径过滤（X 视角归一 + hash 一致视图） ---
func TestFilteredLogRPC(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t11")

	// 根提交只改 README（不触碰 lib）。
	root := cloneRepo(t, s, s.repoURL("t11"))
	write(t, filepath.Join(root, "README.md"), "readme-v2\n")
	git(t, root, "commit", "-qam", "t11: readme only")
	git(t, root, "push", "-q", "origin", "main")

	// 子树提交改 lib/nested/SHARED.txt。
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
			sawReadmeOnly = true // 不应出现：该提交未触碰该路径
		}
	}
	if !sawSeed || !sawSub {
		t.Fatalf("过滤 log 缺少预期提交: seed=%v sub=%v (entries=%d)", sawSeed, sawSub, len(body.Entries))
	}
	if sawReadmeOnly {
		t.Fatal("过滤 log 不应包含未触碰该路径的提交（t11: readme only）")
	}
}

// --- T8b 服务端 sameXView：force push 绕过客户端检查后必须由服务端判定 ---

// pktLine 编码一条 pkt-line（协议测试基建）。
func pktLine(s string) string {
	return fmt.Sprintf("%04x", len(s)+4) + s
}

// rawPush 手工构造 receive-pack POST（协议级，确定性模拟竞态窗口）：
// 客户端取完广告后另一路 push 已落地——old 为陈旧广告值。pack 段为空
// （对象需已在服务端，见 uploadObjects）。返回 report-status 原文。
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

// uploadObjects 把 dir 中某提交的对象上传到根仓库（借助临时分支），
// 不触碰根 main。用于 rawPush 前置：对象必须在服务端。
func uploadObjects(t *testing.T, s *server, repoName, dir, commitish string) {
	t.Helper()
	git(t, dir, "push", "-q", s.repoURL(repoName), commitish+":refs/heads/upload")
	git(t, dir, "push", "-q", s.repoURL(repoName), ":refs/heads/upload") // 用后即删
}

// --- T8b 竞态窗口的陈旧 push 防护（服务端 sameXView）---
//
// 关键协议事实（本次审计实测确认）：git push 发送的 old 恒为【本次
// 广告的当前值】——force 只解除客户端祖先检查，不改 old。因此
// sameXView 防线不在 force 路径，而在竞态窗口（客户端取完广告后、
// POST 到达前另一路 push 落地 → old ≠ curTip）与非规范客户端。
// 本用例用 rawPush 确定性构造该窗口：
//
//	拒绝路径：X 内容自分叉后，陈旧 old 的 push 必须被拒（否则整块
//	子树被静默回滚、丢失对方提交）；
//	放行路径：根侧改动 X 之外的内容，X 视图未变——陈旧 old 放行
//	（无关变更不阻塞协作）。
func TestRaceWindowStalePushProtection(t *testing.T) {
	needGit(t)

	// ---- 拒绝路径 ----
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

	// 竞态窗口：s2 的 old 停留在 t0（广告后 s1 已落地）→ 必须拒绝。
	report := rawPush(t, s, s.treeURL("t8br", "lib/nested"), t0, c2, "refs/heads/main")
	if !strings.Contains(report, "ng refs/heads/main") || !strings.Contains(report, "non-fast-forward") {
		t.Fatalf("竞态窗口陈旧 push 未被服务端拒绝:\n%s", report)
	}
	// s1 内容未被静默回滚。
	fresh := cloneRepo(t, s, s.treeURL("t8br", "lib/nested"))
	if got := readFile(t, filepath.Join(fresh, "SHARED.txt")); !strings.Contains(got, "line-1") {
		t.Fatalf("胜者内容被静默回滚: %q", got)
	}

	// ---- 放行路径（独立仓库，X 内容未变的竞态）----
	s2srv := newServer(t)
	seedRepo(t, s2srv, "t8ba")
	sub := cloneRepo(t, s2srv, s2srv.treeURL("t8ba", "lib/nested"))
	t0a := strings.TrimSpace(git(t, sub, "rev-parse", "origin/main"))

	// 根侧改 README（X=lib/nested 之外），根 main 前进。
	root := cloneRepo(t, s2srv, s2srv.repoURL("t8ba"))
	write(t, filepath.Join(root, "README.md"), "readme-v2\n")
	git(t, root, "commit", "-qam", "t8ba: root edits readme")
	git(t, root, "push", "-q", "origin", "main")

	// 陈旧（t0a 时点的广告）子树 push：X 视图内容未变 → 内容级 FF 放行。
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "line-9\n")
	git(t, sub, "commit", "-qam", "t8ba: sub after unrelated root change")
	c9 := headOf(t, sub)
	uploadObjects(t, s2srv, "t8ba", sub, c9)
	report = rawPush(t, s2srv, s2srv.treeURL("t8ba", "lib/nested"), t0a, c9, "refs/heads/main")
	if !strings.Contains(report, "ok refs/heads/main") {
		t.Fatalf("无关根侧变更不应阻塞陈旧子树 push（X 内容未变应放行）:\n%s", report)
	}
	// 收敛：根工作树同时含 README v2 与子树新内容。
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

// --- T13 协议兼容矩阵：v0 显式 / v2 强制（服务端 v1-only，客户端按规范降级）---
// git ≥2.26 对 fetch 默认即 v2——其余全部用例的 clone/fetch/pull 已隐式
// 覆盖"客户端尝试 v2 → 服务端回 v0 广告 → 降级"路径；此处显式钉住
// 两种版本下的 clone / 增量协商 / 子树 URL 行为契约（git 2.50 实测）。
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

	// 增量协商在 v2 强制下降级走 v1 终态 ACK 路径：新提交经 fetch 到达。
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

	// 子树 URL 在 v2 强制下同样可用。
	sub := t.TempDir()
	git(t, "", "-c", "protocol.version=2", "clone", "-q", s.treeURL("t13", "lib/nested"), sub)
	if got := readFile(t, filepath.Join(sub, "SHARED.txt")); got != "base\n" {
		t.Fatalf("v2 子树 clone 内容 = %q", got)
	}
	fsckClean(t, sub)
}

// --- T14 partial clone：未广告 filter 能力 → 客户端警告后回退全量克隆 ---
// 行为契约（git 2.50 实测）：stderr 出现 "filtering not recognized by
// server, ignoring"，clone 成功且对象集与全量 clone 完全一致；git 仍会写
// remote.origin.promisor 配置（对象全在，无害），后续 fetch 继续携带
// filter 行（uploadpack 对 filter 行显式忽略），不破坏。
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
		// promisor 配置下的后续 fetch（请求携带 filter 行）不破坏。
		git(t, dst, "fetch", "-q", "origin")
	}
}

// --- T15 shallow clone（--depth）：服务端浅化（upload-pack deepen 协议）---
// 参考流对照（git 2.50 http-backend 实测，docs/03 §9）：deepen 请求的每个
// 响应前置 shallow 段（新边界 shallow 行 + 客户端已补全边界的 unshallow
// 行 + flush；空边界 = 裸 flush）；协商轮只发 shallow 段（无 NAK）；done 轮
// 接 ACK/NAK + 按边界裁剪父链的 pack（边界提交保留 parent 头，客户端以
// .git/shallow 嫁接）。
func TestShallowClone(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t15")
	// c2 落在 lib/nested 内（使子树投影历史 ≥2 提交，浅化语义可观察）。
	write(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "base\nline-2\n")
	git(t, src, "commit", "-qam", "t15: c2")
	write(t, filepath.Join(src, "lib", "a.txt"), "a-v3\n")
	git(t, src, "commit", "-qam", "t15: c3")
	git(t, src, "push", "-q", s.repoURL("t15"), "main")
	tip := headOf(t, src)

	// depth 1：仅 tip，边界 = tip 本身。
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

	// depth 2：tip + 直接父；边界 = 父。
	d2 := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "2", s.repoURL("t15"), d2)
	if n := countCommits(t, d2); n != 2 {
		t.Fatalf("depth2 提交数 = %d, want 2", n)
	}
	fsckClean(t, d2)

	// 超深：全历史、非浅仓（空边界 = 裸 flush 段，客户端接受为全量）。
	d9 := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "99", s.repoURL("t15"), d9)
	if n := countCommits(t, d9); n != 3 {
		t.Fatalf("depth99 提交数 = %d, want 3", n)
	}
	if _, err := os.Stat(filepath.Join(d9, ".git", "shallow")); !os.IsNotExist(err) {
		t.Fatal("depth99 不应有浅边界文件")
	}
	fsckClean(t, d9)

	// 子树 URL 浅化：投影历史按深度截断；unshallow 走投影 OID 空间。
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

// --- T16 浅仓增量：fetch（上报边界）/ 加深 / unshallow ---
// 语义对照参考流（git 2.50 http-backend 实测）：浅仓普通 fetch 不带
// deepen——客户端 want 新 tip + 上报当前浅边界 + have；服务端不发 shallow
// 段，pack 裁剪至客户端边界（历史止于旧 tip，浅文件不变）。加深时客户端
// 上报浅边界 + deepen N，服务端以 shallow <新边界> + unshallow <已补全>
// 段应答。unshallow = deepen ∞ + unshallow 回显清除浅文件。
func TestShallowFetchDeepen(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t16")
	// 浅边界提交必须有父（对照参考流：git 会自动清除父链已完备的浅条目，
	// 根提交做边界会被立即清理，语义不可观察）。
	write(t, filepath.Join(src, "README.md"), "readme-v2\n")
	git(t, src, "commit", "-qam", "t16: c2")
	git(t, src, "push", "-q", s.repoURL("t16"), "main")
	boundary := headOf(t, src) // c2：浅边界候选

	d := t.TempDir()
	git(t, "", "clone", "-q", "--depth", "1", s.repoURL("t16"), d)

	// 服务端再前进两提交；浅仓普通 fetch：历史止于旧浅边界 c2，浅文件不变。
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

	// 加深到 2：浅边界下移一层（c3 成新边界，c2 之下仍缺失）。
	git(t, d, "fetch", "-q", "--depth=2", "origin")
	if n := countCommits(t, d); n != 2 {
		t.Fatalf("加深 2 后提交数 = %d, want 2", n)
	}
	fsckClean(t, d)

	// unshallow：客户端上报浅边界 + deepen ∞，服务端 unshallow 回显清除。
	git(t, d, "fetch", "-q", "--unshallow", "origin")
	if n := countCommits(t, d); n != 4 {
		t.Fatalf("unshallow 后提交数 = %d, want 4（全历史）", n)
	}
	if out := strings.TrimSpace(git(t, d, "rev-parse", "--is-shallow-repository")); out != "false" {
		t.Fatal("unshallow 后仍为浅仓")
	}
	fsckClean(t, d)
}
