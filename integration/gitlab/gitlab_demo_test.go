// T83 — GitLab-CE e2e over banyan storage (docs/integration/gitaly-compat.md §8, opt-in): a real
// GitLab whose only gitaly node is banyan (demo/gitlab/docker-compose.yml)
// must take a project create (CreateRepository), an HTTP push (workhorse →
// PostReceivePack), and the API read-backs its web UI makes (root-ref
// resolution, GetTreeEntries with path ".", ListCommits, GetBlob, archive
// via GetArchive). The six protocol seams this demo surfaced — v2 HMAC
// bearer tokens, fully-qualified default branch names, for-each-ref
// pattern prefixes, the "." tree path, structured FindTag errors, and
// include_lfs_blobs on pointer-free trees — each have their pinned case in
// the unit/integration layers; this test is the whole loop against the
// live stack.
//
// Skipped unless the demo is up and a token is provided:
//
//	BANYAN_GITLAB_URL=http://localhost:8180 \
//	BANYAN_GITLAB_TOKEN=<root PAT>  (demo/gitlab/provision.sh mints one)
package gitlab

import (
	"github.com/banyan-vcs/banyan/integration/support"

	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitlabDemo loads the opt-in env; ok=false keeps the test skipped.
func gitlabDemo(t *testing.T) (baseURL, token string, ok bool) {
	t.Helper()
	baseURL = os.Getenv("BANYAN_GITLAB_URL")
	token = os.Getenv("BANYAN_GITLAB_TOKEN")
	if baseURL == "" || token == "" {
		t.Skip("opt-in: set BANYAN_GITLAB_URL + BANYAN_GITLAB_TOKEN against demo/gitlab")
	}
	return baseURL, token, true
}

func TestT83GitLabCEE2E(t *testing.T) {
	support.NeedGit(t)
	baseURL, token, ok := gitlabDemo(t)
	if !ok {
		return
	}
	api := func(method, path, body string) (int, []byte) {
		t.Helper()
		var rd *strings.Reader
		if body != "" {
			rd = strings.NewReader(body)
		} else {
			rd = strings.NewReader("")
		}
		req, _ := http.NewRequest(method, baseURL+"/api/v4"+path, rd)
		req.Header.Set("PRIVATE-TOKEN", token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			raw = append(raw, buf[:n]...)
			if err != nil {
				break
			}
		}
		return resp.StatusCode, raw
	}

	// Fresh project per run — Rails → gitaly CreateRepository → banyan.
	name := fmt.Sprintf("t83-%d", time.Now().UnixNano())
	code, raw := api("POST", "/projects", fmt.Sprintf(`{"name":%q,"path":%q,"visibility":"private"}`, name, name))
	require.Equal(t, http.StatusCreated, code, "create project: %s", raw)
	var proj struct {
		ID int `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &proj))
	t.Cleanup(func() { _, _ = api("DELETE", fmt.Sprintf("/projects/%d", proj.ID), "") })

	// HTTP push through workhorse → banyan PostReceivePack.
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "t83 through gitlab-ce on banyan\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "-c", "user.name=T83", "-c", "user.email=t83@example.com", "commit", "-qm", "seed")
	pushURL := strings.Replace(baseURL, "http://", "http://oauth2:"+token+"@", 1)
	// user-namespace projects need the full path in the git URL
	support.Git(t, src, "push", "-q", pushURL+"/root/"+name+".git", "main")

	// The read-backs the web UI makes. Each one exercised a seam this demo
	// broke before the fixes: default-branch resolution (FindDefaultBranchName
	// full names), tree at "." (GetTreeEntries), commits (ListCommits),
	// raw blob (GetBlob + TreeEntry).
	code, raw = api("GET", fmt.Sprintf("/projects/%d", proj.ID), "")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, string(raw), `"default_branch":"main"`)

	code, raw = api("GET", fmt.Sprintf("/projects/%d/repository/tree?ref=main", proj.ID), "")
	require.Equal(t, http.StatusOK, code, "tree: %s", raw)
	assert.Contains(t, string(raw), `"name":"README.md"`, "path \".\" must list the root tree")

	code, raw = api("GET", fmt.Sprintf("/projects/%d/repository/commits?ref=main", proj.ID), "")
	require.Equal(t, http.StatusOK, code, "commits: %s", raw)
	assert.Contains(t, string(raw), `"title":"seed"`)

	code, raw = api("GET", fmt.Sprintf("/projects/%d/repository/files/README.md/raw?ref=main", proj.ID), "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "t83 through gitlab-ce on banyan\n", string(raw))

	// Archive rides workhorse → GetArchive with include_lfs_blobs=true
	// (the pointer-free gate).
	code, raw = api("GET", fmt.Sprintf("/projects/%d/repository/archive.tar.gz", proj.ID), "")
	require.Equal(t, http.StatusOK, code, "archive: %s", raw)
	assert.True(t, len(raw) > 100, "archive bytes must arrive")
}
