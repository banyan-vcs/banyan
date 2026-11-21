package rpc

// The member management API face (docs/design/data-model.md §16): the group
// table (what labeled repositories inherit), the repository's effective
// view with inherited rows marked viaGroup, the strictly-above guard on
// direct grants, and removal semantics (rank "" — falling back to the
// inherited rank).
import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func putMember(t *testing.T, hsURL, path, user string, body map[string]string) *http.Response {
	t.Helper()
	return apiAs(t, hsURL, http.MethodPut, path, user, body)
}

func getMembers(t *testing.T, hsURL, repo, user string) (effective []map[string]any, direct []map[string]any) {
	t.Helper()
	resp := apiAs(t, hsURL, http.MethodGet, "/api/v1/repos/"+repo+"/members", user, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Members []map[string]any `json:"members"`
		Direct  []map[string]any `json:"direct"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Members, out.Direct
}

func TestMemberEndpoints(t *testing.T) {
	_, _, hs := newAuthStack(t)

	// The world: group eng, repo svc labeled eng.
	resp := apiAs(t, hs.URL, http.MethodPost, "/api/v1/groups", "alice", map[string]string{"name": "eng"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos", "boss", map[string]string{"name": "svc", "group": "eng"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// --- the group member table: set, replace, list ---
	resp = putMember(t, hs.URL, "/api/v1/groups/eng/members", "alice", map[string]string{"user": "alice", "rank": "owner"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = putMember(t, hs.URL, "/api/v1/groups/eng/members", "alice", map[string]string{"user": "bob", "rank": "viewer"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = putMember(t, hs.URL, "/api/v1/groups/eng/members", "alice", map[string]string{"user": "bob", "rank": "developer"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "replace in place")
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/groups/eng/members", "boss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var gm struct {
		Members []map[string]any `json:"members"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&gm))
	require.Len(t, gm.Members, 2)
	assert.Equal(t, "alice", gm.Members[0]["user"])
	assert.Equal(t, "developer", gm.Members[1]["rank"])
	// an unknown group's table is not a 404 on GET (empty list) but the
	// write bounces NOT_FOUND via ErrGroupUnknown
	resp = putMember(t, hs.URL, "/api/v1/groups/missing/members", "boss", map[string]string{"user": "x", "rank": "viewer"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	// bad rank is client input
	resp = putMember(t, hs.URL, "/api/v1/groups/eng/members", "boss", map[string]string{"user": "x", "rank": "boss"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// --- the effective view: inherited rows carry viaGroup ---
	effective, direct := getMembers(t, hs.URL, "svc", "boss")
	require.Len(t, effective, 2)
	assert.Equal(t, "eng", effective[0]["viaGroup"], "alice's row is inherited")
	assert.Equal(t, "owner", effective[0]["rank"])
	assert.Equal(t, "eng", effective[1]["viaGroup"])
	assert.Empty(t, direct, "no direct members yet")

	// --- the inheritance guard on direct grants ---
	resp = putMember(t, hs.URL, "/api/v1/repos/svc/members", "alice", map[string]string{"user": "bob", "rank": "developer"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "equal to inherited bounces")
	resp = putMember(t, hs.URL, "/api/v1/repos/svc/members", "alice", map[string]string{"user": "bob", "rank": "viewer"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "below inherited bounces")
	resp = putMember(t, hs.URL, "/api/v1/repos/svc/members", "alice", map[string]string{"user": "bob", "rank": "owner"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "strictly above passes")
	resp = putMember(t, hs.URL, "/api/v1/repos/svc/members", "alice", map[string]string{"user": "carol", "rank": "developer"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "no inheritance: any rank")

	// effective now: alice inherited owner, bob direct owner, carol direct developer
	effective, direct = getMembers(t, hs.URL, "svc", "boss")
	require.Len(t, effective, 3)
	assert.Equal(t, "eng", effective[0]["viaGroup"])
	assert.Empty(t, effective[1]["viaGroup"], "bob's direct row wins the display")
	assert.Equal(t, "owner", effective[1]["rank"])
	assert.Len(t, direct, 2)

	// unknown repo 404s
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos/no-such/members", "boss", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// --- removal: rank "" falls back to the inherited rank ---
	resp = putMember(t, hs.URL, "/api/v1/repos/svc/members", "alice", map[string]string{"user": "bob", "rank": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	effective, _ = getMembers(t, hs.URL, "svc", "boss")
	require.Len(t, effective, 3)
	for _, row := range effective {
		if row["user"] == "bob" {
			assert.Equal(t, "eng", row["viaGroup"], "bob falls back to his inherited developer row")
			assert.Equal(t, "developer", row["rank"])
		}
	}
	// removing a non-member bounces
	resp = putMember(t, hs.URL, "/api/v1/repos/svc/members", "alice", map[string]string{"user": "ghost", "rank": ""})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// --- group delete cascades the member table and the inherited grants ---
	resp = apiAs(t, hs.URL, http.MethodDelete, "/api/v1/groups/eng", "boss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	effective, _ = getMembers(t, hs.URL, "svc", "boss")
	// alice's and bob's inherited rows die with the group; carol's direct
	// developer grant survives — the repo stays governed by it.
	require.Len(t, effective, 1)
	assert.Equal(t, "carol", effective[0]["user"])
	assert.Empty(t, effective[0]["viaGroup"])
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/groups/eng/members", "boss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&gm))
	assert.Empty(t, gm.Members)
}
