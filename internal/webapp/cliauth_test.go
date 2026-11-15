package webapp

// The CLI approval relay (docs/gateway/authorization.md §6a, no-SSH path):
// a logged-in console session approves a handshake id, the gateway mints
// a short-lived session with the user's own credentials, and the CLI's
// poll claims it EXACTLY ONCE.
import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCLIApprovePollRelay(t *testing.T) {
	var mintAuth, mintBody string
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/auth/login":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		case r.URL.Path == "/api/v1/auth/cli-session" && r.Method == http.MethodPost:
			mintAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			mintBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token":"banyan_shortlived","user":"alice","expires":%d}`, time.Now().Add(12*time.Hour).Unix())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	hs, db := newTestStack(t, gw)

	// Local login → session cookie.
	resp := postJSON(t, hs.URL+"/api/login", map[string]string{"user": "alice", "password": "pw"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookie := resp.Header.Get("Set-Cookie")
	require.NotEmpty(t, cookie)

	// Approve a handshake as alice: the gateway mint MUST ride alice's
	// own credentials (Basic of the web session), and the CLI's device
	// label travels from the URL fragment through to the mint body.
	resp = postJSONCookie(t, hs.URL+"/api/cli/approve", cookie, map[string]string{"id": "handshake-0123456789abcdef", "label": "laptop42"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, strings.HasPrefix(mintAuth, "Basic "), "mint went out with the session's credentials")
	require.JSONEq(t, `{"label":"laptop42"}`, mintBody, "the device label names the minted session")

	// The CLI poll claims the token exactly once. The id rides the BODY —
	// the request line (query included) is what access logs capture.
	get := func() string {
		r, err := http.Post(hs.URL+"/api/cli/poll", "application/json",
			strings.NewReader(`{"id":"handshake-0123456789abcdef"}`))
		require.NoError(t, err)
		defer r.Body.Close()
		var out struct {
			Status  string `json:"status"`
			Token   string `json:"token"`
			User    string `json:"user"`
			Expires int64  `json:"expires"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&out))
		if out.Status == "granted" {
			require.Equal(t, "banyan_shortlived", out.Token)
			require.Equal(t, "alice", out.User)
			require.Greater(t, out.Expires, time.Now().Unix())
			return "granted"
		}
		return out.Status
	}
	require.Equal(t, "granted", get(), "an approved handshake answers the token")
	require.Equal(t, "pending", get(), "the grant is one-shot: the second poll sees nothing")

	// Approvals need a session; the poll needs a well-formed id (both the
	// POST body form and the legacy GET query form are validated).
	resp = postJSON(t, hs.URL+"/api/cli/approve", map[string]string{"id": "handshake-0123456789abcdef"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	r2, err := http.Post(hs.URL+"/api/cli/poll", "application/json", strings.NewReader(`{"id":"short"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, r2.StatusCode)
	r2.Body.Close()
	r3, err := http.Get(hs.URL + "/api/cli/poll?id=handshake-0123456789abcdef")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, r3.StatusCode, "legacy GET still answers (one CLI generation of compatibility)")
	r3.Body.Close()
	_ = db
}
