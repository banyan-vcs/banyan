package rpc

// The CLI session mint (docs/gateway/authorization.md §6a): an
// authenticated identity exchanges itself for a SHORT-LIVED write-scope
// session; anonymous requests 401; the capability broadcast carries the
// SSH/web login addresses and stays anonymous (login itself needs it).
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func newAuthStack(t *testing.T) (*Server, *meta.MemoryMetaStore, *httptest.Server) {
	t.Helper()
	ms := meta.NewMemoryMetaStore()
	objects := storage.NewObjectService(ms, storage.NewLocalShardStore(t.TempDir()), []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	s := NewServer(ms, objects, model.SHA1)
	// Production shape: tokens ride the TokenPolicy decorator over the
	// static table (cmd/banyand wires the same composition).
	s.Auth = &auth.TokenPolicy{Inner: &auth.StaticPolicy{Users: map[string]string{"alice": "pw"}}, Tokens: ms}
	mux := http.NewServeMux()
	s.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return s, ms, hs
}

func TestCLISessionMint(t *testing.T) {
	_, ms, hs := newAuthStack(t)

	// Anonymous: 401.
	resp, err := http.Post(hs.URL+"/api/v1/auth/cli-session", "application/json", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// Authenticated (the web console relays the logged-in user's creds):
	// a short-lived write-scope session comes back once.
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/auth/cli-session", nil)
	req.SetBasicAuth("alice", "pw")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Token   string `json:"token"`
		User    string `json:"user"`
		Expires int64  `json:"expires"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "alice", out.User)
	require.Greater(t, out.Expires, time.Now().Add(11*time.Hour).Unix(), "a ~12h session, not a permanent PAT")
	tok, err := ms.TokenByHash(context.Background(), meta.HashToken(out.Token))
	require.NoError(t, err)
	require.Equal(t, "cli-session", tok.Name)
	require.Equal(t, meta.ScopeWrite, tok.Scope, "sessions never administer")

	// The minted session authenticates the API face.
	req2, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/v1/repos", nil)
	req2.SetBasicAuth("alice", out.Token)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode, "the session rides Basic owner:token")
}

func TestProtoBroadcastLoginAddresses(t *testing.T) {
	s, _, hs := newAuthStack(t)
	s.SSHAddr, s.WebURL = "ssh.example:22", "https://web.example"

	resp, err := http.Get(hs.URL + "/api/v1/proto") // NO credentials — by design
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var caps map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&caps))
	require.Equal(t, "ssh.example:22", caps["ssh"])
	require.Equal(t, "https://web.example", caps["web"])
}
