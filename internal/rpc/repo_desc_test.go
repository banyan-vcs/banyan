package rpc

// The repository description face: creation carries it, the list answers
// it (plus the best-effort updatedAt — 0 for an empty repository), and the
// PUT endpoint rewrites it at write rank.
import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoDescriptionEndpoints(t *testing.T) {
	_, _, hs := newAuthStack(t)

	// Create with a description; the list carries it and the activity of
	// an empty repository answers 0 (omitted on the wire).
	resp := apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos", "boss", map[string]string{
		"name": "svc", "description": "the auth service",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos", "alice", nil)
	require.Equal(t, http.StatusOK, list.StatusCode)
	var out struct {
		Repos []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			UpdatedAt   int64  `json:"updatedAt"`
		} `json:"repos"`
	}
	require.NoError(t, json.NewDecoder(list.Body).Decode(&out))
	require.Len(t, out.Repos, 1)
	assert.Equal(t, "the auth service", out.Repos[0].Description)
	assert.Equal(t, int64(0), out.Repos[0].UpdatedAt, "empty repository: no activity yet")

	// GET answers it; PUT rewrites at write rank (a member can — display
	// metadata, the group-label precedent); clearing works.
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos/svc/description", "alice", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/svc/description", "alice", map[string]string{"description": "the auth service (v2)"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list = apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos", "boss", nil)
	require.NoError(t, json.NewDecoder(list.Body).Decode(&out))
	assert.Equal(t, "the auth service (v2)", out.Repos[0].Description)
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/svc/description", "boss", map[string]string{"description": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// An unknown repo answers 404 on both verbs.
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/repos/nope/description", "boss", map[string]string{"description": "x"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
