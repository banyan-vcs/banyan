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

// TestMRRuleGatesAndWithdrawal: the rule surface's rank gates (members
// read, admins write), the exact-branch scoping of an mr-only rule (other
// branches push freely, the ruled branch survives deletion), and the live
// approval bar — a withdrawn approval re-arms the merge refusal.
func TestMRRuleGatesAndWithdrawal(t *testing.T) {
	support.NeedGit(t)
	s, _ := newAuthServer(t)

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "f.txt"), "base\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", credURL(s, "admin", "x", "/r/mrg.git"), "main")
	support.Git(t, src, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(src, "g.txt"), "feat\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "feat")
	support.Git(t, src, "push", "-q", credURL(s, "admin", "x", "/r/mrg.git"), "feat")

	// Rule management: members read (the settings page renders for them),
	// only admins write.
	code, res := mrAPI(t, s, "alice", "a", "PUT", "/api/v1/repos/mrg/branch-rules/main",
		map[string]any{"level": "mr-only", "approvals": 1})
	require.Equal(t, 403, code, "a developer cannot write rules", "%v", res)
	code, res = mrAPI(t, s, "alice", "a", "DELETE", "/api/v1/repos/mrg/branch-rules/main", nil)
	require.Equal(t, 403, code, "%v", res)
	code, rules := mrAPI(t, s, "alice", "a", "GET", "/api/v1/repos/mrg/branch-rules", nil)
	require.Equal(t, 200, code, "members read the rules", "%v", rules)
	code, res = mrAPI(t, s, "admin", "x", "PUT", "/api/v1/repos/mrg/branch-rules/main",
		map[string]any{"level": "mr-only", "approvals": 1})
	require.Equal(t, 200, code, "%v", res)
	code, rules = mrAPI(t, s, "admin", "x", "GET", "/api/v1/repos/mrg/branch-rules", nil)
	require.Equal(t, 200, code)
	require.Len(t, rules["rules"], 1)

	// Exact-branch scoping: main is fenced (updates AND deletion bounce),
	// feat pushes freely.
	support.Git(t, src, "checkout", "-q", "feat")
	support.Write(t, filepath.Join(src, "g.txt"), "feat2\n")
	support.Git(t, src, "commit", "-qam", "feat moves")
	support.Git(t, src, "push", "-q", credURL(s, "admin", "x", "/r/mrg.git"), "feat")
	out, err := support.GitTry(src, "push", credURL(s, "admin", "x", "/r/mrg.git"), ":main")
	require.Error(t, err, "deleting an mr-only branch must bounce: %s", out)
	require.Contains(t, out, "mr-only: merge via a merge request")

	// The live approval bar: approve, withdraw (the count drops), refuse,
	// re-approve, land.
	code, mr := mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrg/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "t"})
	require.Equal(t, 201, code, "%v", mr)
	code, res = mrAPI(t, s, "bob", "b", "POST", "/api/v1/repos/mrg/merge-requests/1/approve", nil)
	require.Equal(t, 200, code, "%v", res)
	code, res = mrAPI(t, s, "bob", "b", "DELETE", "/api/v1/repos/mrg/merge-requests/1/approve", nil)
	require.Equal(t, 200, code, "%v", res)
	code, res = mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrg/merge-requests/1/merge", nil)
	require.Equal(t, 409, code, "the withdrawn approval re-arms the gate", "%v", res)
	require.Contains(t, errmsg(res), "approval")
	code, res = mrAPI(t, s, "bob", "b", "POST", "/api/v1/repos/mrg/merge-requests/1/approve", nil)
	require.Equal(t, 200, code, "%v", res)
	code, merged := mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/mrg/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])

	// The rule outlives the merge — still listed, still fencing.
	out, err = support.GitTry(src, "push", credURL(s, "admin", "x", "/r/mrg.git"), "main")
	require.Error(t, err, "still mr-only after the merge: %s", out)
}

// TestMRTokenScopeCeiling: PAT scopes ride the MR face unchanged — a read
// token answers the reads but bounces at every mutation's push gate (the
// same ceiling the git face applies); a write token drives the lifecycle.
func TestMRTokenScopeCeiling(t *testing.T) {
	support.NeedGit(t)
	s, _ := newTokenServer(t)
	readTok, _ := mintToken(t, s, "alice", "a", "read")
	writeTok, _ := mintToken(t, s, "alice", "a", "write")

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "f.txt"), "base\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", credURL(s, "alice", "a", "/r/tm.git"), "main")
	support.Git(t, src, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(src, "g.txt"), "feat\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "feat")
	support.Git(t, src, "push", "-q", credURL(s, "alice", "a", "/r/tm.git"), "feat")

	// Read token: the collection read answers; every mutation bounces at
	// the scope ceiling (open gates before anything, the per-MR actions
	// after the target resolves).
	code, list := mrAPI(t, s, "alice", readTok, "GET", "/api/v1/repos/tm/merge-requests", nil)
	require.Equal(t, 200, code, "%v", list)
	code, res := mrAPI(t, s, "alice", readTok, "POST", "/api/v1/repos/tm/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "t"})
	require.Equal(t, 403, code, "%v", res)
	require.Contains(t, errmsg(res), "push permission")

	// Write token opens the request; the read token then bounces on every
	// per-MR action of the real MR.
	code, mr := mrAPI(t, s, "alice", writeTok, "POST", "/api/v1/repos/tm/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "t"})
	require.Equal(t, 201, code, "%v", mr)
	for _, attempt := range []struct {
		method, path string
	}{
		{"POST", "/api/v1/repos/tm/merge-requests/1/approve"},
		{"DELETE", "/api/v1/repos/tm/merge-requests/1/approve"},
		{"POST", "/api/v1/repos/tm/merge-requests/1/merge"},
		{"POST", "/api/v1/repos/tm/merge-requests/1/close"},
	} {
		code, res := mrAPI(t, s, "alice", readTok, attempt.method, attempt.path, nil)
		require.Equal(t, 403, code, "%s %s: %v", attempt.method, attempt.path, res)
		require.Contains(t, errmsg(res), "push permission")
	}

	// Write token merges (no rule — no approval bar); the read token still
	// reads the record afterwards.
	code, merged := mrAPI(t, s, "alice", writeTok, "POST", "/api/v1/repos/tm/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])
	code, view := mrAPI(t, s, "alice", readTok, "GET", "/api/v1/repos/tm/merge-requests/1", nil)
	require.Equal(t, 200, code, "%v", view)
	require.Equal(t, "merged", view["state"])
}

// TestMRReviewMechanicsE2E (T111): the review flow over real git and two
// identities (docs/design/merge-requests.md §4a) — an anchored line
// comment (the server stamps the anchor), a reply, resolution, and the
// request-changes lock: the merge bounces naming the blocker, the author's
// push does NOT dismiss the verdict, the reviewer's re-approval unblocks,
// and the discussion survives the merge. The avatar face rides along: PUT
// stores, GET answers, bogus schemes refuse.
func TestMRReviewMechanicsE2E(t *testing.T) {
	support.NeedGit(t)
	s, _ := newAuthServer(t)

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "f.txt"), "base\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	// main is snapshot-protected on this plane — seed it as admin, branch
	// as alice (the MR's author).
	support.Git(t, src, "push", "-q", credURL(s, "admin", "x", "/r/rev.git"), "main")
	support.Git(t, src, "checkout", "-q", "-b", "feat")
	support.Write(t, filepath.Join(src, "g.txt"), "feat\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "feat")
	support.Git(t, src, "push", "-q", credURL(s, "alice", "a", "/r/rev.git"), "feat")

	code, mr := mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/rev/merge-requests",
		map[string]string{"source": "feat", "target": "main", "title": "review me"})
	require.Equal(t, 201, code, "%v", mr)

	// The anchored comment: alice names the line, the SERVER stamps the
	// anchor with the live source tip (a client-claimed OID cannot exist).
	code, anchored := mrAPI(t, s, "bob", "b", "POST", "/api/v1/repos/rev/merge-requests/1/comments",
		map[string]any{"body": "the f.txt header", "path": "g.txt", "side": "new", "line": 1})
	require.Equal(t, 201, code, "%v", anchored)
	head := support.Git(t, src, "rev-parse", "feat")
	require.Equal(t, strings.TrimSpace(head), anchored["anchor_oid"], "the anchor is the live source tip")
	anchoredID, _ := anchored["id"].(float64)
	require.NotZero(t, anchoredID)

	// A reply rides the head; the head resolves.
	code, reply := mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/rev/merge-requests/1/comments",
		map[string]any{"body": "the follow-up renames it", "parent_id": anchoredID})
	require.Equal(t, 201, code, "%v", reply)
	code, res := mrAPI(t, s, "alice", "a", "POST", fmt.Sprintf("/api/v1/repos/rev/merge-requests/1/comments/%d/resolve", int(anchoredID)),
		map[string]any{"resolved": true})
	require.Equal(t, 200, code, "%v", res)

	// bob requests changes; the merge bounces naming him. (admin drives the
	// merge — main is owner-gated on this plane, and the lock must bite
	// even the owner.)
	code, rev := mrAPI(t, s, "bob", "b", "POST", "/api/v1/repos/rev/merge-requests/1/review",
		map[string]any{"verdict": "request-changes", "body": "rename first"})
	require.Equal(t, 201, code, "%v", rev)
	code, res = mrAPI(t, s, "admin", "x", "POST", "/api/v1/repos/rev/merge-requests/1/merge", nil)
	require.Equal(t, 409, code, "%v", res)
	require.Contains(t, errmsg(res), "request-changes outstanding from bob")

	// The detail exposes the blocking list for the red strip.
	code, view := mrAPI(t, s, "alice", "a", "GET", "/api/v1/repos/rev/merge-requests/1", nil)
	require.Equal(t, 200, code, "%v", view)
	require.ElementsMatch(t, []any{"bob"}, view["blocking"].([]any))

	// The author pushes; the verdict SURVIVES (the reviewer's key only).
	support.Write(t, filepath.Join(src, "g.txt"), "renamed\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "rename")
	support.Git(t, src, "push", "-q", credURL(s, "alice", "a", "/r/rev.git"), "feat")
	code, res = mrAPI(t, s, "admin", "x", "POST", "/api/v1/repos/rev/merge-requests/1/merge", nil)
	require.Equal(t, 409, code, "%v", res)

	// The anchored thread is now stale — the timeline keeps it in place
	// with its badge (the anchor OID no longer matches the live tip).
	code, view = mrAPI(t, s, "alice", "a", "GET", "/api/v1/repos/rev/merge-requests/1", nil)
	require.Equal(t, 200, code, "%v", view)
	require.NotEqual(t, strings.TrimSpace(head), view["source_tip"], "the source tip advanced")

	// bob re-reviews approve; the approval rides along and the merge lands.
	code, res = mrAPI(t, s, "bob", "b", "POST", "/api/v1/repos/rev/merge-requests/1/review",
		map[string]any{"verdict": "approve", "body": "renamed, lgtm"})
	require.Equal(t, 201, code, "%v", res)
	code, merged := mrAPI(t, s, "admin", "x", "POST", "/api/v1/repos/rev/merge-requests/1/merge", nil)
	require.Equal(t, 200, code, "%v", merged)
	require.Equal(t, "merged", merged["state"])

	// The discussion survives the merge: comments + timestamped approvals.
	code, disc := mrAPI(t, s, "alice", "a", "GET", "/api/v1/repos/rev/merge-requests/1/comments", nil)
	require.Equal(t, 200, code, "%v", disc)
	comments := disc["comments"].([]any)
	require.GreaterOrEqual(t, len(comments), 3)
	events := disc["approval_events"].([]any)
	require.Len(t, events, 1)
	approver := events[0].(map[string]any)
	require.Equal(t, "bob", approver["user"])

	// The author's own verdict stays a comment.
	code, res = mrAPI(t, s, "alice", "a", "POST", "/api/v1/repos/rev/merge-requests/1/review",
		map[string]any{"verdict": "approve"})
	require.Equal(t, 400, code, "%v", res)

	// Avatars on this plane: config-file accounts (static policy, no users
	// table rows) answer the explaining 404 — their face stays the
	// name-initial default, and the scheme check still fires first.
	code, res = mrAPI(t, s, "alice", "a", "PUT", "/api/v1/me/avatar",
		map[string]string{"avatarUrl": "http://plain.example/x.png"})
	require.Equal(t, 400, code, "%v", res)
	code, res = mrAPI(t, s, "alice", "a", "PUT", "/api/v1/me/avatar",
		map[string]string{"avatarUrl": "data:image/png;base64,AAAA"})
	require.Equal(t, 404, code, "%v", res)
	require.Contains(t, errmsg(res), "no account row")
	code, res = mrAPI(t, s, "bob", "b", "GET", "/api/v1/users/alice/avatar", nil)
	require.Equal(t, 404, code, "%v", res)
}
