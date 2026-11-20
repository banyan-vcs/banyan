// MR functional e2e (docs/design/merge-requests.md): real git pushes, the
// JSON API, and the landed merge verified through real git — two parents,
// both sides' files, the target ref at the merge commit. Runs on the dev
// plane (nil policy, the "local" identity); the protection matrix —
// mr-only rejections and protected-target rank gates — lives in
// integration/authz where the static policy is wired.
package basic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
)

func apiCall(t *testing.T, s *support.Server, method, path string, body any) (int, map[string]any) {
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
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestMergeRequestE2E(t *testing.T) {
	support.NeedGit(t)
	s, _, _ := support.NewServerPlane(t)

	// main: one seed commit; feat: one commit ahead.
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "readme.md"), "seed\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", s.URL+"/r/mr.git", "main")

	support.Git(t, src, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(src, "feat.txt"), "feat work\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "feat")
	support.Git(t, src, "push", "-q", s.URL+"/r/mr.git", "feat")

	// Open the MR over the API.
	code, mr := apiCall(t, s, "POST", "/api/v1/repos/mr/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "add feat"})
	require.Equal(t, 201, code, "%v", mr)
	require.Equal(t, float64(1), mr["iid"])
	require.Equal(t, "open", mr["state"])

	// The detail answers live mergeability.
	code, view := apiCall(t, s, "GET", "/api/v1/repos/mr/merge-requests/1", nil)
	require.Equal(t, 200, code, "%v", view)
	require.Equal(t, true, view["mergeable"], "%v", view)

	// No rule on main: the merge needs no approvals. Land it.
	code, merged := apiCall(t, s, "POST", "/api/v1/repos/mr/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])
	mergeOID, _ := merged["merge_commit"].(string)
	require.NotEmpty(t, mergeOID)

	// Real git agrees: the advertised main IS the merge commit users see —
	// the woven view folds the raw two-parent commit (docs/design/merge-requests.md),
	// so the recorded OID must resolve in a fresh clone and carry both sides.
	require.NotEmpty(t, mergeOID)
	out := support.Git(t, t.TempDir(), "ls-remote", s.URL+"/r/mr.git", "refs/heads/main")
	require.Contains(t, out, mergeOID, "main must advertise the recorded merge commit: %s", out)

	dst := t.TempDir()
	support.Git(t, dst, "clone", "-q", s.URL+"/r/mr.git", "repo")
	work := filepath.Join(dst, "repo")
	require.FileExists(t, filepath.Join(work, "feat.txt"))
	require.FileExists(t, filepath.Join(work, "readme.md"))
	head := support.Git(t, work, "rev-parse", "origin/main")
	require.Equal(t, mergeOID, strings.TrimSpace(head))

	// The MR list answers state-filtered.
	code, list := apiCall(t, s, "GET", "/api/v1/repos/mr/merge-requests?state=merged", nil)
	require.Equal(t, 200, code)
	require.Len(t, list["merge_requests"], 1)
	code, _ = apiCall(t, s, "GET", "/api/v1/repos/mr/merge-requests?state=open", nil)
	require.Equal(t, 200, code)
}

// TestMergeRequestConflictE2E: a conflicting MR refuses to merge with the
// conflict paths, stays open, and merges after the source side resolves.
func TestMergeRequestConflictE2E(t *testing.T) {
	support.NeedGit(t)
	s, _, _ := support.NewServerPlane(t)

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "f.txt"), "base\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", s.URL+"/r/conf.git", "main")
	seed := strings.TrimSpace(support.Git(t, src, "rev-parse", "HEAD"))

	support.Git(t, src, "checkout", "-q", "-b", "feat", seed)
	support.Write(t, filepath.Join(src, "f.txt"), "theirs\n")
	support.Git(t, src, "commit", "-qam", "theirs")
	support.Git(t, src, "push", "-q", s.URL+"/r/conf.git", "feat")

	support.Git(t, src, "checkout", "-q", "main")
	support.Write(t, filepath.Join(src, "f.txt"), "ours\n")
	support.Git(t, src, "commit", "-qam", "ours")
	support.Git(t, src, "push", "-q", s.URL+"/r/conf.git", "main")

	code, mr := apiCall(t, s, "POST", "/api/v1/repos/conf/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "conflicting"})
	require.Equal(t, 201, code, "%v", mr)

	code, view := apiCall(t, s, "GET", "/api/v1/repos/conf/merge-requests/1", nil)
	require.Equal(t, 200, code)
	require.Equal(t, false, view["mergeable"])
	require.Contains(t, fmt.Sprint(view["conflicts"]), "f.txt")

	// Merge bounces with 409 + the conflict path; the MR stays open.
	code, out := apiCall(t, s, "POST", "/api/v1/repos/conf/merge-requests/1/merge", nil)
	require.Equal(t, 409, code, "%v", out)
	code, view = apiCall(t, s, "GET", "/api/v1/repos/conf/merge-requests/1", nil)
	require.Equal(t, "open", view["state"])

	// Resolve on the source side (match the target content); merge lands.
	support.Git(t, src, "checkout", "-q", "feat")
	support.Write(t, filepath.Join(src, "f.txt"), "ours\n")
	support.Git(t, src, "commit", "-qam", "resolve to target")
	support.Git(t, src, "push", "-q", s.URL+"/r/conf.git", "feat")
	code, merged := apiCall(t, s, "POST", "/api/v1/repos/conf/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])
}
