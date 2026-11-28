package rpc

// The review face end to end (docs/design/merge-requests.md §review): real
// commits land through the service, the JSON face walks the whole review
// flow — anchored comment (the server stamps the anchor), reply, resolve,
// request-changes blocking the merge with 409, the re-review unblocking,
// the merge landing, the discussion surviving it — and the avatar routes
// round-trip the account column.
import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// seedMergeRequestRepo lands main + feat with one file each through the real commit
// path, then opens the merge request over the JSON face.
func seedMergeRequestRepo(t *testing.T, s *Server, hsURL string) int64 {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.Svc.Meta.CreateRepo(ctx, "r", model.SHA1))
	mk := func(branch string, base model.OID, path, content, msg string) model.OID {
		t.Helper()
		if !base.IsZero() {
			require.NoError(t, s.Svc.Meta.EnsureTimeline(ctx, model.Timeline{Repo: "r", Anchor: model.RootAnchor, Name: branch}, base, ""))
		}
		blob := model.ObjectOID(model.BlobType, []byte(content), model.SHA1)
		require.NoError(t, s.Svc.PutObjects(ctx, "r", []service.PutObject{{OID: blob, Type: "blob", Data: []byte(content)}}))
		res, err := s.Svc.CreateCommit(ctx, service.CommitRequest{
			Repo: "r", Branch: branch, Base: base, Message: msg,
			Changes: []service.Change{{Path: path, Blob: blob}},
		})
		require.NoError(t, err)
		return res.Head
	}
	main := mk("main", model.OID(""), "readme", "seed\n", "seed")
	mk("feat", main, "feat.go", "package feat\n", "work")
	resp := apiAs(t, hsURL, http.MethodPost, "/api/v1/repos/r/merge-requests",
		"alice", map[string]string{"source": "feat", "target": "main", "title": "feat → main"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var mergeRequest meta.MergeRequestInfo
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&mergeRequest))
	return mergeRequest.IID
}

func TestMergeRequestReviewFlow(t *testing.T) {
	s, _, hs := newAuthStack(t)
	seedMergeRequestRepo(t, s, hs.URL)

	// alice (author) comments overview-level; boss anchors a line comment.
	resp := apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/comments",
		"alice", map[string]any{"body": "first pass — please look at the package clause"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/comments",
		"boss", map[string]any{"body": "name it features", "path": "feat.go", "side": "new", "line": 1})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var anchored meta.MergeRequestComment
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&anchored))
	assert.NotEmpty(t, anchored.AnchorOID, "the face stamps the anchor server-side")
	st, err := s.Svc.Meta.Timeline(context.Background(), model.Timeline{Repo: "r", Anchor: model.RootAnchor, Name: "feat"})
	require.NoError(t, err)
	assert.Equal(t, string(st.Head), anchored.AnchorOID)

	// alice replies; alice resolves her own thread start? — boss's head, alice
	// resolves (any push-rank member in v1).
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/comments",
		"alice", map[string]any{"body": "renamed in the follow-up", "parent_id": anchored.ID})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/comments/2/resolve",
		"alice", map[string]any{"resolved": true})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// boss requests changes; the merge bounces with the blocking reviewer.
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/review",
		"boss", map[string]any{"verdict": "request-changes", "body": "the package name"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/merge", "alice", nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	assert.Contains(t, env.Error.Message, "request-changes outstanding from boss")

	// The detail carries the blocking list (the merge box's red strip).
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos/r/merge-requests/1", "alice", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var view struct {
		Blocking []string `json:"blocking"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&view))
	assert.Equal(t, []string{"boss"}, view.Blocking)

	// boss re-reviews approve — the approval rides along, the merge lands.
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/review",
		"boss", map[string]any{"verdict": "approve", "body": "lgtm"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/merge", "alice", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The timeline: comments survive the merge; approval events carry times.
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos/r/merge-requests/1/comments", "alice", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var disc struct {
		Comments       []meta.MergeRequestComment  `json:"comments"`
		ApprovalEvents []meta.MergeRequestApproval `json:"approval_events"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&disc))
	assert.GreaterOrEqual(t, len(disc.Comments), 3, "the discussion survives the merge")
	require.Len(t, disc.ApprovalEvents, 1)
	assert.Equal(t, "boss", disc.ApprovalEvents[0].User)

	// The author's self-approval refusal carries to the review face.
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/r/merge-requests/1/review",
		"alice", map[string]any{"verdict": "approve"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAvatarRoutes(t *testing.T) {
	_, ms, hs := newAuthStack(t)
	require.NoError(t, ms.PutUser(context.Background(), meta.User{Name: "alice", Password: "hash", CreatedAt: 1}))

	// PUT rounds the column through the seam.
	resp := apiAs(t, hs.URL, http.MethodPut, "/api/v1/me/avatar",
		"alice", map[string]string{"avatarUrl": "data:image/png;base64,AAAA"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	u, err := ms.UserByName(context.Background(), "alice")
	require.NoError(t, err)
	assert.Equal(t, "data:image/png;base64,AAAA", u.AvatarURL)

	// The per-name lookup answers the stored value (empty included).
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/users/alice/avatar", "boss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		AvatarURL string `json:"avatarUrl"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "data:image/png;base64,AAAA", out.AvatarURL)

	// Bogus scheme and oversized both answer INVALID_ARG; clearing works.
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/me/avatar",
		"alice", map[string]string{"avatarUrl": "http://insecure.example/x.png"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/me/avatar",
		"alice", map[string]string{"avatarUrl": "data:image/png;base64," + repeat("A", meta.MaxAvatarURL+1)})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPut, "/api/v1/me/avatar", "alice", map[string]string{"avatarUrl": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/users/alice/avatar", "boss", nil)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Empty(t, out.AvatarURL)
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
