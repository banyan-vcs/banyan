// Package integration codifies six rounds of manual testing (MVP → unified DAG
// → negotiation → cross-view stress → multi-user collaboration) into automated
// regression integration tests (docs/operations/testing.md), split by test goal:
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
package support

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"archive/tar"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// --- Test infrastructure ---

type Server struct {
	*httptest.Server
}

func NewServer(t *testing.T) *Server {
	t.Helper()
	s, _, _ := NewServerPlane(t)
	return s
}

// NewServerPlane is NewServer with the underlying object plane exposed —
// auxiliary frontends (the gitaly gRPC server) serve the same plane.
func NewServerPlane(t *testing.T) (*Server, meta.MetaStore, *storage.ObjectService) {
	t.Helper()
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(TempDirQuiet(t))
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(ms, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gitwire.NewServer(ms, objects, model.SHA1).Register(mux)
	rpc.NewServer(ms, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Server{Server: srv}, ms, objects
}

func (s *Server) RepoURL(name string) string { return s.URL + "/r/" + name + ".git" }
func (s *Server) TreeURL(name, sub string) string {
	return s.URL + "/r/" + name + ".git/tree/" + sub
}

// tempDirQuiet is t.TempDir plus a retrying sweep: async server-side
// goroutines (post-receive hook delivery is fire-and-forget) can outlive
// the httptest server by microseconds and drop one more file into the
// shard root while the standard RemoveAll is already walking it — on
// Linux that surfaces as "directory not empty" and fails the TEST, not
// the code under test. The extra pass (registered later, so it runs
// first in LIFO order) sweeps until the tree stays gone.
func TempDirQuiet(t *testing.T) string {
	t.Helper()
	// Self-managed, NOT t.TempDir: async server-side writers (post-receive
	// hook delivery is fire-and-forget; index renders persist synthetics
	// after a fetch) can outlive the httptest server by a beat and drop one
	// more file into the shard root — on Linux a stdlib t.TempDir cleanup
	// then FAILS THE TEST with "directory not empty". This variant owns its
	// cleanup entirely: sweep, let a straggler land, confirm gone — repeat
	// until it stays gone; no stdlib cleanup is involved.
	dir, err := os.MkdirTemp("", "banyan-it-*")
	require.NoError(t, err)
	t.Cleanup(func() {
		for i := 0; i < 25; i++ {
			if err := os.RemoveAll(dir); err != nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			time.Sleep(15 * time.Millisecond) // let a straggler land
			if _, serr := os.Stat(dir); os.IsNotExist(serr) {
				return
			}
		}
		_ = os.RemoveAll(dir) // best effort; give up quietly
	})
	return dir
}

func NeedGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI not available")
	}
}

// git runs a real git command; failure is Fatal (output included in the error).
// Uniformly injects core.autocrlf=false (kills Windows CRLF noise) and a fixed identity.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := GitTry(dir, args...)
	require.NoError(t, err, "git %v (dir=%s) 失败\n%s", args, dir, out)
	return out
}

// gitTry tolerates failure (for asserting rejection behavior); returns combined output.
// The empty credential.helper resets any host helper on every call: embedded-credential
// URLs succeed without one, and a dev machine's Git Credential Manager would otherwise
// run on the approve/reject path — its store failures (Windows 0x8) print fatal lines
// that pollute output parsed by ref-count assertions (auth_test.go's ls-remote).
func GitTry(dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "credential.helper=",
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

func Write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func AppendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString(line)
	require.NoError(t, err)
}

func ReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// seedRepo builds a tiny repo and pushes it to the server (triggers auto-creation):
// README.md + lib/a.txt + lib/nested/SHARED.txt, single commit.
func SeedRepo(t *testing.T, s *Server, name string) string {
	t.Helper()
	dir := t.TempDir()
	Git(t, dir, "init", "-q", "-b", "main", ".")
	Write(t, filepath.Join(dir, "README.md"), "readme-v1\n")
	Write(t, filepath.Join(dir, "lib", "a.txt"), "a-v1\n")
	Write(t, filepath.Join(dir, "lib", "nested", "SHARED.txt"), "base\n")
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-qm", "seed: initial "+name)
	Git(t, dir, "push", "-q", s.RepoURL(name), "main")
	return dir
}

func CloneRepo(t *testing.T, s *Server, rawurl string) string {
	t.Helper()
	dst := t.TempDir()
	Git(t, "", "clone", "-q", rawurl, dst)
	return dst
}

func HeadOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(Git(t, dir, "rev-parse", "HEAD"))
}

func CountCommits(t *testing.T, dir string) int {
	t.Helper()
	out := strings.TrimSpace(Git(t, dir, "rev-list", "--count", "HEAD"))
	var n int
	fmt.Sscanf(out, "%d", &n)
	return n
}

func HasCommit(t *testing.T, dir, hash string) bool {
	t.Helper()
	out := Git(t, dir, "log", "--format=%H")
	return strings.Contains(out, hash)
}

func BlobOf(t *testing.T, dir, relPath string) string {
	t.Helper()
	return strings.TrimSpace(Git(t, dir, "rev-parse", "HEAD:"+relPath))
}

func FsckClean(t *testing.T, dir string) {
	t.Helper()
	out := Git(t, dir, "fsck", "--no-dangling")
	require.Empty(t, strings.TrimSpace(out), "fsck 不干净")
}

// countInPack returns the repo's local object count (CI efficiency metric; on small
// repos git splits packs into loose objects by default — sum of count + in-pack).
func CountInPack(t *testing.T, dir string) int {
	t.Helper()
	out := Git(t, dir, "count-objects", "-v")
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

// untar extracts an in-memory TAR into dir (snapshot round trips).
func Untar(t *testing.T, data []byte, dir string) error {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, body, 0o644); err != nil {
				return err
			}
		}
	}
}

// gitDated commits with pinned author/committer timestamps so archive mtimes
// are deterministic on both sides of the oracle.
func GitDated(t *testing.T, dir string, unix int64, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "user.name=it", "-c", "user.email=it@banyan.dev",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	stamp := strconv.FormatInt(unix, 10) + " +0000"
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_DATE="+stamp)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	require.NoError(t, cmd.Run(), "git %v: %s", args, buf.String())
	return buf.String()
}

// gitV2 runs a real git command under protocol.version=2 with packet
// tracing; returns combined output (stderr included — v2 negotiation lines
// land there).
func GitV2(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "user.name=it", "-c", "user.email=it@banyan.dev",
		"-c", "protocol.version=2",
	}, args...)
	cmd := exec.Command("git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TRACE_PACKET=1")
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	require.NoError(t, err, "git %v (dir=%s) 失败\n%s", args, dir, buf.String())
	return buf.String()
}

// newStack builds an in-process server with direct handles (T17 pattern) so
// the test can call storage-level repack and restart the process state.
func NewStack(t *testing.T, shardRoot, dbPath string, sqlite bool) (*Server, *storage.ObjectService) {
	t.Helper()
	var ms meta.MetaStore
	if sqlite {
		m, err := meta.NewSQLiteMetaStore(dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		ms = m
	} else {
		ms = meta.NewMemoryMetaStore()
	}
	local := storage.NewLocalShardStore(shardRoot)
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(ms, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gitwire.NewServer(ms, objects, model.SHA1).Register(mux)
	rpc.NewServer(ms, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Server{Server: srv}, objects
}
