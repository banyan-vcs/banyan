// T89/T90 — the subtree management control plane (docs/07 §8, docs/20
// §"子树管理控制面"): the /api/v1/subtrees listing (which directory
// timelines exist, their last commits, which carry a projection ref) and
// the runtime subtree-access rules (fetch AND push gated by a metadata-plane
// allowlist under a GitLabSSO-identified stack — the GitLab-fronted form).
package subtree

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// subtreesResponse mirrors the /api/v1/subtrees payload.
type subtreesResponse struct {
	Repo          string `json:"repo"`
	DefaultBranch string `json:"defaultBranch"`
	Subtrees      []struct {
		Path      string `json:"path"`
		Projected bool   `json:"projected"`
		Branches  []struct {
			Name    string `json:"name"`
			Head    string `json:"head"`
			Subject string `json:"subject"`
			Author  string `json:"author"`
			When    int64  `json:"when"`
		} `json:"branches"`
	} `json:"subtrees"`
}

func fetchSubtrees(t *testing.T, url, repo string) subtreesResponse {
	t.Helper()
	resp, err := http.Get(url + "/api/v1/subtrees?repo=" + repo)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "subtrees listing must answer 200")
	var out subtreesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// TestT89SubtreesListing: the management listing filters the /.woven
// internals and aux refs, groups by anchor, decorates heads with their last
// commit, and marks the projection set.
func TestT89SubtreesListing(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)

	// Seed: two top-level dirs (lib, app) + a nested one; then a subtree
	// push into lib creates the /lib directory timeline, plus a dev branch
	// on the subtree face.
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "readme\n")
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-v1\n")
	support.Write(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "base\n")
	support.Write(t, filepath.Join(src, "app", "x.go"), "package app\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed root")
	support.Git(t, src, "tag", "v1")
	support.Git(t, src, "push", "-q", s.RepoURL("t89"), "main", "v1")
	// A second branch at the root — subtree views are branch-scoped, so a
	// dev push into /tree/lib needs dev to resolve at the root first.
	support.Git(t, src, "push", "-q", s.RepoURL("t89"), "main:refs/heads/dev")

	lib := support.CloneRepo(t, s, s.TreeURL("t89", "lib"))
	support.AppendLine(t, filepath.Join(lib, "a.txt"), "local edit\n")
	support.Git(t, lib, "commit", "-qam", "lib local work")
	support.Git(t, lib, "push", "-q", s.TreeURL("t89", "lib"), "main")
	support.Git(t, lib, "checkout", "-q", "-b", "dev")
	support.AppendLine(t, filepath.Join(lib, "a.txt"), "dev line\n")
	support.Git(t, lib, "commit", "-qam", "lib dev branch")
	support.Git(t, lib, "push", "-q", s.TreeURL("t89", "lib"), "dev")

	out := fetchSubtrees(t, s.URL, "t89")
	assert.Equal(t, "main", out.DefaultBranch)
	require.Len(t, out.Subtrees, 1, "exactly one subtree (/lib): %v", out.Subtrees)
	sub := out.Subtrees[0]
	assert.Equal(t, "/lib", sub.Path)
	assert.True(t, sub.Projected, "lib is a top-level dir of main — projected")
	require.Len(t, sub.Branches, 2, "main + dev timelines")
	assert.Equal(t, "dev", sub.Branches[0].Name, "branches sorted by name")
	assert.Equal(t, "main", sub.Branches[1].Name)
	assert.Equal(t, "lib dev branch", sub.Branches[0].Subject, "last commit subject rides the head")
	assert.NotEmpty(t, sub.Branches[0].Author)
	assert.NotZero(t, sub.Branches[0].When)
	assert.NotEmpty(t, sub.Branches[1].Head)

	// The head advertised is the subtree timeline's own (root-local)
	// history, not the woven projection — /tree/ semantics.
	lsSub := lsRemoteRef(t, s.TreeURL("t89", "lib"), "refs/heads/main")
	assert.Equal(t, sub.Branches[1].Head, lsSub, "listing head == subtree timeline head")

	// Filtering: nothing /.woven, no root anchor, no aux refs, no nested
	// scope without its own timeline.
	body := fmt.Sprint(out.Subtrees)
	assert.NotContains(t, body, ".woven")
	assert.NotContains(t, body, "v1")

	// Unknown repo answers the standard envelope (404).
	resp, err := http.Get(s.URL + "/api/v1/subtrees?repo=nope")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// newGitLabSSOServer boots the GitLab-fronted stack: identities validated by
// a fake GitLab (/api/v4/user), wrapped with the runtime subtree rules.
func newGitLabSSOServer(t *testing.T) (*support.Server, *httptest.Server) {
	t.Helper()
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer root-token":
			fmt.Fprint(w, `{"username":"root","is_admin":true}`)
		case "Bearer alice-token":
			fmt.Fprint(w, `{"username":"alice"}`)
		case "Bearer bob-token":
			fmt.Fprint(w, `{"username":"bob"}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(gl.Close)

	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(support.TempDirQuiet(t))
	objects := storage.NewObjectService(ms, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	policy := &auth.SubtreeRulePolicy{Inner: auth.NewGitLabSSO(gl.URL), Rules: ms}
	mux := http.NewServeMux()
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.Auth = policy
	gw.Register(mux)
	api := rpc.NewServer(ms, objects, model.SHA1)
	api.Auth = policy
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &support.Server{Server: srv}, gl
}

func ssoCall(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// TestT90SubtreeAccessRules: the runtime rule plane under GitLabSSO —
// admin-managed allowlists gate subtree fetch AND push on banyan's own /r/
// face; the root anchor stays open (the documented closure boundary).
func TestT90SubtreeAccessRules(t *testing.T) {
	support.NeedGit(t)
	s, _ := newGitLabSSOServer(t)
	root := func(p string) string { return "http://root:root-token@" + s.Listener.Addr().String() + p }
	alice := func(p string) string { return "http://alice:alice-token@" + s.Listener.Addr().String() + p }
	bob := func(p string) string { return "http://bob:bob-token@" + s.Listener.Addr().String() + p }

	// Seed as root (any GitLab-identified user may push; no rules yet).
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "readme\n")
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-v1\n")
	support.Write(t, filepath.Join(src, "secret", "plan.txt"), "the plan\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", root("/r/t90.git"), "main")

	// Anonymous API is challenged (GitLabSSO in front of the JSON face).
	resp, err := http.Get(s.URL + "/api/v1/subtrees?repo=t90")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "anonymous must be challenged")

	// An empty rule store must serialize as [] — JSON null crashed the
	// console's rule lookup on the admin path (the token-layer precedent).
	resp = ssoCall(t, "GET", s.URL+"/api/v1/repos/t90/subtree-access", "root-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "admin lists rules")
	var emptyRules struct {
		Rules []map[string]any `json:"rules"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&emptyRules))
	resp.Body.Close()
	require.NotNil(t, emptyRules.Rules, "empty rule list must be [] not null")
	require.Empty(t, emptyRules.Rules)

	// bob can reach the unmanaged secret subtree first…
	bobSecret := support.CloneRepo(t, s, bob("/r/t90.git/tree/secret"))
	assert.Equal(t, "the plan\n", support.ReadFile(t, filepath.Join(bobSecret, "plan.txt")))

	// …then root (instance admin) restricts it to alice.
	resp = ssoCall(t, "PUT", s.URL+"/api/v1/repos/t90/subtree-access", "root-token",
		`{"path":"/secret","users":["alice"]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "admin sets the rule")
	resp.Body.Close()

	// Non-admins cannot manage rules.
	resp = ssoCall(t, "PUT", s.URL+"/api/v1/repos/t90/subtree-access", "alice-token",
		`{"path":"/secret","users":["bob"]}`)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "non-admin must not manage rules")

	// The listing still answers for any identified user (authn gate).
	resp = ssoCall(t, "GET", s.URL+"/api/v1/subtrees?repo=t90", "bob-token", "")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Fetch gating on the git face.
	out, err := support.GitTry("", "clone", "-q", bob("/r/t90.git/tree/secret"), t.TempDir())
	require.Error(t, err, "bob must not clone the restricted subtree:\n%s", out)
	fresh := support.CloneRepo(t, s, alice("/r/t90.git/tree/secret"))
	assert.Equal(t, "the plan\n", support.ReadFile(t, filepath.Join(fresh, "plan.txt")), "alice passes the allowlist")

	// The root anchor stays open — the documented boundary: a subtree rule
	// can never hide content from a shallower anchor (docs/17).
	home := support.CloneRepo(t, s, bob("/r/t90.git"))
	assert.Equal(t, "the plan\n", support.ReadFile(t, filepath.Join(home, "secret", "plan.txt")),
		"root clone serves the full closure (registered boundary)")

	// Push gating: alice's lineage branch through bob's credentials is
	// denied by the rule, through alice's it lands.
	work := support.CloneRepo(t, s, alice("/r/t90.git/tree/secret"))
	support.AppendLine(t, filepath.Join(work, "plan.txt"), "edit\n")
	support.Git(t, work, "commit", "-qam", "secret edit")
	out, err = support.GitTry(work, "push", "-q", bob("/r/t90.git/tree/secret"), "main")
	require.Error(t, err, "bob must not push into the restricted subtree:\n%s", out)
	support.Git(t, work, "push", "-q", alice("/r/t90.git/tree/secret"), "main")

	// Deleting the rule reopens the path (delete needs the admin too).
	resp = ssoCall(t, "DELETE", s.URL+"/api/v1/repos/t90/subtree-access?path=/secret", "root-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	reopened := support.CloneRepo(t, s, bob("/r/t90.git/tree/secret"))
	assert.Contains(t, support.ReadFile(t, filepath.Join(reopened, "plan.txt")), "edit", "rule removal reopens the subtree")

	// Rule listing round-trips.
	resp = ssoCall(t, "PUT", s.URL+"/api/v1/repos/t90/subtree-access", "root-token",
		`{"path":"/lib","users":["alice","bob"]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = ssoCall(t, "GET", s.URL+"/api/v1/repos/t90/subtree-access", "root-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rules struct {
		Rules []meta.SubtreeRule `json:"rules"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rules))
	resp.Body.Close()
	require.Len(t, rules.Rules, 1)
	assert.Equal(t, "/lib", rules.Rules[0].Path)
	assert.Equal(t, []string{"alice", "bob"}, rules.Rules[0].Users, "users round-trip sorted")

	// Malformed rules are client errors, not 500s.
	resp = ssoCall(t, "PUT", s.URL+"/api/v1/repos/t90/subtree-access", "root-token",
		`{"path":"lib","users":["alice"]}`)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "relative path must 400")
}
