package rpc

// Password self-service (docs/gateway/authorization.md §2a): the caller
// re-verifies the current credential explicitly, the new one takes effect
// immediately (the wired policy's verified-credential cache is dropped
// through the decorator chain), config-file accounts answer 400 with the
// reason, and the floor stops trivial passwords.
import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestPasswordChange(t *testing.T) {
	// CI runner CPU: hashing at MinCost — see auth.BcryptCost.
	auth.BcryptCost = bcrypt.MinCost
	t.Cleanup(func() { auth.BcryptCost = bcrypt.DefaultCost })
	ms := meta.NewMemoryMetaStore()
	objects := storage.NewObjectService(ms, storage.NewLocalShardStore(t.TempDir()), []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	s := NewServer(ms, objects, model.SHA1)
	// Production shape: keys → members → subtree rules → tokens → the
	// static policy, the account table wired in — the invalidation must
	// survive the WHOLE chain (a missing forwarder on any decorator would
	// leave the old password alive until TTL lapse; the subtree-rule layer
	// once did exactly that, caught live on the demo stack).
	static := &auth.StaticPolicy{Users: map[string]string{"bob": "cfg-pass"}}
	static.UserSource = ms
	hash, err := auth.HashPassword("first-password")
	require.NoError(t, err)
	require.NoError(t, ms.PutUser(t.Context(), meta.User{Name: "alice", Password: hash, Admin: true, CreatedAt: 1}))
	s.Auth = &auth.KeyPolicy{Inner: &auth.MemberPolicy{Inner: &auth.SubtreeRulePolicy{Inner: &auth.TokenPolicy{Inner: static, Tokens: ms}, Rules: ms}, Members: ms}, Keys: ms}
	mux := http.NewServeMux()
	s.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)

	// change posts the password request. authPass is the session's Basic
	// credential (valid); the body carries the explicit current check —
	// the two are the same in real sessions but the face verifies the body
	// on its own, and the wrong-current refusal needs a valid session.
	change := func(user, authPass, current, next string) *http.Response {
		body, err := json.Marshal(map[string]string{"current": current, "next": next})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/auth/password", bytes.NewReader(body))
		require.NoError(t, err)
		req.SetBasicAuth(user, authPass)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	// --- the happy path: current verified, new stored, effective at once ---
	resp := change("alice", "first-password", "first-password", "rotated-pass-1")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// the OLD password dies immediately (cache invalidated through the chain)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/v1/repos", nil)
	req.SetBasicAuth("alice", "first-password")
	old, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	old.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, old.StatusCode, "old password must die immediately")
	req.SetBasicAuth("alice", "rotated-pass-1")
	fresh, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	fresh.Body.Close()
	assert.Equal(t, http.StatusOK, fresh.StatusCode, "new password must authenticate at once")
	// the admin flag survived the rotation
	u, err := ms.UserByName(t.Context(), "alice")
	require.NoError(t, err)
	assert.True(t, u.Admin, "rotation must not touch the admin flag")

	// --- refusals ---
	resp = change("alice", "rotated-pass-1", "rotated-pass-1", "short")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "under-floor new password")
	resp = change("alice", "rotated-pass-1", "wrong-current", "rotated-pass-2")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "wrong current password")
	resp = change("alice", "rotated-pass-1", "rotated-pass-1", "rotated-pass-1")
	require.Equal(t, http.StatusOK, resp.StatusCode, "same password is allowed (not worth a rule)")

	// --- config-managed accounts answer with the reason ---
	resp = change("bob", "cfg-pass", "cfg-pass", "rotated-pass-3")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var env errEnvelope
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	assert.Contains(t, env.Error.Message, "not managed here")

	// --- anonymous bounces at the authn gate ---
	bare, err := http.Post(hs.URL+"/api/v1/auth/password", "application/json", nil)
	require.NoError(t, err)
	bare.Body.Close()
	require.Equal(t, http.StatusUnauthorized, bare.StatusCode)
}
