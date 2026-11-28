// merge request functional e2e (docs/design/merge-requests.md): real git pushes, the
// JSON API, and the landed merge verified through real git — two parents,
// both sides' files, the target ref at the merge commit. Runs on the dev
// plane (nil policy, the "local" identity); the protection matrix —
// merge-request-only rejections and protected-target rank gates — lives in
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
	support.Git(t, src, "push", "-q", s.URL+"/r/flow.git", "main")

	support.Git(t, src, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(src, "feat.txt"), "feat work\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "feat")
	support.Git(t, src, "push", "-q", s.URL+"/r/flow.git", "feat")

	// Open the merge request over the API.
	code, mergeRequest := apiCall(t, s, "POST", "/api/v1/repos/flow/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "add feat"})
	require.Equal(t, 201, code, "%v", mergeRequest)
	require.Equal(t, float64(1), mergeRequest["iid"])
	require.Equal(t, "open", mergeRequest["state"])

	// The detail answers live mergeability.
	code, view := apiCall(t, s, "GET", "/api/v1/repos/flow/merge-requests/1", nil)
	require.Equal(t, 200, code, "%v", view)
	require.Equal(t, true, view["mergeable"], "%v", view)

	// No rule on main: the merge needs no approvals. Land it.
	code, merged := apiCall(t, s, "POST", "/api/v1/repos/flow/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])
	mergeOID, _ := merged["merge_commit"].(string)
	require.NotEmpty(t, mergeOID)

	// Real git agrees: the advertised main IS the merge commit users see —
	// the woven view folds the raw two-parent commit (docs/design/merge-requests.md),
	// so the recorded OID must resolve in a fresh clone and carry both sides.
	require.NotEmpty(t, mergeOID)
	out := support.Git(t, t.TempDir(), "ls-remote", s.URL+"/r/flow.git", "refs/heads/main")
	require.Contains(t, out, mergeOID, "main must advertise the recorded merge commit: %s", out)

	dst := t.TempDir()
	support.Git(t, dst, "clone", "-q", s.URL+"/r/flow.git", "repo")
	work := filepath.Join(dst, "repo")
	require.FileExists(t, filepath.Join(work, "feat.txt"))
	require.FileExists(t, filepath.Join(work, "readme.md"))
	head := support.Git(t, work, "rev-parse", "origin/main")
	require.Equal(t, mergeOID, strings.TrimSpace(head))

	// The merge request list answers state-filtered.
	code, list := apiCall(t, s, "GET", "/api/v1/repos/flow/merge-requests?state=merged", nil)
	require.Equal(t, 200, code)
	require.Len(t, list["merge_requests"], 1)
	code, _ = apiCall(t, s, "GET", "/api/v1/repos/flow/merge-requests?state=open", nil)
	require.Equal(t, 200, code)
}

// TestMergeRequestConflictE2E: a conflicting merge request refuses to merge with the
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

	code, mergeRequest := apiCall(t, s, "POST", "/api/v1/repos/conf/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "conflicting"})
	require.Equal(t, 201, code, "%v", mergeRequest)

	code, view := apiCall(t, s, "GET", "/api/v1/repos/conf/merge-requests/1", nil)
	require.Equal(t, 200, code)
	require.Equal(t, false, view["mergeable"])
	require.Contains(t, fmt.Sprint(view["conflicts"]), "f.txt")

	// Merge bounces with 409 + the conflict path; the merge request stays open.
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

// TestMergeRequestLifecycleE2E: the state machine over real HTTP — close
// and reopen, the terminal gates, the shareable-record semantics (unknown
// and malformed IIDs), approval withdrawal, and the branch-rule CRUD the
// settings page rides. The dev plane has one identity ("local"), so the
// author-self-approval refusal is the approval story here; the
// multi-identity matrix lives in integration/authz.
func TestMergeRequestLifecycleE2E(t *testing.T) {
	support.NeedGit(t)
	s, _, _ := support.NewServerPlane(t)

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "readme.md"), "seed\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", s.URL+"/r/lifecycle.git", "main")
	support.Git(t, src, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(src, "feat.txt"), "feat\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "feat")
	support.Git(t, src, "push", "-q", s.URL+"/r/lifecycle.git", "feat")

	// Unknown and malformed IIDs answer 404 / 400 — the shareable-record
	// semantics of a permanent URL space.
	code, res := apiCall(t, s, "GET", "/api/v1/repos/lifecycle/merge-requests/99", nil)
	require.Equal(t, 404, code, "%v", res)
	require.Contains(t, fmt.Sprint(res["error"]), "NOT_FOUND")
	code, res = apiCall(t, s, "GET", "/api/v1/repos/lifecycle/merge-requests/abc", nil)
	require.Equal(t, 400, code, "%v", res)
	code, res = apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests/1/merge", nil)
	require.Equal(t, 404, code, "merging an unknown merge request is 404, not 409", "%v", res)

	code, mergeRequest := apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "lifecycle"})
	require.Equal(t, 201, code, "%v", mergeRequest)

	// The dev plane's single identity is the author: self-approval bounces,
	// and the withdrawal is an idempotent no-op that never 404s.
	code, res = apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests/1/approve", nil)
	require.Equal(t, 400, code, "%v", res)
	require.Contains(t, fmt.Sprint(res["error"]), "author")
	code, res = apiCall(t, s, "DELETE", "/api/v1/repos/lifecycle/merge-requests/1/approve", nil)
	require.Equal(t, 200, code, "un-approve is idempotent", "%v", res)

	// Close: state only, filtered in the list; the merge needs open.
	code, closed := apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests/1/close", nil)
	require.Equal(t, 200, code, "%v", closed)
	require.Equal(t, "closed", closed["state"])
	code, list := apiCall(t, s, "GET", "/api/v1/repos/lifecycle/merge-requests?state=closed", nil)
	require.Equal(t, 200, code)
	require.Len(t, list["merge_requests"], 1)
	code, list = apiCall(t, s, "GET", "/api/v1/repos/lifecycle/merge-requests?state=open", nil)
	require.Equal(t, 200, code)
	require.Empty(t, list["merge_requests"])
	code, res = apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests/1/merge", nil)
	require.Equal(t, 400, code, "%v", res)
	require.Contains(t, fmt.Sprint(res["error"]), "closed")

	// Reopen, then the merge lands; merged is terminal for close/reopen.
	code, reopened := apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests/1/reopen", nil)
	require.Equal(t, 200, code, "%v", reopened)
	require.Equal(t, "open", reopened["state"])
	code, merged := apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])
	require.NotEmpty(t, merged["merge_commit"])

	code, view := apiCall(t, s, "GET", "/api/v1/repos/lifecycle/merge-requests/1", nil)
	require.Equal(t, 200, code)
	require.Equal(t, "merged", view["state"])
	require.Equal(t, false, view["mergeable"], "the merged detail is the record, not a live recompute")
	require.Equal(t, "local", view["merged_by"])
	code, res = apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests/1/close", nil)
	require.Equal(t, 409, code, "%v", res)
	code, res = apiCall(t, s, "POST", "/api/v1/repos/lifecycle/merge-requests/1/reopen", nil)
	require.Equal(t, 409, code, "a merged merge request never reopens", "%v", res)

	// Branch-rule CRUD (the settings page's surface): the merge-request-only default
	// approval count kicks in when the caller omits it, a bad level bounces,
	// and delete is idempotent.
	code, rule := apiCall(t, s, "PUT", "/api/v1/repos/lifecycle/branch-rules/release",
		map[string]any{"level": "merge-request-only"})
	require.Equal(t, 200, code, "%v", rule)
	require.Equal(t, float64(1), rule["approvals"], "merge-request-only defaults to one approval")
	code, res = apiCall(t, s, "PUT", "/api/v1/repos/lifecycle/branch-rules/bad",
		map[string]any{"level": "nope"})
	require.Equal(t, 400, code, "%v", res)
	code, rules := apiCall(t, s, "GET", "/api/v1/repos/lifecycle/branch-rules", nil)
	require.Equal(t, 200, code)
	require.Len(t, rules["rules"], 1)
	code, res = apiCall(t, s, "DELETE", "/api/v1/repos/lifecycle/branch-rules/release", nil)
	require.Equal(t, 200, code, "%v", res)
	code, res = apiCall(t, s, "DELETE", "/api/v1/repos/lifecycle/branch-rules/release", nil)
	require.Equal(t, 200, code, "rule delete is idempotent", "%v", res)
	code, rules = apiCall(t, s, "GET", "/api/v1/repos/lifecycle/branch-rules", nil)
	require.Equal(t, 200, code)
	require.Empty(t, rules["rules"])
}

// TestMergeRequestBranchRuleExactMatchE2E: rules address branches by exact name —
// the slashy name rides the path's tail segments (release/v2), only that
// branch is fenced (release and release/v2/feature push freely), and the
// fence is a ROOT-timeline affair: the subtree face of the same repo
// (/tree/lib) never consults it.
func TestMergeRequestBranchRuleExactMatchE2E(t *testing.T) {
	support.NeedGit(t)
	s, _, _ := support.NewServerPlane(t)

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "readme.md"), "seed\n")
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", s.URL+"/r/sle.git", "main")
	for _, b := range []string{"release/v2", "release/v3", "hotfix"} {
		support.Git(t, src, "checkout", "-q", "-b", b, "main")
		support.Write(t, filepath.Join(src, strings.ReplaceAll(b, "/", "-")+".txt"), "x\n")
		support.Git(t, src, "add", "-A")
		support.Git(t, src, "commit", "-qm", b)
		support.Git(t, src, "push", "-q", s.URL+"/r/sle.git", b)
	}

	// The slashy name is the path tail — the rule round-trips verbatim.
	code, rule := apiCall(t, s, "PUT", "/api/v1/repos/sle/branch-rules/release/v2",
		map[string]any{"level": "merge-request-only"})
	require.Equal(t, 200, code, "%v", rule)
	require.Equal(t, "release/v2", rule["branch"])

	// Exact match: the ruled branch bounces, the sibling and the plain
	// neighbor push freely.
	support.Git(t, src, "checkout", "-q", "release/v2")
	support.Write(t, filepath.Join(src, "release-v2.txt"), "y\n")
	support.Git(t, src, "commit", "-qam", "bump")
	out, err := support.GitTry(src, "push", s.URL+"/r/sle.git", "release/v2")
	require.Error(t, err, "the ruled branch must bounce: %s", out)
	require.Contains(t, out, "merge-request-only: merge via a merge request")

	support.Git(t, src, "checkout", "-q", "release/v3")
	support.Write(t, filepath.Join(src, "release-v3.txt"), "y\n")
	support.Git(t, src, "commit", "-qam", "bump")
	support.Git(t, src, "push", "-q", s.URL+"/r/sle.git", "release/v3")
	support.Git(t, src, "checkout", "-q", "hotfix")
	support.Write(t, filepath.Join(src, "hotfix.txt"), "y\n")
	support.Git(t, src, "commit", "-qam", "bump")
	support.Git(t, src, "push", "-q", s.URL+"/r/sle.git", "hotfix")

	// The fence is root-timeline only: merge-request-only on main fences the root face
	// but the subtree face of the same repo pushes freely (the subtree owns
	// its own timelines; the woven root view derives, no root CAS happens).
	code, res := apiCall(t, s, "PUT", "/api/v1/repos/sle/branch-rules/main",
		map[string]any{"level": "merge-request-only"})
	require.Equal(t, 200, code, "%v", res)
	sub := support.CloneRepo(t, s, s.TreeURL("sle", "lib"))
	support.Write(t, filepath.Join(sub, "a.txt"), "a-sub\n")
	support.Git(t, sub, "commit", "-qam", "subtree edit")
	support.Git(t, sub, "push", "-q", "origin", "main")
	// The adoption moved the advertised root tip — rebase first so the
	// direct attempt is fast-forward-clean and actually reaches the gate
	// (otherwise git's own client-side check rejects before any command).
	support.Git(t, src, "checkout", "-q", "main")
	support.Git(t, src, "pull", "--rebase", "-q", s.URL+"/r/sle.git", "main")
	support.Write(t, filepath.Join(src, "readme.md"), "direct\n")
	support.Git(t, src, "commit", "-qam", "root direct attempt")
	out, err = support.GitTry(src, "push", s.URL+"/r/sle.git", "main")
	require.Error(t, err, "the root face stays fenced: %s", out)
	require.Contains(t, out, "merge-request-only: merge via a merge request")

	// A slashy rule deletes by the same tail form.
	code, res = apiCall(t, s, "DELETE", "/api/v1/repos/sle/branch-rules/release/v2", nil)
	require.Equal(t, 200, code, "%v", res)
	support.Git(t, src, "checkout", "-q", "release/v2")
	support.Write(t, filepath.Join(src, "release-v2.txt"), "z\n")
	support.Git(t, src, "commit", "-qam", "after delete")
	support.Git(t, src, "push", "-q", s.URL+"/r/sle.git", "release/v2")
}
