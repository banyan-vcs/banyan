package rpc

// The user SSH-key API face (docs/gateway/authorization.md §7a): add (one
// authorized_keys line), list your own, remove your own — and the
// assembled chain answers IdentifyKey with the stored key (the KeyPolicy
// wraps the same stack banyand wires: keys outermost over tokens over the
// static table).
import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestSSHKeyEndpoints(t *testing.T) {
	ms := meta.NewMemoryMetaStore()
	objects := storage.NewObjectService(ms, storage.NewLocalShardStore(t.TempDir()), []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	s := NewServer(ms, objects, model.SHA1)
	// The production composition: KeyPolicy outermost, tokens under it, the
	// static table innermost (cmd/banyand's static branch).
	s.Auth = &auth.KeyPolicy{Inner: &auth.TokenPolicy{
		Inner:  &auth.StaticPolicy{Users: map[string]string{"alice": "pw", "boss": "pw"}, Admins: []string{"boss"}},
		Tokens: ms,
	}, Keys: ms}
	mux := http.NewServeMux()
	s.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)

	// A real key, spelled as the authorized_keys line the console pastes.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pub, err := ssh.NewPublicKey(priv.Public().(ed25519.PublicKey))
	require.NoError(t, err)
	line := pub.Type() + " " + base64.StdEncoding.EncodeToString(pub.Marshal()) + " alice@laptop"
	wantFP := ssh.FingerprintSHA256(pub)

	// --- anonymous bounces at the authn gate ---
	resp, err := http.Post(hs.URL+"/api/v1/auth/ssh-keys", "application/json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// --- add: the parsed line answers its fingerprint ---
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/ssh-keys", "alice", map[string]string{"key": line})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var added struct {
		Fingerprint string `json:"fingerprint"`
		Type        string `json:"type"`
		Comment     string `json:"comment"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&added))
	assert.Equal(t, wantFP, added.Fingerprint)
	assert.Equal(t, "ssh-ed25519", added.Type)
	assert.Equal(t, "alice@laptop", added.Comment)

	// --- one key, one account: the same line bounces for anyone ---
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/ssh-keys", "boss", map[string]string{"key": line})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// --- client input problems: garbage, and a multi-line paste ---
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/ssh-keys", "alice", map[string]string{"key": "not a key"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/ssh-keys", "alice", map[string]string{"key": line + "\n" + line})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a multi-line paste must bounce (exactly one line)")

	// --- list: own keys, newest first, no blob on the wire ---
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/auth/ssh-keys", "alice", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var listed struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	require.Len(t, listed.Keys, 1)
	assert.Equal(t, wantFP, listed.Keys[0]["fingerprint"])
	assert.NotContains(t, listed.Keys[0], "blob", "the key material never echoes")
	resp = apiAs(t, hs.URL, http.MethodGet, "/api/v1/auth/ssh-keys", "boss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	assert.Empty(t, listed.Keys)

	// --- delete: the fingerprint rides a query parameter ---
	resp = apiAs(t, hs.URL, http.MethodDelete, "/api/v1/auth/ssh-keys", "alice", nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "DELETE without a fingerprint is client input")
	fpQ := "?fingerprint=" + url.QueryEscape(wantFP)
	resp = apiAs(t, hs.URL, http.MethodDelete, "/api/v1/auth/ssh-keys"+fpQ, "boss", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "owner-scoped: boss cannot remove alice's key")
	resp = apiAs(t, hs.URL, http.MethodDelete, "/api/v1/auth/ssh-keys"+fpQ, "alice", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// --- the assembled chain: the stored key identifies through the policy,
	// whatever username the connection claims; the deleted one does not ---
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/auth/ssh-keys", "alice", map[string]string{"key": line})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	if id := s.Auth.IdentifyKey("git", pub.Marshal()); id.User != "alice" {
		t.Errorf("stored key must identify as alice (claimed user advisory): got %q", id.User)
	}
	resp = apiAs(t, hs.URL, http.MethodDelete, "/api/v1/auth/ssh-keys"+fpQ, "alice", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	if id := s.Auth.IdentifyKey("alice", pub.Marshal()); !id.Anonymous() {
		t.Errorf("removed key must not identify: got %q", id.User)
	}
}
