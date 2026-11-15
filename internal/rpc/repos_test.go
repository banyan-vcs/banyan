package rpc

// The project-hierarchy surface (docs/design/data-model.md §14): creation
// with a parent (write rank, all-or-nothing), the parent read/move endpoint
// (admin territory like the other repo-management rows), and the validation
// matrix the store enforces surfacing as the right status codes.
import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiAs issues a JSON request with the stack's basic credentials.
func apiAs(t *testing.T, hsURL, method, path, user string, body any) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, hsURL+path, rdr)
	require.NoError(t, err)
	req.SetBasicAuth(user, "pw")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func repoParents(t *testing.T, hsURL, user string) map[string]string {
	t.Helper()
	resp := apiAs(t, hsURL, http.MethodGet, "/api/v1/repos", user, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Repos []struct {
			ID     string `json:"id"`
			Parent string `json:"parent"`
		} `json:"repos"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	parents := map[string]string{}
	for _, r := range out.Repos {
		parents[r.ID] = r.Parent
	}
	return parents
}

func TestRepoHierarchyEndpoints(t *testing.T) {
	_, _, hs := newAuthStack(t)

	// Three levels ride on creation: root → child → grandchild.
	for _, tt := range []struct {
		name   string
		parent string
		user   string
		want   int
	}{
		{"monorepo", "", "boss", http.StatusOK},
		{"svc-auth", "monorepo", "boss", http.StatusOK},
		{"svc-auth-lib", "svc-auth", "boss", http.StatusOK},
	} {
		body := map[string]string{"name": tt.name}
		if tt.parent != "" {
			body["parent"] = tt.parent
		}
		resp := apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos", tt.user, body)
		require.Equal(t, tt.want, resp.StatusCode, "create %s", tt.name)
	}
	parents := repoParents(t, hs.URL, "boss")
	assert.Equal(t, "", parents["monorepo"])
	assert.Equal(t, "monorepo", parents["svc-auth"])
	assert.Equal(t, "svc-auth", parents["svc-auth-lib"])

	// Self-parenting is refused before anything is written.
	resp := apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos", "boss", map[string]string{"name": "loop", "parent": "loop"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// A missing parent fails and rolls the half-created repo back — creation
	// is all-or-nothing.
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos", "boss", map[string]string{"name": "orphan", "parent": "no-such-repo"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.NotContains(t, repoParents(t, hs.URL, "boss"), "orphan")

	// Re-homing is repo management: admin moves, member is refused.
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/svc-auth-lib/parent", "boss", map[string]string{"parent": "monorepo"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Repo   string `json:"repo"`
		Parent string `json:"parent"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "monorepo", out.Parent)
	assert.Equal(t, "monorepo", repoParents(t, hs.URL, "boss")["svc-auth-lib"])

	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/svc-auth-lib/parent", "alice", map[string]string{"parent": ""})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "re-homing is admin territory")

	// GET reports the link; the store's cycle guard surfaces as 400.
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos/svc-auth-lib/parent", "boss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/monorepo/parent", "boss", map[string]string{"parent": "svc-auth-lib"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "ancestor under its own descendant is a cycle")

	// A read-scope PAT cannot create a repo — parent or not, creation is a
	// metadata-plane write (write-rank gate, docs/gateway/authorization.md §8b).
	read := mintAs(t, hs, "alice", "pw", "read")
	require.Equal(t, http.StatusForbidden, statusAs(t, hs, http.MethodPost, "/api/v1/repos", "alice", read, `{"name":"pat-child","parent":"monorepo"}`))
}
