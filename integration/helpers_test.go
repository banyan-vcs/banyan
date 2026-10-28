// Package integration codifies six rounds of manual testing (MVP → unified DAG
// → negotiation → cross-view stress → multi-user collaboration) into automated
// regression integration tests (docs/13-testing.md), split by test goal:
//
//	transport_basic_test.go       root/subtree clone, refs, incremental sync
//	subtree_collaboration_test.go weave semantics, non-interference, multi-user
//	push_guards_test.go           server-side staleness guards, negotiation
//	protocol_compat_test.go       git protocol matrix (versions/partial/shallow)
//	persistence_test.go           SQLite restart persistence
//	rpc_log_test.go               /api/v1/log path filtering
//
// Shape: httptest runs the full server in-process (MemoryMetaStore +
// LocalShardStore + gitwire + rpc, same wiring as cmd/banyan) with the real git
// CLI as client — exactly the manual verification path (protocol-level bugs only
// surface with real clients). All repos are tiny (ms-level push/clone); the go-git
// 4043-commit reload run remains a manual scenario (README quick start).
package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// --- Test infrastructure ---

type server struct {
	*httptest.Server
}

func newServer(t *testing.T) *server {
	t.Helper()
	s, _, _ := newServerPlane(t)
	return s
}

// newServerPlane is newServer with the underlying object plane exposed —
// auxiliary frontends (the gitaly gRPC server) serve the same plane.
func newServerPlane(t *testing.T) (*server, storage.MetaStore, *storage.ObjectService) {
	t.Helper()
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &server{Server: srv}, meta, objects
}

func (s *server) repoURL(name string) string { return s.URL + "/r/" + name + ".git" }
func (s *server) treeURL(name, sub string) string {
	return s.URL + "/r/" + name + ".git/tree/" + sub
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI not available")
	}
}

// git runs a real git command; failure is Fatal (output included in the error).
// Uniformly injects core.autocrlf=false (kills Windows CRLF noise) and a fixed identity.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitTry(dir, args...)
	require.NoError(t, err, "git %v (dir=%s) 失败\n%s", args, dir, out)
	return out
}

// gitTry tolerates failure (for asserting rejection behavior); returns combined output.
func gitTry(dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "user.name=it", "-c", "user.email=it@banyan.dev",
	}, args...)
	cmd := exec.Command("git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString(line)
	require.NoError(t, err)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// seedRepo builds a tiny repo and pushes it to the server (triggers auto-creation):
// README.md + lib/a.txt + lib/nested/SHARED.txt, single commit.
func seedRepo(t *testing.T, s *server, name string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(dir, "README.md"), "readme-v1\n")
	write(t, filepath.Join(dir, "lib", "a.txt"), "a-v1\n")
	write(t, filepath.Join(dir, "lib", "nested", "SHARED.txt"), "base\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "seed: initial "+name)
	git(t, dir, "push", "-q", s.repoURL(name), "main")
	return dir
}

func cloneRepo(t *testing.T, s *server, rawurl string) string {
	t.Helper()
	dst := t.TempDir()
	git(t, "", "clone", "-q", rawurl, dst)
	return dst
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

func countCommits(t *testing.T, dir string) int {
	t.Helper()
	out := strings.TrimSpace(git(t, dir, "rev-list", "--count", "HEAD"))
	var n int
	fmt.Sscanf(out, "%d", &n)
	return n
}

func hasCommit(t *testing.T, dir, hash string) bool {
	t.Helper()
	out := git(t, dir, "log", "--format=%H")
	return strings.Contains(out, hash)
}

func blobOf(t *testing.T, dir, relPath string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD:"+relPath))
}

func fsckClean(t *testing.T, dir string) {
	t.Helper()
	out := git(t, dir, "fsck", "--no-dangling")
	require.Empty(t, strings.TrimSpace(out), "fsck 不干净")
}

// countInPack returns the repo's local object count (CI efficiency metric; on small
// repos git splits packs into loose objects by default — sum of count + in-pack).
func countInPack(t *testing.T, dir string) int {
	t.Helper()
	out := git(t, dir, "count-objects", "-v")
	total := 0
	for _, line := range strings.Split(out, "\n") {
		for _, key := range []string{"count:", "in-pack:"} {
			if strings.HasPrefix(line, key) {
				n := 0
				fmt.Sscanf(strings.TrimSpace(line), key+" %d", &n)
				total += n
			}
		}
	}
	require.NotZero(t, total, "count-objects 解析失败:\n%s", out)
	return total
}
