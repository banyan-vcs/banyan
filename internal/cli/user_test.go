package cli

// The administrator reset command (docs/gateway/authorization.md §2a):
// one POST with the caller's stored admin credential, the random
// password printed exactly once, and the right exit codes on refusals.
import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUserResetPassword(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/reset-password" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Header.Get("Authorization") != "":
			w.Write([]byte(`{"user":"alice","password":"rand0m-secret"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"authentication required"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BANYAN_CONFIG", t.TempDir()+"/conf.yaml")
	t.Setenv("BANYAN_SERVER", srv.URL)

	// --- usage refusals ---
	var out, errb strings.Builder
	require.Equal(t, 2, Run([]string{"user"}, &out, &errb), "bare user")
	require.Equal(t, 2, Run([]string{"user", "reset-password"}, &out, &errb), "missing name")
	require.Equal(t, 2, Run([]string{"user", "explode"}, &out, &errb), "unknown subcommand")

	// --- no stored credential: the auth gate answers, exit 2, no print ---
	out.Reset()
	errb.Reset()
	require.Equal(t, 2, Run([]string{"user", "reset-password", "alice"}, &out, &errb))
	require.NotContains(t, out.String(), "rand0m-secret")

	// --- with the admin session: one POST, the password printed exactly once ---
	putCred(srv.URL, credential{User: "boss", Token: "tok", Expires: time.Now().Add(time.Hour).Unix(), Method: "web"})
	out.Reset()
	errb.Reset()
	require.Equal(t, 0, Run([]string{"user", "reset-password", "alice"}, &out, &errb), errb.String())
	require.Contains(t, gotAuth, "Basic ", "the session credential rides")
	require.Contains(t, out.String(), "alice's new password (shown once): rand0m-secret")
	require.Contains(t, out.String(), "must choose its own password")

	// --- a trailing slash on BANYAN_SERVER finds the same credential ---
	t.Setenv("BANYAN_SERVER", srv.URL+"/")
	out.Reset()
	errb.Reset()
	require.Equal(t, 0, Run([]string{"user", "reset-password", "alice"}, &out, &errb), errb.String())
	require.Contains(t, out.String(), "rand0m-secret")
}
