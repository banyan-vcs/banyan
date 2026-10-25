// Goal: the authorization surface (docs/13, M5) end to end with real git
// clients — authentication on every wire endpoint, branch visibility
// (personal namespaces) on both protocol versions, push guards per
// granularity, and directory-level fetch control on subtree virtual repos
// (T38).
package integration

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// newAuthServer boots the stack with the demo static policy wired into the
// git wire layer (docs/13 §2): alice/bob are developers, admin owns the
// protected main, /secret is an admin-only subtree.
func newAuthServer(t *testing.T) (*server, *auth.StaticPolicy) {
	t.Helper()
	policy := &auth.StaticPolicy{
		Users:             map[string]string{"alice": "a", "bob": "b", "admin": "x"},
		Admins:            []string{"admin"},
		ProtectedBranches: []string{"main"},
		SubtreeAccess:     map[string][]string{"/secret": {"admin"}},
	}
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	objects := storage.NewObjectService(meta, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	mux := http.NewServeMux()
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	gw.Auth = policy
	gw.Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &server{Server: srv}, policy
}

// credURL embeds Basic credentials in the URL the way git accepts.
func credURL(s *server, user, pass, path string) string {
	return strings.Replace(s.URL, "http://", "http://"+user+":"+pass+"@", 1) + path
}

func TestAuthorizationWire(t *testing.T) {
	needGit(t)
	s, _ := newAuthServer(t)
	admin := func(p string) string { return credURL(s, "admin", "x", p) }
	alice := func(p string) string { return credURL(s, "alice", "a", p) }
	bob := func(p string) string { return credURL(s, "bob", "b", p) }

	// Seed as admin: main is protected, only admin may push it.
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "README.md"), "readme\n")
	write(t, filepath.Join(src, "secret", "plan.txt"), "the plan\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "seed")
	git(t, src, "push", "-q", admin("/r/t38.git"), "main")

	// Anonymous access is challenged on every endpoint. The empty
	// credential.helper resets any host helper (a dev machine's manager
	// would otherwise silently supply credentials) — this is the truly
	// anonymous client. The exact failure text is environment-dependent
	// (401 body vs terminal-prompt failure); the contract is: no clone.
	out, err := gitTry("", "-c", "credential.helper=", "clone", "-q", s.repoURL("t38"), t.TempDir())
	require.Error(t, err, "匿名克隆应失败:\n%s", out)

	// Authenticated clone works; the shared view shows only main.
	home := cloneRepo(t, s, alice("/r/t38.git"))
	assert.Equal(t, "readme\n", readFile(t, filepath.Join(home, "README.md")))
	lsAlice := git(t, "", "ls-remote", "--heads", alice("/r/t38.git"))
	assert.Equal(t, 1, strings.Count(lsAlice, "\n"), "alice 只应看到 main:\n%s", lsAlice)

	// Personal branches: alice pushes hers, bob cannot even see it.
	git(t, home, "checkout", "-q", "-b", "alice/wip")
	appendLine(t, filepath.Join(home, "README.md"), "alice wip\n")
	git(t, home, "commit", "-qam", "wip")
	git(t, home, "push", "-q", alice("/r/t38.git"), "alice/wip")
	lsBob := git(t, "", "ls-remote", "--heads", bob("/r/t38.git"))
	assert.NotContains(t, lsBob, "alice/wip", "bob 不应看到 alice 的个人分支")
	lsAlice2 := git(t, "", "ls-remote", "--heads", alice("/r/t38.git"))
	assert.Contains(t, lsAlice2, "alice/wip", "alice 应看到自己的个人分支")
	lsAdmin := git(t, "", "ls-remote", "--heads", admin("/r/t38.git"))
	assert.Contains(t, lsAdmin, "alice/wip", "admin 看得到全部")

	// v2 ls-refs filters the same way (the default protocol version).
	lsV2 := gitV2(t, "", "ls-remote", "--heads", bob("/r/t38.git"))
	assert.NotContains(t, lsV2, "alice/wip", "v2 ls-refs 同样过滤")

	// Nobody pushes into someone else's personal namespace.
	git(t, home, "checkout", "-q", "-b", "alice/evil")
	out, err = gitTry(home, "push", "-q", bob("/r/t38.git"), "alice/evil")
	require.Error(t, err, "bob 不能往 alice 的命名空间推:\n%s", out)
	assert.Contains(t, out, "permission denied")

	// Raw-OID fetch through a hidden branch is rejected (visible-tips
	// coverage, not just advertisement filtering).
	hiddenTip := strings.Fields(lsAlice2)[0]
	out, err = gitTry("", "fetch", "-q", bob("/r/t38.git"), hiddenTip)
	require.Error(t, err, "按 OID 取隐藏分支对象应被拒:\n%s", out)

	// Protected branch: developer denied, admin allowed.
	git(t, home, "checkout", "-q", "main")
	appendLine(t, filepath.Join(home, "README.md"), "dev change\n")
	git(t, home, "commit", "-qam", "dev on main")
	out, err = gitTry(home, "push", "-q", alice("/r/t38.git"), "main")
	require.Error(t, err, "保护分支仅 admin:\n%s", out)
	git(t, home, "push", "-q", admin("/r/t38.git"), "main")

	// Directory granularity: /secret is admin-only even for clones.
	out, err = gitTry("", "clone", "-q", bob("/r/t38.git/tree/secret"), t.TempDir())
	require.Error(t, err, "受限目录拒绝:\n%s", out)
	secret := cloneRepo(t, s, admin("/r/t38.git/tree/secret"))
	assert.Equal(t, "the plan\n", readFile(t, filepath.Join(secret, "plan.txt")))

	// Wrong password is anonymous → challenged.
	out, err = gitTry("", "ls-remote", credURL(s, "alice", "wrong", "/r/t38.git"))
	require.Error(t, err)

	// The warm hook and views keep working under auth: a fresh clone of the
	// root repo by admin sees the personal branch folded content intact.
	fresh := cloneRepo(t, s, admin("/r/t38.git"))
	fsckClean(t, fresh)
}

// TestAuthorizationRPCAuthnOff documents the M5 boundary: the JSON RPC
// surface stays open when only the git wire layer carries a policy — fine
// for the demo where the web service is the UI; fine-grained RPC authz is
// listed as M5 follow-up (docs/13 §5).
func TestAuthorizationRPCAuthnOff(t *testing.T) {
	needGit(t)
	s, _ := newAuthServer(t)
	resp, err := http.Get(s.URL + "/api/v1/repos")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "RPC 无策略时保持开放（M5 后续）")
}
