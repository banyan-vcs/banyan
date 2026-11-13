// T85 — the post-receive → GitLab internal API adapter (docs/integration/gitlab-integration.md §2): a
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
package gitlab

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

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestT85GitLabPostReceiveAdapter(t *testing.T) {
	support.NeedGit(t)
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
		case "/api/v4/internal/allowed":
			// The policy gate (T86) rides the same chain; T85 arms the
			// pass-through verdict so the push reaches the counters.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":true}`))
			return
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
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(support.TempDirQuiet(t))
	objects := storage.NewObjectService(ms, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.SetGitLab(&gitwire.GitLabConfig{URL: fake.URL, Secret: secret})
	mux := http.NewServeMux()
	gw.Register(mux)
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	s2 := &support.Server{Server: front}

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "t85 seed\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")

	// ① Anonymous-identity push (no platform headers): adapter silent.
	support.Git(t, src, "push", "-q", s2.RepoURL("t85"), "main")
	mu.Lock()
	assert.Nil(t, preBody, "plain push carries no identity: adapter stays off")
	mu.Unlock()

	// ② Platform-mediated push — http.extraHeader stands in for the
	// gitaly bridge's identity threading.
	support.AppendLine(t, filepath.Join(src, "README.md"), "second\n")
	support.Git(t, src, "commit", "-qam", "t85 second")
	newHead := support.HeadOf(t, src)
	support.Git(t, src, "-c", "http.extraHeader=X-Banyan-Gitlab-Id: user-7",
		"-c", "http.extraHeader=X-Banyan-Gitlab-Repo: project-42",
		"-c", "http.extraHeader=X-Banyan-Gitlab-Username: t85-user",
		"-c", "http.extraHeader=X-Banyan-Gitlab-Protocol: http",
		"push", "-q", s2.RepoURL("t85"), "main")

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

// T86 — the /allowed policy gate: a platform-mediated push must pass
// GitLab's ref-level policy (GitAccess.check(action, changes) — protected
// branches first) before any ref moves. Contract replayed from the live
// source (api/internal/base.rb check_allowed + GitAccessActor.from_params +
// ChangesList): identifier carries the GlId shapes, changes ride as
// "old new ref" lines, denial is HTTP 401 + {status:false, message} on
// purpose — Rails picked 401 so the message reaches the git client. A
// declined push never reaches /pre_receive (upstream ordering: policy
// precedes the counter pair), and an unreachable/broken Rails fails the
// push closed, like the upstream hook.
func TestT86GitLabAllowedGate(t *testing.T) {
	support.NeedGit(t)
	const secret = "t86-shared-secret"

	var mu sync.Mutex
	var allowedBody map[string]any
	var allowedJWT string
	var preCalls, postCalls int
	respCode, respBody := http.StatusOK, `{"status":true}`
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v4/internal/allowed":
			allowedBody = body
			allowedJWT = r.Header.Get("Gitlab-Shell-Api-Request")
			w.WriteHeader(respCode)
			_, _ = w.Write([]byte(respBody))
		case "/api/v4/internal/pre_receive":
			preCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/api/v4/internal/post_receive":
			postCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected internal API call: %s", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(fake.Close)

	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(support.TempDirQuiet(t))
	objects := storage.NewObjectService(ms, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.SetGitLab(&gitwire.GitLabConfig{URL: fake.URL, Secret: secret})
	mux := http.NewServeMux()
	gw.Register(mux)
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	s2 := &support.Server{Server: front}

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "t86 seed\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")

	// ① No platform identity → no gate: the plain push never consults
	// /allowed (banyan's own face, authorization unchanged).
	support.Git(t, src, "push", "-q", s2.RepoURL("t86"), "main")
	mu.Lock()
	assert.Nil(t, allowedBody, "identity-less push bypasses the policy gate")
	seedHead := support.HeadOf(t, src)
	mu.Unlock()

	support.AppendLine(t, filepath.Join(src, "README.md"), "blocked\n")
	support.Git(t, src, "commit", "-qam", "t86 blocked change")
	blockedHead := support.HeadOf(t, src)

	glPushTo := func(refspec string) (string, error) {
		return support.GitTry(src,
			"-c", "http.extraHeader=X-Banyan-Gitlab-Id: user-9",
			"-c", "http.extraHeader=X-Banyan-Gitlab-Repo: project-43",
			"-c", "http.extraHeader=X-Banyan-Gitlab-Username: t86-user",
			"-c", "http.extraHeader=X-Banyan-Gitlab-Protocol: http",
			"push", s2.RepoURL("t86"), refspec)
	}
	glPush := func() (string, error) { return glPushTo("main") }

	// ② Denied: Rails answers 401 + the protected-branch message. The push
	// dies client-side with that message, the ref never moves, and neither
	// counter half fires.
	mu.Lock()
	respCode, respBody = http.StatusUnauthorized, `{"status":false,"message":"GitLab: You are not allowed to push code to protected branches on this project."}`
	mu.Unlock()
	out, err := glPush()
	require.Error(t, err, "the policy denial must fail the push")
	assert.Contains(t, out, "remote rejected")
	assert.Contains(t, out, "protected branches", "Rails' message reaches the client")
	remoteTip := support.Git(t, "", "ls-remote", s2.RepoURL("t86"), "refs/heads/main")
	assert.Contains(t, remoteTip, seedHead, "ref unchanged after denial")
	assert.NotContains(t, remoteTip, blockedHead)
	mu.Lock()
	require.NotNil(t, allowedBody)
	assert.Equal(t, "user-9", allowedBody["identifier"])
	assert.Equal(t, "project-43", allowedBody["gl_repository"])
	assert.Equal(t, "http", allowedBody["protocol"])
	assert.Equal(t, "git-receive-pack", allowedBody["action"])
	changes, _ := allowedBody["changes"].(string)
	assert.Contains(t, changes, seedHead+" "+blockedHead+" refs/heads/main\n", "ChangesList line form")
	assert.Zero(t, preCalls, "declined push never reaches the counter pair")
	assert.Zero(t, postCalls)
	// The gate rides the same signed transport as the counter halves.
	parts := strings.Split(allowedJWT, ".")
	require.Len(t, parts, 3)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, serr := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, serr)
	assert.Equal(t, mac.Sum(nil), sig, "/allowed call is JWT-signed")
	mu.Unlock()

	// ③ Broken Rails (a 500 error page is not a verdict): fail closed.
	mu.Lock()
	respCode, respBody = http.StatusInternalServerError, `<html>502</html>`
	mu.Unlock()
	_, err = glPush()
	require.Error(t, err, "no verdict, no push — upstream fails closed too")

	// ④ Allowed: the same platform push sails through and the full chain
	// fires (allowed → pre counter → post receive).
	mu.Lock()
	respCode, respBody = http.StatusOK, `{"status":true}`
	mu.Unlock()
	out, err = glPush()
	require.NoError(t, err, "allowed platform push must succeed: %s", out)
	mu.Lock()
	assert.GreaterOrEqual(t, preCalls, 1, "past the gate, the counter increments")
	mu.Unlock()
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return postCalls >= 1
	}, 10*time.Second, 100*time.Millisecond, "post_receive fires for the allowed push")

	// ⑤ Branch creation rides the same gate with the full all-zero oldrev:
	// ChangesList splits on whitespace, so an empty field would shift the
	// ref name into newrev and send Rails' LfsCheck off to resolve
	// "refs/heads/…" as a revision (live-caught against GitLab 19.5).
	out, err = glPushTo("HEAD:refs/heads/t86-create")
	require.NoError(t, err, "branch creation through the gate: %s", out)
	mu.Lock()
	assert.Equal(t, strings.Repeat("0", 40)+" "+blockedHead+" refs/heads/t86-create\n",
		allowedBody["changes"], "creation line carries the all-zero old OID")
	mu.Unlock()
}
