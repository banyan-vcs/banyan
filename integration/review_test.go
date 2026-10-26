// review_test.go (T51, docs/17 §review): lightweight code review with the
// anchor model doing the isolating — two subprojects run review lifecycles
// in parallel, and neither side's reviewers can see, comment on, or verdict
// the other's reviews; the root member sees both. Real git clients produce
// the feature branches under review.
package integration

import (
	"fmt"
	"path/filepath"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewSubprojectIsolation(t *testing.T) {
	needGit(t)
	s := newConsoleStack(t)
	root := s.addUser(t, "root", true)
	s.addUser(t, "alice", false) // root-scope developer
	s.addUser(t, "bob", false)   // auth author
	s.addUser(t, "carol", false) // auth reviewer
	s.addUser(t, "erin", false)  // db author
	s.addUser(t, "dave", false)  // db reviewer

	// --- project setup: two registered subprojects, per-anchor teams ---
	code, out := s.call(t, "POST", "/api/v1/groups", root, map[string]string{"name": "mobile"})
	require.Equal(t, 201, code, "%v", out)
	code, out = s.call(t, "POST", "/api/v1/repos/mobile", root, map[string]string{"name": "app"})
	require.Equal(t, 201, code, "%v", out)
	code, _ = s.call(t, "PUT", "/api/v1/repos/mobile/app/members/alice", root, map[string]string{"role": "developer"})
	require.Equal(t, 200, code)
	code, _ = s.call(t, "PATCH", "/api/v1/repos/mobile/app", root, map[string][]string{"protected_branches": {}})
	require.Equal(t, 200, code)

	aliceDir := t.TempDir()
	git(t, aliceDir, "clone", s.gitURL("/mobile/app.git", "alice"), "repo")
	work := filepath.Join(aliceDir, "repo")
	write(t, filepath.Join(work, "services", "auth", "token.go"), "package auth\n\nfunc Issue() string { return \"v1\" }\n")
	write(t, filepath.Join(work, "services", "db", "store.go"), "package db\n\nfunc Get() string { return \"v1\" }\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "seed")
	git(t, work, "push", "origin", "HEAD:refs/heads/main")

	code, _ = s.call(t, "POST", "/api/v1/repos/mobile/app/services/auth", root, nil)
	require.Equal(t, 201, code)
	code, _ = s.call(t, "POST", "/api/v1/repos/mobile/app/services/db", root, nil)
	require.Equal(t, 201, code)

	for _, grant := range []struct{ anchor, user, role string }{
		{"services/auth", "bob", "developer"},
		{"services/auth", "carol", "maintainer"},
		{"services/db", "erin", "developer"},
		{"services/db", "dave", "maintainer"},
	} {
		code, _ = s.call(t, "PUT", "/api/v1/repos/mobile/app/"+grant.anchor+"/members/"+grant.user,
			root, map[string]string{"role": grant.role})
		require.Equal(t, 200, code, "grant %s/%s", grant.anchor, grant.user)
	}

	// --- both authors propose changes on their own subproject ---
	// The v5 collaboration story is timeline-first: a subproject's work goes
	// to its own main. The review baseline is a frozen root-branch pointer
	// at the pre-change state (any root branch projects onto every anchor,
	// docs/02 §12); the proposal is the author's pushed main.
	git(t, work, "push", "origin", "main:refs/heads/auth-baseline")
	git(t, work, "push", "origin", "main:refs/heads/db-baseline")
	authReview := proposeSubtreeChange(t, s, "bob", "services/auth", "auth-baseline", "token.go",
		"package auth\n\nfunc Issue() string { return \"v2-reviewed\" }\n", "auth: rotate issue")
	dbReview := proposeSubtreeChange(t, s, "erin", "services/db", "db-baseline", "store.go",
		"package db\n\nfunc Get() string { return \"v2-reviewed\" }\n", "db: bump get")

	// --- isolation: neither reviewer can reach the other's review ---
	// carol (auth) sees only the auth review at her anchor.
	code, out = s.call(t, "GET", "/api/v1/repos/mobile/app/services/auth/reviews", s.tokens["carol"], nil)
	require.Equal(t, 200, code)
	require.Len(t, out["reviews"], 1)
	require.Equal(t, "auth: rotate issue", firstTitle(t, out))
	// dave (db) sees only the db review at his.
	code, out = s.call(t, "GET", "/api/v1/repos/mobile/app/services/db/reviews", s.tokens["dave"], nil)
	require.Equal(t, 200, code)
	require.Len(t, out["reviews"], 1)
	require.Equal(t, "db: bump get", firstTitle(t, out))

	// dave reaching the AUTH review by its real id through the db URL is a
	// 404 (the review's anchor is outside that subtree), and through the
	// auth URL too (he has no role there).
	code, _ = s.call(t, "GET", fmt.Sprintf("/api/v1/repos/mobile/app/services/db/reviews/%d", authReview), s.tokens["dave"], nil)
	require.Equal(t, 404, code)
	code, _ = s.call(t, "GET", fmt.Sprintf("/api/v1/repos/mobile/app/services/auth/reviews/%d", authReview), s.tokens["dave"], nil)
	require.Equal(t, 404, code)
	code, _ = s.call(t, "POST", fmt.Sprintf("/api/v1/repos/mobile/app/services/auth/reviews/%d/comments", authReview),
		s.tokens["dave"], map[string]string{"body": "snooping"})
	require.Equal(t, 404, code)
	code, _ = s.call(t, "POST", fmt.Sprintf("/api/v1/repos/mobile/app/services/auth/reviews/%d/approve", authReview),
		s.tokens["dave"], nil)
	require.Equal(t, 404, code)

	// --- parallel lifecycles, zero interference ---
	// The author cannot self-approve.
	code, _ = s.call(t, "POST", fmt.Sprintf("/api/v1/repos/mobile/app/services/auth/reviews/%d/approve", authReview),
		s.tokens["bob"], nil)
	require.Equal(t, 403, code)

	// carol reviews auth: comment then approve.
	code, _ = s.call(t, "POST", fmt.Sprintf("/api/v1/repos/mobile/app/services/auth/reviews/%d/comments", authReview),
		s.tokens["carol"], map[string]string{"body": "lgtm modulo constants"})
	require.Equal(t, 201, code)
	code, out = s.call(t, "POST", fmt.Sprintf("/api/v1/repos/mobile/app/services/auth/reviews/%d/approve", authReview),
		s.tokens["carol"], nil)
	require.Equal(t, 200, code, "%v", out)
	require.Equal(t, "approved", out["status"])

	// dave reviews db independently — same instant, different verdict flavor.
	code, out = s.call(t, "POST", fmt.Sprintf("/api/v1/repos/mobile/app/services/db/reviews/%d/approve", dbReview),
		s.tokens["dave"], nil)
	require.Equal(t, 200, code, "%v", out)
	require.Equal(t, "approved", out["status"])

	// The auth review carries exactly carol's events; dave's snooping
	// comment never landed.
	code, out = s.call(t, "GET", fmt.Sprintf("/api/v1/repos/mobile/app/services/auth/reviews/%d", authReview),
		s.tokens["carol"], nil)
	require.Equal(t, 200, code)
	events := out["events"].([]any)
	require.Len(t, events, 2) // carol's comment + approve
	require.NotContains(t, fmt.Sprint(events), "snooping")

	// --- the root member sees both; the diff view shows the right content ---
	code, out = s.call(t, "GET", "/api/v1/repos/mobile/app/reviews", s.tokens["alice"], nil)
	require.Equal(t, 200, code)
	require.Len(t, out["reviews"], 2, "root-scope listing aggregates the subtree reviews")

	code, out = s.call(t, "GET", fmt.Sprintf("/api/v1/repos/mobile/app/services/auth/reviews/%d/diff", authReview),
		s.tokens["carol"], nil)
	require.Equal(t, 200, code, "%v", out)
	files := out["files"].([]any)
	require.Len(t, files, 1)
	f0 := files[0].(map[string]any)
	// Anchor-relative path and real modification (D1 post-mortem: the old
	// "nested path" shape was the test helper's anchor-nested write, not an
	// engine defect — see TestSubtreeViewCrossScopeIsolation).
	require.Equal(t, "token.go", f0["path"].(string))
	require.Equal(t, "modified", f0["status"].(string))
	hunks := f0["hunks"].([]any)
	require.NotEmpty(t, hunks)
	require.Contains(t, fmt.Sprint(hunks), "v2-reviewed")

	code, out = s.call(t, "GET", fmt.Sprintf("/api/v1/repos/mobile/app/services/db/reviews/%d/diff", dbReview),
		s.tokens["dave"], nil)
	require.Equal(t, 200, code, "%v", out)
	files = out["files"].([]any)
	require.Len(t, files, 1)
	require.Equal(t, "store.go", files[0].(map[string]any)["path"].(string))
	require.Equal(t, "modified", files[0].(map[string]any)["status"].(string))
}

// proposeSubtreeChange drives one author's full proposal: clone the
// subtree view, edit the file at the CLONE ROOT (the anchor's scoped view —
// a scoped clone's natural commit form), push the anchor's main (the
// everyday v5 flow), open a review of that push against the frozen baseline
// branch. Writing the anchor-nested path instead (sub/services/auth/x.go)
// would create a MIXED-SCOPE commit the views faithfully project — nested
// paths in the subtree view and services/auth/services/auth/... in the root
// weave — which is content, not corruption (xviewTree's mixed-scope rule).
func proposeSubtreeChange(t *testing.T, s *consoleStack, author, anchor, baseline, file, newContent, title string) int64 {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "clone", s.gitURL("/mobile/app/"+anchor+".git", author), "sub")
	sub := filepath.Join(dir, "sub")
	write(t, filepath.Join(sub, file), newContent)
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-m", title)
	git(t, sub, "push")

	code, out := s.call(t, "POST", "/api/v1/repos/mobile/app/"+anchor+"/reviews", s.tokens[author],
		map[string]string{"title": title, "source_ref": "main", "base_ref": baseline})
	require.Equal(t, 201, code, "%s review create: %v", author, out)
	id, _ := out["id"].(float64)
	require.NotZero(t, id)
	return int64(id)
}

func firstTitle(t *testing.T, out map[string]any) string {
	t.Helper()
	revs := out["reviews"].([]any)
	require.NotEmpty(t, revs)
	return revs[0].(map[string]any)["title"].(string)
}
