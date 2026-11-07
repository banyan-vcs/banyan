// T85 — the post-receive → GitLab internal API adapter (docs/20 §2): a
// push that carries a platform identity (workhorse sets GlId/GlRepository
// on the gRPC request; the gitaly bridge threads them to the receive path
// as the X-Banyan-Gitlab-* headers below) must notify Rails' /pre_receive
// and /post_receive with the gitlab-shell contract — a JWT (HS256,
// iss=gitlab-shell, 60s TTL) signed with the shared secret in the
// Gitlab-Shell-Api-Request header, and a JSON body keyed the way the
// internal API reads it. The fake replays exactly the checks Rails
// performs (lib/gitlab/shell.rb verify_api_request, internal/base.rb
// post_receive → GitPostReceive#parse_changes), so a contract drift fails
// here, not in a live GitLab.
package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestT85GitLabPostReceiveAdapter(t *testing.T) {
	needGit(t)
	const secret = "t85-shared-secret"

	var mu sync.Mutex
	var preBody, postBody map[string]any
	var jwtHeaders []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		defer mu.Unlock()
		jwtHeaders = append(jwtHeaders, r.Header.Get("Gitlab-Shell-Api-Request"))
		switch r.URL.Path {
		case "/api/v4/internal/pre_receive":
			preBody = body
		case "/api/v4/internal/post_receive":
			postBody = body
		default:
			t.Errorf("unexpected internal API call: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(fake.Close)

	// A gitwire server with the adapter armed — the identity headers below
	// are exactly what the gitaly bridge injects for platform-mediated
	// pushes (smarthttp_service.go); a plain push without them must not
	// fire the adapter at all.
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(tempDirQuiet(t))
	objects := storage.NewObjectService(meta, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	gw.SetGitLab(&gitwire.GitLabConfig{URL: fake.URL, Secret: secret})
	mux := http.NewServeMux()
	gw.Register(mux)
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	s2 := &server{Server: front}

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "README.md"), "t85 seed\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "seed")

	// ① Anonymous-identity push (no platform headers): adapter silent.
	git(t, src, "push", "-q", s2.repoURL("t85"), "main")
	mu.Lock()
	assert.Nil(t, preBody, "plain push carries no identity: adapter stays off")
	mu.Unlock()

	// ② Platform-mediated push — http.extraHeader stands in for the
	// gitaly bridge's identity threading.
	appendLine(t, filepath.Join(src, "README.md"), "second\n")
	git(t, src, "commit", "-qam", "t85 second")
	newHead := headOf(t, src)
	git(t, src, "-c", "http.extraHeader=X-Banyan-Gitlab-Id: user-7",
		"-c", "http.extraHeader=X-Banyan-Gitlab-Repo: project-42",
		"-c", "http.extraHeader=X-Banyan-Gitlab-Username: t85-user",
		"-c", "http.extraHeader=X-Banyan-Gitlab-Protocol: http",
		"push", "-q", s2.repoURL("t85"), "main")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return postBody != nil
	}, 10*time.Second, 100*time.Millisecond, "post_receive must fire")

	mu.Lock()
	defer mu.Unlock()
	// pre pairs Rails' reference counter; post carries identity + the
	// applied ref in GitPostReceive's line format.
	assert.Equal(t, "project-42", preBody["gl_repository"])
	assert.Equal(t, "project-42", postBody["gl_repository"])
	assert.Equal(t, "user-7", postBody["identifier"])
	assert.Equal(t, float64(7), postBody["user_id"], "actor.user resolves from user_id")
	assert.Equal(t, "http", postBody["protocol"])
	changes, _ := postBody["changes"].(string)
	assert.Contains(t, changes, " refs/heads/main\n", "applied ref in old-new-ref line form")
	assert.Contains(t, changes, newHead, "the new tip")

	// The JWT: Rails' own verification is HS256 with the shared secret and
	// iss=gitlab-shell (shell.rb verify_api_request).
	require.NotEmpty(t, jwtHeaders)
	parts := strings.Split(jwtHeaders[len(jwtHeaders)-1], ".")
	require.Len(t, parts, 3)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	require.Equal(t, mac.Sum(nil), sig, "HS256 with the shared secret")
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	claims := map[string]any{}
	require.NoError(t, json.Unmarshal(claimsRaw, &claims))
	assert.Equal(t, "gitlab-shell", claims["iss"])
	assert.Contains(t, claims, "exp", "expiring token (60s TTL)")
}
