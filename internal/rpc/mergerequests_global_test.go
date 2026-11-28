package rpc

// The workspace-wide merge-request list (the console sidebar's cross-repo
// view): every repo's ledger rows in one answer, newest update first,
// with the state filter carrying through.
import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobalMergeRequestList(t *testing.T) {
	_, ms, hs := newAuthStack(t)
	ctx := context.Background()
	require.NoError(t, ms.CreateRepo(ctx, "alpha", model.SHA1))
	require.NoError(t, ms.CreateRepo(ctx, "beta", model.SHA1))
	seed := func(repo, source, title string, state string, updated int64) {
		_, err := ms.CreateMergeRequest(ctx, meta.MergeRequestInfo{
			Repo: repo, Source: source, Target: "main", Title: title,
			State: state, Author: "alice", SourceHead: "ab" + title[:1], CreatedAt: updated - 1, UpdatedAt: updated,
		})
		require.NoError(t, err)
	}
	seed("alpha", "feat-a", "oldest", meta.MergeRequestStateOpen, 5)
	seed("beta", "feat-b", "newest", meta.MergeRequestStateOpen, 9)
	seed("alpha", "feat-c", "closed one", meta.MergeRequestStateClosed, 7)

	get := func(state string) []meta.MergeRequestInfo {
		t.Helper()
		url := "/api/v1/merge-requests"
		if state != "" {
			url += "?state=" + state
		}
		resp := apiAs(t, hs.URL, http.MethodGet, url, "alice", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out struct {
			MergeRequests []meta.MergeRequestInfo `json:"merge_requests"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out.MergeRequests
	}

	// All states, newest update first, every row carrying its repo.
	all := get("")
	require.Len(t, all, 3)
	assert.Equal(t, "newest", all[0].Title)
	assert.Equal(t, "beta", all[0].Repo)
	assert.Equal(t, "closed one", all[1].Title)
	assert.Equal(t, "oldest", all[2].Title)

	// The state filter carries through per repo.
	open := get("open")
	require.Len(t, open, 2)
	assert.Equal(t, "newest", open[0].Title)
	assert.Equal(t, "oldest", open[1].Title)
}
