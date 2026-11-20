// MR protection matrix (docs/design/merge-requests.md): the mr-only level
// rejects EVERY direct write — git push, JSON ref CAS, the commit API —
// even from the admin; the merge-request path is the only way in, and its
// approval gate counts other people. A protected target keeps the snapshot
// semantics: merging demands owner rank, the same permission a direct push
// would need.
package authz

import (
	"fmt"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
)

func mrAPI(t *testing.T, s *support.Server, user, pass, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = strings.NewReader(string(raw))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, s.URL+path, rdr)
	require.NoError(t, err)
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errmsg(res map[string]any) string {
	if e, ok := res["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok {
			return m
		}
	}
	return fmt.Sprint(res)
}

func TestMRBranchProtection(t *testing.T) {
	support.NeedGit(t)
	s, _ := newAuthServer(t)

	// Seed main as admin, branch feat as admin (ordinary branches: any
	// authenticated pusher with rank — StaticPolicy grants all users
	// developer rank on ordinary branches).
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "f.txt"), "base\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", credURL(s, "admin", "x", "/r/mrp.git"), "main")
	support.Git(t, src, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(src, "g.txt"), "feat\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "feat")
	support.Git(t, src, "push", "-q", credURL(s, "admin", "x", "/r/mrp.git"), "feat")

	// mr-only on main (admin writes the rule).
	code, rule := mrAPI(t, s, "admin", "x", "PUT", "/api/v1/repos/mrp/branch-rules/main",
		map[string]any{"level": "mr-only", "approvals": 1})
	require.Equal(t, 200, code, "%v", rule)

	// Direct git push bounces for EVERYONE — the admin included.
	support.Git(t, src, "checkout", "-q", "main")
	support.Write(t, filepath.Join(src, "f.txt"), "direct\n")
	support.Git(t, src, "commit", "-qam", "direct push attempt")
	out, err := support.GitTry(src, "push", credURL(s, "admin", "x", "/r/mrp.git"), "main")
	require.Error(t, err, "the admin's direct push must bounce: %s", out)
	require.Contains(t, out, "mr-only: merge via a merge request")

	// The JSON ref CAS and the commit API are fenced the same way.
	code, res := mrAPI(t, s, "admin", "x", "POST", "/api/v1/refs",
		map[string]any{"repo": "mrp", "name": "refs/heads/main", "new": "0000000000000000000000000000000000000001"})
	require.Equal(t, 403, code, "%v", res)
	require.Contains(t, errmsg(res), "mr-only")
	code, res = mrAPI(t, s, "admin", "x", "POST", "/api/v1/commit",
		map[string]any{"repo": "mrp", "message": "api commit", "changes": []map[string]any{}})
	require.Equal(t, 403, code, "%v", res)
	require.Contains(t, errmsg(res), "mr-only")

	// The MR path works: alice opens, bob approves (the author cannot),
	// alice merges — the approval count is the gate, not rank.
	code, mr := mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrp/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "feat into main"})
	require.Equal(t, 201, code, "%v", mr)
	code, res = mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrp/merge-requests/1/approve", nil)
	require.Equal(t, 400, code, "author self-approval must bounce", "%v", res)
	code, res = mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrp/merge-requests/1/merge", nil)
	require.Equal(t, 409, code, "below the approval count", "%v", res)
	require.Contains(t, errmsg(res), "approval")

	code, res = mrAPI(t, s, "bob", "b", "POST", "/api/v1/repos/mrp/merge-requests/1/approve", nil)
	require.Equal(t, 200, code, "%v", res)
	code, merged := mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrp/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])
	mergeOID, _ := merged["merge_commit"].(string)
	require.NotEmpty(t, mergeOID)

	// The merged main carries both sides, and the rule still fences the
	// next direct push.
	remote := support.Git(t, t.TempDir(), "ls-remote", credURL(s, "admin", "x", "/r/mrp.git"), "refs/heads/main")
	require.Contains(t, remote, mergeOID)
	out, err = support.GitTry(src, "push", credURL(s, "admin", "x", "/r/mrp.git"), "main")
	require.Error(t, err, "still mr-only after the merge: %s", out)
}

func TestMRProtectedTargetNeedsOwner(t *testing.T) {
	support.NeedGit(t)
	s, _ := newAuthServer(t) // ProtectedBranches: ["main"]

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "f.txt"), "base\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", credURL(s, "admin", "x", "/r/mrp2.git"), "main")
	support.Git(t, src, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(src, "g.txt"), "feat\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "feat")
	support.Git(t, src, "push", "-q", credURL(s, "admin", "x", "/r/mrp2.git"), "feat")

	// Snapshot policy protects main (owner rank only) and no branch rule
	// exists: the merge gate mirrors the direct-push semantics — the
	// developer's merge bounces, the admin's lands.
	code, mr := mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrp2/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "t"})
	require.Equal(t, 201, code, "%v", mr)
	code, res := mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrp2/merge-requests/1/merge", nil)
	require.Equal(t, 403, code, "a protected target merges at owner rank", "%v", res)
	code, merged := mrAPI(t, s, "admin", "x", "POST", "/api/v1/repos/mrp2/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])
}
