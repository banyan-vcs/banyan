package cli

// host ls/use, the login --status report, and the logout/401 UX: the
// multi-host flows on top of the config store. Network probes and
// revocations run against stub servers; the interactive menus are
// exercised through pickFromList directly (the os.Stdin paths only
// degrade to their non-interactive hints in tests).
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func TestLogoutRevokesServerSide(t *testing.T) {
	isolateConfig(t)

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	putCred(srv.URL, credential{User: "alice", Token: "banyan_live", Method: "web", Expires: time.Now().Add(time.Hour).Unix()})
	var out, errb strings.Builder
	require.Equal(t, 0, runLogout("/nonexistent", []string{srv.URL}, &out, &errb))
	assert.Contains(t, out.String(), "revoked the server-side session")
	assert.Equal(t, "/api/v1/auth/tokens/"+meta.HashToken("banyan_live"), gotPath,
		"logout self-revokes via the token's sha256 (own tokens only)")
	assert.Contains(t, gotAuth, "Basic ")
	assert.Nil(t, credForHost(srv.URL), "the local credential is gone too")

	// An unreachable server must not block local logout (best-effort).
	putCred("http://127.0.0.1:1", credential{User: "bob", Token: "banyan_dead", Method: "web", Expires: time.Now().Add(time.Hour).Unix()})
	out.Reset()
	assert.Equal(t, 0, runLogout("/nonexistent", []string{"http://127.0.0.1:1"}, &out, &errb))
	assert.NotContains(t, out.String(), "revoked")
	assert.Nil(t, credForHost("http://127.0.0.1:1"))
}

func TestExpiredWebSessionHint(t *testing.T) {
	isolateConfig(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"login first"}}`))
	}))
	t.Cleanup(srv.Close)

	putCred(srv.URL, credential{User: "alice", Token: "banyan_stale", Method: "web", Expires: time.Now().Add(-time.Minute).Unix()})
	c := newClient(srv.URL)
	err := c.post("/api/v1/repos", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "re-login: banyan login "+srv.URL,
		"a web-path 401 (no silent refresh) points at the way out")
}

func TestPickFromList(t *testing.T) {
	var out strings.Builder
	i, err := pickFromList(&out, strings.NewReader("2\n"), "pick:", []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Equal(t, 1, i)
	assert.Contains(t, out.String(), "2) b")

	out.Reset()
	i, err = pickFromList(&out, strings.NewReader("x\n3\n"), "pick:", []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Equal(t, 2, i, "an invalid line retries, then accepts")
	assert.Contains(t, out.String(), "enter a number 1-3")

	out.Reset()
	_, err = pickFromList(&out, strings.NewReader(""), "pick:", []string{"a"})
	assert.Error(t, err, "EOF without a selection errors")
}

func TestHostLsAndUse(t *testing.T) {
	isolateConfig(t)

	// Empty store: an informational zero, not an error.
	var out, errb strings.Builder
	assert.Equal(t, 0, runHost([]string{"ls"}, &out, &errb))
	assert.Contains(t, out.String(), "no configured servers")

	future := time.Now().Add(time.Hour).Unix()
	putCred("http://a.corp", credential{User: "alice", Token: "banyan_a", Method: "ssh", SSHAddr: "a:22", Expires: future})
	putCred("http://b.corp", credential{User: "bob", Token: "banyan_b", Method: "web", Expires: future})

	out.Reset()
	assert.Equal(t, 0, runHost([]string{"ls"}, &out, &errb))
	assert.Contains(t, out.String(), "* http://a.corp  alice via ssh") // the default is marked
	assert.Contains(t, out.String(), "  http://b.corp  bob via web")
	assert.Contains(t, out.String(), "switch with: banyan host use")

	out.Reset()
	assert.Equal(t, 0, runHost([]string{"ls", "--json"}, &out, &errb))
	var rows []struct {
		Server  string `json:"server"`
		Default bool   `json:"default"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &rows))
	require.Len(t, rows, 2)
	assert.Equal(t, "http://a.corp", rows[0].Server)
	assert.True(t, rows[0].Default)
	assert.False(t, rows[1].Default)

	// Explicit switch, unknown host, and the non-interactive hint.
	out.Reset()
	assert.Equal(t, 0, runHost([]string{"use", "http://b.corp/"}, &out, &errb))
	assert.Contains(t, out.String(), "default server: http://b.corp")
	assert.Equal(t, "http://b.corp", loadConfig().DefaultHost)

	out.Reset()
	errb.Reset()
	assert.Equal(t, 2, runHost([]string{"use", "http://unknown.io"}, &out, &errb))
	assert.Contains(t, errb.String(), "not a configured server")

	out.Reset()
	errb.Reset()
	assert.Equal(t, 2, runHost([]string{"use"}, &out, &errb))
	assert.Contains(t, errb.String(), "pass the server")
}

func TestLoginStatusAcrossHosts(t *testing.T) {
	isolateConfig(t)

	// Two islands: one accepts the stored token, one refuses everything.
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == basicHeaderValue("alice", "banyan_live") {
			_, _ = w.Write([]byte(`{"repos":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(live.Close)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(refusing.Close)

	future := time.Now().Add(time.Hour).Unix()
	putCred(live.URL, credential{User: "alice", Token: "banyan_live", Method: "ssh", Expires: future})
	putCred(refusing.URL, credential{User: "alice", Token: "banyan_refused", Method: "web", Expires: future})
	putCred("http://expired.io", credential{User: "dave", Token: "banyan_old", Method: "web", Expires: time.Now().Add(-time.Minute).Unix()})

	var out, errb strings.Builder
	assert.Equal(t, 0, loginStatus("", false, &out, &errb))
	assert.Contains(t, out.String(), live.URL+" as alice via ssh")
	assert.Contains(t, out.String(), "(valid)")   // the live session, network-verified
	assert.Contains(t, out.String(), "(rejected") // a stored but refused token
	assert.Contains(t, out.String(), "(EXPIRED")  // past the clock — no network needed
	assert.Contains(t, out.String(), "default:")  // the footer names the active island

	// The explicit form narrows to one host and exits 1 when absent.
	out.Reset()
	assert.Equal(t, 0, loginStatus(live.URL, true, &out, &errb))
	assert.Contains(t, out.String(), "as alice")
	out.Reset()
	assert.Equal(t, 1, loginStatus("http://never.io", true, &out, &errb))
	assert.Contains(t, out.String(), "not logged in")
}
