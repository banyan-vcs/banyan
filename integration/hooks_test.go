// hooks_test.go — T47: receive hooks as a metadata-plane registry with
// HTTP-callback evaluation (docs/04 §7): registrations go through
// /api/v1/repos/<repo>/hooks (every gateway sees the same hooks — no
// gateway-local files); the serving gateway POSTs one JSON payload per
// matching hook (most specific directory first) and 2xx allows, anything
// else declines with the response body relayed to the client. update
// carries the single ref under test; post-receive carries the applied set;
// timeouts/unreachable endpoints fail CLOSED for pre-receive/update.
package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// hookCall is one delivered callback (payload + secret header).
type hookCall struct {
	Event     string `json:"event"`
	Repo      string `json:"repo"`
	Directory string `json:"directory"`
	User      string `json:"user"`
	RepoBytes int64  `json:"repo_bytes"`
	Updates   []struct {
		Old string `json:"old"`
		New string `json:"new"`
		Ref string `json:"ref"`
	} `json:"updates"`
	Secret string
}

// hookService is a scriptable callback target: records every call, declines
// the ones `decline` marks (403 + message), optionally stalls past any
// client timeout.
type hookService struct {
	mu      sync.Mutex
	calls   []hookCall
	decline func(hookCall) (bool, string)
	stall   time.Duration
	srv     *httptest.Server
}

func newHookService(t *testing.T) *hookService {
	t.Helper()
	h := &hookService{}
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		var c hookCall
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &c)
		c.Secret = r.Header.Get("X-Banyan-Hook-Secret")
		h.mu.Lock()
		h.calls = append(h.calls, c)
		decline, msg := false, ""
		if h.decline != nil {
			decline, msg = h.decline(c)
		}
		stall := h.stall
		h.mu.Unlock()
		if stall > 0 {
			time.Sleep(stall)
		}
		if decline {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(msg))
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookService) snapshot() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.calls...)
}

// putHook upserts through the public API (rpc → meta), the operator path.
func putHook(t *testing.T, s *server, repo string, h storage.HookInfo) {
	t.Helper()
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPut, s.URL+"/api/v1/repos/"+repo+"/hooks", bytes.NewReader(raw))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "PUT hook 失败: %v", resp.Status)
}

func deleteHook(t *testing.T, s *server, repo, id string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, s.URL+"/api/v1/repos/"+repo+"/hooks?id="+id, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "DELETE hook 失败: %v", resp.Status)
}

func listHooks(t *testing.T, s *server, repo string) []storage.HookInfo {
	t.Helper()
	resp, err := http.Get(s.URL + "/api/v1/repos/" + repo + "/hooks")
	require.NoError(t, err)
	defer resp.Body.Close()
	var out struct {
		Hooks []storage.HookInfo `json:"hooks"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Hooks
}

func newHookServer(t *testing.T) *server {
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

// createRepoRPC creates the repo over the management API — no push, so the
// first git push stays a branch-create (zero old) under registered hooks.
func createRepoRPC(t *testing.T, s *server, name string) {
	t.Helper()
	resp, err := http.Post(s.URL+"/api/v1/repos", "application/json",
		strings.NewReader(`{"name":"`+name+`"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "建仓失败: %v", resp.Status)
}

func TestHooksReceiveLifecycle(t *testing.T) {
	needGit(t)
	s := newHookServer(t)
	hs := newHookService(t)
	createRepoRPC(t, s, "hr")

	putHook(t, s, "hr", storage.HookInfo{ID: "cap", Events: []string{"pre-receive", "post-receive"}, URL: hs.srv.URL + "/hook"})
	hooks := listHooks(t, s, "hr")
	require.Len(t, hooks, 1, "注册表经公共 API 可读")
	assert.Equal(t, "cap", hooks[0].ID)

	src := seedRepo(t, s, "hr")
	calls := hs.snapshot()
	require.NotEmpty(t, calls, "pre-receive 应回调")

	pre := calls[0]
	assert.Equal(t, "pre-receive", pre.Event)
	assert.Equal(t, "hr", pre.Repo)
	assert.Equal(t, "", pre.Directory, "根推送 directory 为空")
	assert.Greater(t, pre.RepoBytes, int64(0), "pack 先于 pre-receive 摄入，repo_bytes 应为实时值")
	require.Len(t, pre.Updates, 1)
	assert.Equal(t, "refs/heads/main", pre.Updates[0].Ref)
	assert.Regexp(t, `^0{40}$`, pre.Updates[0].Old, "创建分支 old 为全零 hex")
	assert.Regexp(t, `^[0-9a-f]{40}$`, pre.Updates[0].New)

	var post *hookCall
	snap := hs.snapshot()
	for i := range snap {
		if snap[i].Event == "post-receive" {
			post = &snap[i]
			break
		}
	}
	require.NotNil(t, post, "post-receive 应回调")
	assert.Len(t, post.Updates, 1)
	assert.Equal(t, "refs/heads/main", post.Updates[0].Ref)

	// DELETE removes the registration; pushes pass without callbacks.
	deleteHook(t, s, "hr", "cap")
	assert.Empty(t, listHooks(t, s, "hr"))
	write(t, filepath.Join(src, "after.txt"), "after\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "after delete")
	before := len(hs.snapshot())
	git(t, src, "push", "-q", s.repoURL("hr"), "main")
	assert.Equal(t, before, len(hs.snapshot()), "注销后不应再有回调")
}

func TestHooksDecline(t *testing.T) {
	needGit(t)
	s := newHookServer(t)
	hs := newHookService(t)
	hs.decline = func(c hookCall) (bool, string) {
		return c.Event == "pre-receive", "policy: no pushes today"
	}
	createRepoRPC(t, s, "hd")
	putHook(t, s, "hd", storage.HookInfo{ID: "gate", Events: []string{"pre-receive", "update"}, URL: hs.srv.URL + "/hook"})

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "README.md"), "v1\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "seed")
	out, err := gitTry(src, "push", s.repoURL("hd"), "main")
	require.Error(t, err, "pre-receive 拒绝应使 push 失败\n%s", out)
	assert.Contains(t, out, "hook declined")
	assert.Contains(t, out, "policy: no pushes today", "回调响应体首行应回传客户端")
	assert.Empty(t, strings.TrimSpace(git(t, "", "ls-remote", s.repoURL("hd"))), "拒绝后不应有 ref 落地")

	// update declining ONE ref: the other branch of the same push lands.
	hs.decline = func(c hookCall) (bool, string) {
		if c.Event == "update" && len(c.Updates) == 1 && c.Updates[0].Ref == "refs/heads/locked" {
			return true, "locked branch is frozen"
		}
		return false, ""
	}
	git(t, src, "branch", "locked")
	write(t, filepath.Join(src, "free.txt"), "free\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "two branches")
	out, err = gitTry(src, "push", s.repoURL("hd"), "main", "locked")
	require.Error(t, err, "分支级拒绝应使 push 整体报错\n%s", out)
	refs := git(t, "", "ls-remote", "--heads", s.repoURL("hd"))
	assert.Contains(t, refs, "refs/heads/main")
	assert.NotContains(t, refs, "refs/heads/locked", "被 update 钩子点名的分支不应落地")
}

func TestHooksDirectoryScope(t *testing.T) {
	needGit(t)
	s := newHookServer(t)
	hs := newHookService(t)
	libSvc := newHookService(t)
	libSvc.decline = func(hookCall) (bool, string) { return true, "lib is read-only" }

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "lib", "a.txt"), "a\n")
	write(t, filepath.Join(src, "doc", "d.txt"), "d\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "seed")
	git(t, src, "push", "-q", s.repoURL("scope"), "main")

	putHook(t, s, "scope", storage.HookInfo{ID: "root", Events: []string{"pre-receive"}, URL: hs.srv.URL + "/hook"})
	putHook(t, s, "scope", storage.HookInfo{ID: "libgate", Dir: "lib", Events: []string{"pre-receive"}, URL: libSvc.srv.URL + "/hook"})

	libView := cloneRepo(t, s, s.treeURL("scope", "lib"))
	write(t, filepath.Join(libView, "a.txt"), "a-v2\n")
	git(t, libView, "add", "-A")
	git(t, libView, "commit", "-qm", "lib change")
	out, err := gitTry(libView, "push", "origin", "main")
	require.Error(t, err, "lib 目录级 hook 应拒绝\n%s", out)
	assert.Contains(t, out, "lib is read-only")

	docView := cloneRepo(t, s, s.treeURL("scope", "doc"))
	write(t, filepath.Join(docView, "d.txt"), "d-v2\n")
	git(t, docView, "add", "-A")
	git(t, docView, "commit", "-qm", "doc change")
	out, err = gitTry(docView, "push", "origin", "main")
	require.NoError(t, err, "doc 目录无专属 hook、仓库级放行\n%s", out)

	for _, c := range libSvc.snapshot() {
		assert.Equal(t, "lib", c.Directory, "lib 钩子只应收到 lib 目录的回调")
	}
	var sawDoc bool
	for _, c := range hs.snapshot() {
		if c.Directory == "doc" {
			sawDoc = true
		}
	}
	assert.True(t, sawDoc, "仓库级钩子应收到 doc 目录推送（directory=doc）")
}

func TestHooksTimeoutAndSecret(t *testing.T) {
	needGit(t)
	s := newHookServer(t)
	hs := newHookService(t)
	hs.stall = 1500 * time.Millisecond
	createRepoRPC(t, s, "ht")
	putHook(t, s, "ht", storage.HookInfo{
		ID: "slow", Events: []string{"pre-receive"}, URL: hs.srv.URL + "/hook",
		Secret: "s3cret", TimeoutMS: 300,
	})
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "f.txt"), "f\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "one")
	out, err := gitTry(src, "push", s.repoURL("ht"), "main")
	require.Error(t, err, "回调超时应拒（fail-closed）\n%s", out)
	assert.Contains(t, out, "timed out")
	calls := hs.snapshot()
	require.NotEmpty(t, calls)
	assert.Equal(t, "s3cret", calls[0].Secret, "X-Banyan-Hook-Secret 应送达")
}
