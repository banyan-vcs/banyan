package rpc

// The administrator reset and its must-change fence
// (docs/gateway/authorization.md §2a): the reset answers a fresh random
// password exactly once, the old password dies immediately, the account
// owes a self-chosen password (whoami/login carry the bit), the mint
// faces bounce until it is paid, and the user's own change clears the
// fence while the admin flag survives.
import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
)

func TestPasswordReset(t *testing.T) {
	// CI runner CPU: hashing at MinCost — see auth.BcryptCost.
	auth.BcryptCost = bcrypt.MinCost
	t.Cleanup(func() { auth.BcryptCost = bcrypt.DefaultCost })
	s, ms, hs := newAuthStack(t)
	// The account table rides the static policy's user source (the
	// production wiring); alice starts as a plain table account.
	static := &auth.StaticPolicy{Users: map[string]string{"boss": "pw"}, Admins: []string{"boss"}}
	static.UserSource = ms
	s.Auth = &auth.TokenPolicy{Inner: static, Tokens: ms}
	hash, err := auth.HashPassword("first-password")
	require.NoError(t, err)
	require.NoError(t, ms.PutUser(t.Context(), meta.User{Name: "alice", Password: hash, CreatedAt: 1}))

	// asJSON issues a JSON request with EXPLICIT basic credentials (apiAs
	// hardcodes the stack's "pw"; table accounts here carry their own).
	asJSON := func(method, path, user, pass string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, err := json.Marshal(body)
			require.NoError(t, err)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, hs.URL+path, rdr)
		require.NoError(t, err)
		req.SetBasicAuth(user, pass)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	reset := func(caller string) *http.Response {
		return apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/reset-password", caller, map[string]string{"user": "alice"})
	}
	loginJSON := func(user, pass string) (int, map[string]any) {
		body, err := json.Marshal(map[string]string{"user": user, "password": pass})
		require.NoError(t, err)
		resp, err := http.Post(hs.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// --- refusals first: non-admin, unknown account, anonymous ---
	resp := asJSON(http.MethodPost, "/api/v1/auth/reset-password", "alice", "first-password", map[string]string{"user": "alice"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "a plain account may not reset")
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/reset-password", "boss", map[string]string{"user": "nobody"})
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "unknown account")
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/reset-password", "boss", map[string]string{"user": "boss"})
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "config-file accounts are not managed here")
	bare, err := http.Post(hs.URL+"/api/v1/auth/reset-password", "application/json", nil)
	require.NoError(t, err)
	bare.Body.Close()
	require.Equal(t, http.StatusUnauthorized, bare.StatusCode)

	// --- the reset: a one-time random password, the admin flag intact ---
	resp = reset("boss")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "alice", out.User)
	require.Greater(t, len(out.Password), 8, "a real random secret, not a stub")
	stored, err := ms.UserByName(t.Context(), "alice")
	require.NoError(t, err)
	assert.True(t, auth.CheckPassword(stored.Password, out.Password), "the answered password must verify")
	assert.False(t, stored.Admin, "a plain account stays plain")
	assert.True(t, stored.MustChangePassword, "the fence is up")

	// --- the old password dies at once; the random one logs in with the bit ---
	code, _ := loginJSON("alice", "first-password")
	assert.Equal(t, http.StatusUnauthorized, code, "old password dies immediately")
	code, body := loginJSON("alice", out.Password)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, body["mustChange"], "login carries the fence")

	// --- whoami carries the bit; the mint faces bounce behind it ---
	resp = asJSON(http.MethodGet, "/api/v1/auth/whoami", "alice", out.Password, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var who map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&who))
	assert.Equal(t, "alice", who["user"])
	assert.Equal(t, true, who["mustChange"])
	resp = asJSON(http.MethodPost, "/api/v1/auth/tokens", "alice", out.Password, map[string]string{"name": "sneak", "scope": "write"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "no PAT behind the fence")
	resp = asJSON(http.MethodPost, "/api/v1/auth/ssh-keys", "alice", out.Password, map[string]string{"key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJSvXGZZ0eJkHTCq7UQPlt6ncDXKlKTCeanrWpXH3PXd nobody@example"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "no SSH key behind the fence")

	// --- the user's own change pays the fence and keeps the admin flag ---
	resp = asJSON(http.MethodPost, "/api/v1/auth/password", "alice", out.Password, map[string]string{"current": out.Password, "next": "chosen-pass-1"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	stored, err = ms.UserByName(t.Context(), "alice")
	require.NoError(t, err)
	assert.False(t, stored.MustChangePassword, "self-service clears the fence")
	code, body = loginJSON("alice", "chosen-pass-1")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, body["mustChange"])
	resp = asJSON(http.MethodPost, "/api/v1/auth/tokens", "alice", "chosen-pass-1", map[string]string{"name": "after", "scope": "write"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "minting works once the fence is paid")

	// --- resetting an ADMIN account keeps its rank ---
	require.NoError(t, ms.PutUser(t.Context(), meta.User{Name: "root2", Password: hash, Admin: true, CreatedAt: 2}))
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/reset-password", "boss", map[string]string{"user": "root2"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	stored, err = ms.UserByName(t.Context(), "root2")
	require.NoError(t, err)
	assert.True(t, stored.Admin, "the admin flag survives the reset")
	assert.True(t, stored.MustChangePassword)
}
