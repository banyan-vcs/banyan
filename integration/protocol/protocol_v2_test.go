// Goal: git protocol v2 command set (docs/gateway/git-compat.md §9b; normative reference
// protocol-v2.txt, behavioral baseline upload-pack.c/serve.c). Two layers:
// raw pkt-line requests pin the wire format (advertisement, ls-refs
// ref-prefix/peel/symrefs, fetch section framing, negotiation rounds);
// real-git runs pin the client behavior — with GIT_TRACE_PACKET proving the
// session actually spoke v2 rather than silently downgrading (T30).
package protocol

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// --- raw v2 helpers ---

// v2Get fetches info/refs with the version=2 negotiation header.
func v2Get(t *testing.T, s *support.Server, rawurl string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl+"/info/refs?service=git-upload-pack", nil)
	require.NoError(t, err)
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return resp, body
}

// v2Command posts one command request and returns the raw response body.
func v2Command(t *testing.T, s *support.Server, rawurl, command string, args []string) []byte {
	t.Helper()
	var body strings.Builder
	body.WriteString(pktLine("command=" + command + "\n"))
	body.WriteString("0001") // delim: capabilities → args
	for _, a := range args {
		body.WriteString(pktLine(a + "\n"))
	}
	body.WriteString("0000")
	req, err := http.NewRequest("POST", rawurl+"/git-upload-pack", strings.NewReader(body.String()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return raw
}

// readPkts decodes a pkt-line stream into text entries; flush → "",
// delim → "<delim>", raw tail (v2 packfile section) → "<raw:N bytes>".
func readPkts(raw []byte) []string {
	var out []string
	pos := 0
	for pos+4 <= len(raw) {
		var n int
		fmt.Sscanf(string(raw[pos:pos+4]), "%04x", &n)
		switch {
		case n == 0:
			out = append(out, "")
			pos += 4
		case n == 1:
			out = append(out, "<delim>")
			pos += 4
		case n < 4 || pos+n > len(raw):
			out = append(out, fmt.Sprintf("<bad pkt %d at %d, len %d>", n, pos, len(raw)))
			return out
		default:
			out = append(out, strings.TrimSuffix(string(raw[pos+4:pos+n]), "\n"))
			pos += n
		}
	}
	if pos < len(raw) {
		out = append(out, fmt.Sprintf("<raw:%d bytes>", len(raw)-pos))
	}
	return out
}

// v2PackAfter splits a fetch response at the "packfile" section, decodes the
// sideband pkt framing (*PKT-LINE(%x01-03 ...)) and returns the raw pack.
func v2PackAfter(t *testing.T, raw []byte) []byte {
	t.Helper()
	marker := "000dpackfile\n" // 9 payload bytes + 4 length bytes = 0x000d
	i := strings.Index(string(raw), marker)
	require.GreaterOrEqual(t, i, 0, "响应缺少 packfile 段:\n%s", readPkts(raw))
	var pack []byte
	pos := i + len(marker)
	for pos+4 <= len(raw) {
		var n int
		fmt.Sscanf(string(raw[pos:pos+4]), "%04x", &n)
		if n == 0 { // flush-pkt ends the section
			break
		}
		require.GreaterOrEqual(t, n, 5, "sideband pkt 过短")
		chunk := raw[pos+4 : pos+n]
		require.Equal(t, byte(1), chunk[0], "packfile 段应为 band 1")
		pack = append(pack, chunk[1:]...)
		pos += n
	}
	require.NotEmpty(t, pack, "packfile 段为空")
	return pack
}


// --- T30: raw wire format ---

func TestV2Advertisement(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	support.SeedRepo(t, s, "v2a")

	resp, body := v2Get(t, s, s.RepoURL("v2a"))
	assert.Equal(t, "application/x-git-upload-pack-advertisement", resp.Header.Get("Content-Type"),
		"v2 广告 content-type")
	lines := readPkts(body)
	require.NotEmpty(t, lines)
	assert.Equal(t, "version 2", lines[0], "首行必须是 version 2")
	assert.NotContains(t, string(body), "# service=", "v2 广告不得含 v1 service 前导")
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "fetch=shallow filter", "fetch 能力（shallow+filter）")
	assert.Contains(t, joined, "ls-refs=unborn", "ls-refs 能力")
	assert.Contains(t, joined, "object-format=sha1")
	// receive-pack discovery ignores the v2 header and serves v1.
	req, _ := http.NewRequest("GET", s.RepoURL("v2a")+"/info/refs?service=git-receive-pack", nil)
	req.Header.Set("Git-Protocol", "version=2")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	assert.Contains(t, string(b2), "# service=git-receive-pack", "receive-pack 恒 v1")
}

func TestV2LsRefs(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "v2b")
	support.Git(t, src, "tag", "-a", "v1.0", "-m", "release 1.0")
	support.Git(t, src, "push", "-q", s.RepoURL("v2b"), "main", "--tags")
	head := support.HeadOf(t, src)

	// Full listing with symrefs + peel.
	raw := v2Command(t, s, s.RepoURL("v2b"), "ls-refs", []string{"peel", "symrefs"})
	lines := readPkts(raw)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, head+" refs/heads/main")
	assert.Contains(t, joined, "HEAD symref-target:refs/heads/main", "HEAD symref 属性")
	assert.Contains(t, joined, "refs/tags/v1.0")
	assert.Contains(t, joined, "peeled:"+head, "annotated tag 剥壳属性")
	assert.Equal(t, "", lines[len(lines)-1], "响应以 flush 结束")

	// ref-prefix: only refs/heads/ — tags and HEAD stay out.
	raw = v2Command(t, s, s.RepoURL("v2b"), "ls-refs", []string{"ref-prefix refs/heads/"})
	joined = strings.Join(readPkts(raw), "\n")
	assert.Contains(t, joined, "refs/heads/main")
	assert.NotContains(t, joined, "refs/tags/", "ref-prefix 应过滤 tags")
	assert.NotContains(t, joined, "HEAD", "ref-prefix 应过滤 HEAD")

	// Unborn HEAD of an empty repo (ls-refs=unborn). The receive-pack
	// discovery auto-creates the repo (demo-friendly M2 behavior).
	req, _ := http.NewRequest("GET", s.RepoURL("v2b-empty")+"/info/refs?service=git-receive-pack", nil)
	respEmpty, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	io.Copy(io.Discard, respEmpty.Body)
	respEmpty.Body.Close()
	raw = v2Command(t, s, s.RepoURL("v2b-empty"), "ls-refs", []string{"unborn", "symrefs"})
	joined = strings.Join(readPkts(raw), "\n")
	assert.Contains(t, joined, "unborn HEAD symref-target:refs/heads/main", "空仓库 unborn HEAD")
}

func TestV2FetchSections(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "v2c")
	head := support.HeadOf(t, src)

	// Clone-shaped request: wants + done, no haves → NO acknowledgments
	// section (upload-pack v2 skips acks for done requests) — the packfile
	// section opens the response directly.
	raw := v2Command(t, s, s.RepoURL("v2c"), "fetch", []string{
		"want " + head, "ofs-delta", "done",
	})
	lines := readPkts(raw)
	require.GreaterOrEqual(t, len(lines), 3, "段组帧不足: %v", lines)
	assert.Equal(t, "packfile", lines[0], "done 请求应直接进入 packfile 段")
	assert.Equal(t, "", lines[len(lines)-1], "packfile 段以 flush 收尾")
	dp, err := storage.DecodePack(v2PackAfter(t, raw))
	require.NoError(t, err)
	assert.Equal(t, 1, len(dp.Commits), "v2 fetch 包内 commit")
	assert.NotEmpty(t, dp.Blobs)

	// Terminal round without done: known have → acknowledgments (ACKs +
	// ready) + delim + packfile in the SAME response — the stateless
	// single-round optimization.
	raw = v2Command(t, s, s.RepoURL("v2c"), "fetch", []string{
		"want " + head, "have " + head,
	})
	lines = readPkts(raw)
	assert.Equal(t, "acknowledgments", lines[0])
	assert.Contains(t, lines, "ACK "+head, "已知 have 应 ACK")
	assert.Contains(t, lines, "ready", "无 done 的终态轮应带 ready")
	assert.Contains(t, lines, "packfile", "终态轮同响应送包")

	// done with a known have: still no acknowledgments section.
	raw = v2Command(t, s, s.RepoURL("v2c"), "fetch", []string{
		"want " + head, "have " + head, "done",
	})
	assert.Equal(t, "packfile", readPkts(raw)[0], "done 轮直接 packfile 段")

	// No known have, no done: NAK-only round, no pack — the client continues.
	raw = v2Command(t, s, s.RepoURL("v2c"), "fetch", []string{
		"want " + head, "have " + strings.Repeat("dead", 10),
	})
	lines = readPkts(raw)
	assert.Equal(t, []string{"acknowledgments", "NAK", ""}, lines, "未知 have 无 done 应 NAK+flush 收场")

	// Reachability gate: an unadvertised, unreachable want is rejected.
	raw = v2Command(t, s, s.RepoURL("v2c"), "fetch", []string{
		"want " + strings.Repeat("beef", 10), "done",
	})
	assert.Contains(t, string(raw), "not our ref", "不可达 want 应 ERR 拒绝")

	// Unsupported filter: ERR, mirroring upload-pack.c.
	raw = v2Command(t, s, s.RepoURL("v2c"), "fetch", []string{
		"want " + head, "filter blob:limit=1k", "done",
	})
	assert.Contains(t, string(raw), "unsupported spec", "不支持的 filter 应 ERR 拒绝")

	// v2 filter round-trip: blob:none keeps blobs out of the section pack.
	raw = v2Command(t, s, s.RepoURL("v2c"), "fetch", []string{
		"want " + head, "filter blob:none", "done",
	})
	dp, err = storage.DecodePack(v2PackAfter(t, raw))
	require.NoError(t, err)
	assert.Empty(t, dp.Blobs, "blob:none 过滤后包内不应有 blob")
	assert.NotEmpty(t, dp.Trees)
}

func TestV2ShallowSections(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "v2d")
	for i := 0; i < 3; i++ {
		support.AppendLine(t, filepath.Join(src, "README.md"), fmt.Sprintf("v2d line %d\n", i))
		support.Git(t, src, "commit", "-qam", fmt.Sprintf("v2d c%d", i))
	}
	support.Git(t, src, "push", "-q", s.RepoURL("v2d"), "main")
	tip := support.HeadOf(t, src)

	raw := v2Command(t, s, s.RepoURL("v2d"), "fetch", []string{
		"want " + tip, "deepen 1", "ofs-delta", "done",
	})
	lines := readPkts(raw)
	assert.Equal(t, "shallow-info", lines[0], "done+deepen 应以 shallow-info 段开篇（无 acks 段）: %v", lines)
	assert.Contains(t, lines, "shallow-info", "deepen 应产生 shallow-info 段: %v", lines)
	assert.Contains(t, lines, "packfile")
	shallowLine := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "shallow ") {
			shallowLine = l
		}
	}
	require.NotEmpty(t, shallowLine, "depth 1 应产生 shallow 边界行")
	assert.Equal(t, "shallow "+tip, shallowLine, "边界=tip 提交")
	dp, err := storage.DecodePack(v2PackAfter(t, raw))
	require.NoError(t, err)
	assert.Equal(t, 1, len(dp.Commits), "depth 1 只含 1 个 commit")
}

// --- T30: real client end to end ---

func TestV2RealGitClient(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "v2e")
	seedHead := support.HeadOf(t, src)

	// Clone over v2 — the trace must prove the session negotiated v2.
	cloneDir := filepath.Join(t.TempDir(), "c1")
	out := support.GitV2(t, "", "clone", "-q", s.RepoURL("v2e"), cloneDir)
	require.Contains(t, out, "git< version 2", "会话必须协商 v2（而非静默降级）")
	assert.Equal(t, seedHead, support.HeadOf(t, cloneDir))
	support.FsckClean(t, cloneDir)

	// ls-remote over v2 (exercises ls-refs with peel/symrefs).
	out = support.GitV2(t, "", "ls-remote", s.RepoURL("v2e"))
	require.Contains(t, out, "git< version 2")
	assert.Contains(t, out, seedHead+"	refs/heads/main")

	// Incremental fetch: new commit on the server, fetch over v2, have
	// subtraction drives a single-round terminal response.
	support.AppendLine(t, filepath.Join(src, "lib", "a.txt"), "v2e-a2\n")
	support.Git(t, src, "commit", "-qam", "v2e: increment")
	support.Git(t, src, "push", "-q", s.RepoURL("v2e"), "main")
	newHead := support.HeadOf(t, src)
	out = support.GitV2(t, cloneDir, "fetch", "-q", "origin")
	require.Contains(t, out, "git< version 2")
	support.Git(t, cloneDir, "merge", "-q", "--ff-only", "origin/main")
	assert.Equal(t, newHead, support.HeadOf(t, cloneDir))
	assert.Equal(t, "a-v1\nv2e-a2\n", support.ReadFile(t, filepath.Join(cloneDir, "lib", "a.txt")))

	// Fetch of a specific ref uses ref-prefixes (ls-refs arg path).
	out = support.GitV2(t, cloneDir, "fetch", "-q", "origin", "main")
	require.Contains(t, out, "git< version 2")

	// Shallow clone + unshallow over v2.
	sh := filepath.Join(t.TempDir(), "sh")
	support.GitV2(t, "", "clone", "-q", "--depth", "1", s.RepoURL("v2e"), sh)
	require.FileExists(t, filepath.Join(sh, ".git", "shallow"), "v2 浅克隆应写 .git/shallow")
	assert.Equal(t, newHead, support.HeadOf(t, sh))
	support.GitV2(t, sh, "fetch", "-q", "--unshallow", "origin")
	assert.NoFileExists(t, filepath.Join(sh, ".git", "shallow"), "unshallow 后浅文件应清除")
	assert.Equal(t, newHead, support.HeadOf(t, sh))
	support.FsckClean(t, sh)

	// Subtree URL over v2.
	sub := filepath.Join(t.TempDir(), "sub")
	support.GitV2(t, "", "clone", "-q", s.TreeURL("v2e", "lib/nested"), sub)
	assert.Equal(t, "base\n", support.ReadFile(t, filepath.Join(sub, "SHARED.txt")))
	support.FsckClean(t, sub)

	// Partial clone over v2: filter honored, lazy fetch backfills blobs.
	pc := filepath.Join(t.TempDir(), "pc")
	support.GitV2(t, "", "clone", "-q", "--no-checkout", "--filter=blob:none", s.RepoURL("v2e"), pc)
	support.GitV2(t, pc, "checkout", "main")
	assert.Equal(t, "readme-v1\n", support.ReadFile(t, filepath.Join(pc, "README.md")), "v2 惰性取回后内容")
	support.FsckClean(t, pc)
}

func TestV2PushStaysV1(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "v2f")
	// Push under protocol.version=2 must still work (receive-pack is v0-only;
	// the negotiation header on the receive-pack discovery is ignored).
	support.AppendLine(t, filepath.Join(src, "README.md"), "v2f-more\n")
	support.Git(t, src, "commit", "-qam", "v2f: second")
	out := support.GitV2(t, src, "push", "-q", s.RepoURL("v2f"), "main")
	_ = out
	newHead := support.HeadOf(t, src)
	remote := support.GitV2(t, "", "ls-remote", s.RepoURL("v2f"))
	assert.Contains(t, remote, newHead+"	refs/heads/main", "v2 客户端 push 后服务端头应更新")
}
