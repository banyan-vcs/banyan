// Goal: RPC surface — /api/v1/log path filtering with X-view normalization
// and the hash-consistent view (T11).
package integration

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- T11 /api/v1/log path filter (X-view normalization + hash-consistent view) ---
func TestFilteredLogRPC(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "t11")

	// Root commit touches README only (not lib).
	root := cloneRepo(t, s, s.repoURL("t11"))
	write(t, filepath.Join(root, "README.md"), "readme-v2\n")
	git(t, root, "commit", "-qam", "t11: readme only")
	git(t, root, "push", "-q", "origin", "main")

	// Subtree commit touches lib/nested/SHARED.txt.
	sub := cloneRepo(t, s, s.treeURL("t11", "lib/nested"))
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "x\n")
	git(t, sub, "commit", "-qam", "t11: sub touches shared")
	git(t, sub, "push", "-q", "origin", "main")

	q := url.Values{"repo": {"t11"}, "path": {"/lib/nested"}, "limit": {"50"}}
	resp, err := http.Get(s.URL + "/api/v1/log?" + q.Encode())
	require.NoError(t, err)
	defer resp.Body.Close()
	var body struct {
		Entries []struct {
			OID     string `json:"oid"`
			Subject string `json:"subject"`
		} `json:"entries"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	var sawSeed, sawSub, sawReadmeOnly bool
	for _, e := range body.Entries {
		switch {
		case strings.HasPrefix(e.Subject, "seed: initial"):
			sawSeed = true
		case strings.HasPrefix(e.Subject, "t11: sub touches shared"):
			sawSub = true
		case strings.HasPrefix(e.Subject, "t11: readme only"):
			sawReadmeOnly = true // must not appear: this commit never touched the path
		}
	}
	assert.True(t, sawSeed, "过滤 log 缺 seed 提交 (entries=%d)", len(body.Entries))
	assert.True(t, sawSub, "过滤 log 缺子树提交 (entries=%d)", len(body.Entries))
	assert.False(t, sawReadmeOnly, "过滤 log 不应包含未触碰该路径的提交（t11: readme only）")
}
