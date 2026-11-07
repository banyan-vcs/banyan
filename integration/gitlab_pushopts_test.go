// T87 — the last GitLab plumbing round: push options ride the wire
// end-to-end (capability advertisement → negotiated section after the
// command list → /post_receive payload key params[:push_options], the
// exact shape PostReceiveService feeds Gitlab::PushOptions), and the
// /allowed verdict's git_config_options carry receive.maxInputSize —
// GitLab's "Maximum push size" admin setting — which the gate enforces
// against the spooled pack (git's index-pack fails the whole push; so does
// the gate, one step later at the same outcome).
package integration

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestT87GitLabPushOptionsAndSizeLimit(t *testing.T) {
	needGit(t)
	const secret = "t87-shared-secret"

	var mu sync.Mutex
	var postBody map[string]any
	var verdictBody = `{"status":true}`
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v4/internal/allowed":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(verdictBody))
		case "/api/v4/internal/pre_receive":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/api/v4/internal/post_receive":
			postBody = body
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected internal API call: %s", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(fake.Close)

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
	write(t, filepath.Join(src, "README.md"), "t87 seed\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "seed")
	// Anonymous seed push (adapter stays silent without an identity).
	git(t, src, "push", "-q", s2.repoURL("t87"), "main")

	glPushTo := func(refspec string, opts ...string) (string, error) {
		args := []string{
			"-c", "http.extraHeader=X-Banyan-Gitlab-Id: user-11",
			"-c", "http.extraHeader=X-Banyan-Gitlab-Repo: project-87",
			"-c", "http.extraHeader=X-Banyan-Gitlab-Username: t87-user",
			"-c", "http.extraHeader=X-Banyan-Gitlab-Protocol: http",
			"push",
		}
		for _, o := range opts {
			args = append(args, "-o", o)
		}
		args = append(args, s2.repoURL("t87"), refspec)
		return gitTry(src, args...)
	}
	resetPost := func() {
		mu.Lock()
		defer mu.Unlock()
		postBody = nil
	}
	waitPost := func() map[string]any {
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return postBody != nil
		}, 10*time.Second, 100*time.Millisecond, "post_receive must fire")
		mu.Lock()
		defer mu.Unlock()
		return postBody
	}

	// ① Push options ride through: -o strings arrive verbatim in the
	// post_receive payload (PostReceiveService → Gitlab::PushOptions →
	// merge_request.create / ci.skip / …).
	appendLine(t, filepath.Join(src, "README.md"), "with options\n")
	git(t, src, "commit", "-qam", "t87 with options")
	resetPost()
	out, err := glPushTo("main", "merge_request.create", "ci.skip")
	require.NoError(t, err, "push with options: %s", out)
	body := waitPost()
	mu.Lock()
	assert.Equal(t, []any{"merge_request.create", "ci.skip"}, body["push_options"],
		"-o strings ride post_receive verbatim")
	mu.Unlock()

	// ② A plain push negotiates no push-options section: the key is absent.
	appendLine(t, filepath.Join(src, "README.md"), "no options\n")
	git(t, src, "commit", "-qam", "t87 no options")
	resetPost()
	out, err = glPushTo("main")
	require.NoError(t, err, "plain push: %s", out)
	body = waitPost()
	mu.Lock()
	assert.NotContains(t, body, "push_options", "no -o, no section, no key")
	mu.Unlock()

	// ③ receive.maxInputSize from the verdict caps the push: GitLab's
	// "Maximum push size" setting reaches the gate and a larger pack dies
	// with git's own error shape. The payload is random — packs are
	// deflate-compressed, a run of one byte would squeeze under any cap.
	mu.Lock()
	verdictBody = `{"status":true,"git_config_options":["uploadpack.allowFilter=true","receive.maxInputSize=512"]}`
	mu.Unlock()
	big := make([]byte, 32*1024)
	_, _ = rand.Read(big)
	write(t, filepath.Join(src, "big.bin"), string(big))
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t87 oversized")
	bigHead := headOf(t, src)
	out, err = glPushTo("main")
	require.Error(t, err, "oversized push must fail")
	assert.Contains(t, out, "pack exceeds maximum allowed size")
	remoteTip := git(t, "", "ls-remote", s2.repoURL("t87"), "refs/heads/main")
	assert.NotContains(t, remoteTip, bigHead, "ref unchanged after the size rejection")

	// ④ A generous cap (or none) lets the same pack through.
	mu.Lock()
	verdictBody = `{"status":true,"git_config_options":["receive.maxInputSize=1048576"]}`
	mu.Unlock()
	out, err = glPushTo("main")
	require.NoError(t, err, "within the cap: %s", out)
	remoteTip = git(t, "", "ls-remote", s2.repoURL("t87"), "refs/heads/main")
	assert.Contains(t, remoteTip, bigHead)
}
