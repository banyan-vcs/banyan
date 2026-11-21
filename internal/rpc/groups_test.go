package rpc

// The groups API face (docs/design/data-model.md §15): create with the
// repo-creation rank, the list carrying the assignment table, the label
// endpoints (write rank — organizational metadata only), and delete
// clearing assignments.
import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getGroups(t *testing.T, hsURL, user string) (groups []string, repoGroups map[string]string) {
	t.Helper()
	resp := apiAs(t, hsURL, http.MethodGet, "/api/v1/groups", user, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Groups []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			CreatedBy   string `json:"createdBy"`
		} `json:"groups"`
		RepoGroups map[string]string `json:"repoGroups"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	names := make([]string, 0, len(out.Groups))
	for _, g := range out.Groups {
		names = append(names, g.Name)
	}
	return names, out.RepoGroups
}

func TestGroupEndpoints(t *testing.T) {
	_, _, hs := newAuthStack(t)

	// Create rides the repo-creation rank: a member can, a bad name bounces.
	resp := apiAs(t, hs.URL, http.MethodPost, "/api/v1/groups", "alice", map[string]string{"name": "eng", "description": "engineering"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/groups", "boss", map[string]string{"name": "a/b"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/groups", "boss", map[string]string{"name": "eng"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "duplicate group is CONFLICT_GROUP")

	// Creation with a group label is all-or-nothing: an unknown group rolls
	// the half-created repo back.
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos", "boss", map[string]string{"name": "svc", "group": "eng"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos", "boss", map[string]string{"name": "orphan", "group": "missing"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos/orphan/group", "boss", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "rolled-back repo is gone")

	// The list carries groups (sorted) and the assignment table.
	names, repoGroups := getGroups(t, hs.URL, "alice")
	assert.Equal(t, []string{"eng"}, names)
	assert.Equal(t, "eng", repoGroups["svc"])

	// The label move is write rank (organizational metadata only): a member
	// can set and clear it; an unknown repo 404s, an unknown group 404s.
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/svc/group", "alice", map[string]string{"group": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, repoGroups = getGroups(t, hs.URL, "boss")
	assert.NotContains(t, repoGroups, "svc")
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/svc/group", "alice", map[string]string{"group": "eng"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos/svc/group", "alice", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var label struct {
		Repo  string `json:"repo"`
		Group string `json:"group"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&label))
	assert.Equal(t, "eng", label.Group)
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/no-such/group", "boss", map[string]string{"group": "eng"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/svc/group", "boss", map[string]string{"group": "missing"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Deleting the group clears its assignments in the same breath.
	resp = apiAs(t, hs.URL, http.MethodDelete, "/api/v1/groups/eng", "alice", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, repoGroups = getGroups(t, hs.URL, "boss")
	assert.NotContains(t, repoGroups, "svc", "delete must clear assignments")
	resp = apiAs(t, hs.URL, http.MethodDelete, "/api/v1/groups/eng", "boss", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
