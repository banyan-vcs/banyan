package rpc

// The CLI session mint (docs/gateway/authorization.md §6a): an
// authenticated identity exchanges itself for a SHORT-LIVED write-scope
// session; anonymous requests 401; the capability broadcast carries the
// SSH/web login addresses and stays anonymous (login itself needs it).
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// static table (cmd/banyand wires the same composition). boss is the
	// deployment admin — the scope-ceiling tests need one.
	s.Auth = &auth.TokenPolicy{Inner: &auth.StaticPolicy{
		Users:  map[string]string{"alice": "pw", "boss": "pw"},
		Admins: []string{"boss"},
	}, Tokens: ms}
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

	// A labeled mint names the device — revocable per machine, and the
	// label is sanitized server-side (an hostile label cannot smuggle
	// punctuation into token listings).
	reqL, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/auth/cli-session",
		strings.NewReader(`{"label":"laptop42; DROP"}`))
	reqL.SetBasicAuth("alice", "pw")
	respL, err := http.DefaultClient.Do(reqL)
	require.NoError(t, err)
	var labeled struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.NewDecoder(respL.Body).Decode(&labeled))
	respL.Body.Close()
	tokL, err := ms.TokenByHash(context.Background(), meta.HashToken(labeled.Token))
	require.NoError(t, err)
	require.Equal(t, "cli-session:laptop42DROP", tokL.Name)
}

// mintAs mints a PAT for user with the given scope and returns its plaintext.
func mintAs(t *testing.T, hs *httptest.Server, user, pass, scope string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":"t","scope":%q}`, scope)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/auth/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "mint %s %s", user, scope)
	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Token
}

// statusAs answers the status of one authenticated call — the escalation probe.
func statusAs(t *testing.T, hs *httptest.Server, method, path, user, pass, body string) int {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, hs.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

// TestMintEndpointsScopeCeiling: minting is a write-rank act and the minted
// scope never exceeds the caller's own — a read PAT must not be able to
// upgrade itself into a write session, a write PAT or an admin PAT (the
// read→write→admin chain was open before: both mint endpoints checked only
// anonymity, docs/gateway/authorization.md §8a).
func TestMintEndpointsScopeCeiling(t *testing.T) {
	_, _, hs := newAuthStack(t)

	readPAT := mintAs(t, hs, "alice", "pw", "read")
	writePAT := mintAs(t, hs, "alice", "pw", "write")

	// A read PAT cannot mint itself anything, on either endpoint.
	require.Equal(t, http.StatusForbidden, statusAs(t, hs, http.MethodPost, "/api/v1/auth/cli-session", "alice", readPAT, ""),
		"read PAT must not mint a write session")
	require.Equal(t, http.StatusForbidden, statusAs(t, hs, http.MethodPost, "/api/v1/auth/tokens", "alice", readPAT, `{"scope":"write"}`),
		"read PAT must not mint a write PAT")
	require.Equal(t, http.StatusForbidden, statusAs(t, hs, http.MethodPost, "/api/v1/auth/tokens", "alice", readPAT, `{"scope":"admin"}`),
		"read PAT must not mint an admin PAT")

	// A write PAT mints sessions fine (its own rank), but not admin.
	require.Equal(t, http.StatusOK, statusAs(t, hs, http.MethodPost, "/api/v1/auth/cli-session", "alice", writePAT, ""))
	require.Equal(t, http.StatusForbidden, statusAs(t, hs, http.MethodPost, "/api/v1/auth/tokens", "alice", writePAT, `{"scope":"admin"}`),
		"write rank must not mint admin")

	// A plain non-admin user cannot mint admin; the admin can.
	require.Equal(t, http.StatusForbidden, statusAs(t, hs, http.MethodPost, "/api/v1/auth/tokens", "alice", "pw", `{"scope":"admin"}`))
	require.Equal(t, http.StatusOK, statusAs(t, hs, http.MethodPost, "/api/v1/auth/tokens", "boss", "pw", `{"scope":"admin"}`))
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
