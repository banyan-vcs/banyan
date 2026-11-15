// midstream_test.go: the mid-stream abort discipline end to end. A pack
// stream that breaks mid-copy must kill the HTTP connection — git gets a
// hard, retryable transport error — never end as a well-formed short
// response (the laundering shape behind "N bytes of body are still
// expected", pitfall 79). The break is injected at the storage seam
// (ShardBackend.OpenPack) after the repo is packed, so the serve path is
// the production verbatim stream.
package gitwire

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// breakingBackend delegates everything and cuts OpenPack streams after
// breakAfter bytes — the deterministic shape of a storage read dying
// mid-pack. breakAfter <= 0 passes streams through untouched.
type breakingBackend struct {
	storage.ShardBackend
	breakAfter int64
}

func (b *breakingBackend) OpenPack(ctx context.Context, repo string, packOID model.OID, holders []string) (io.ReadCloser, int64, error) {
	rc, size, err := b.ShardBackend.OpenPack(ctx, repo, packOID, holders)
	if err != nil {
		return nil, 0, err
	}
	if b.breakAfter <= 0 {
		return rc, size, nil
	}
	return &breakingReader{rc: rc, remain: b.breakAfter}, size, nil
}

type breakingReader struct {
	rc     io.ReadCloser
	remain int64
}

func (r *breakingReader) Read(p []byte) (int, error) {
	if r.remain <= 0 {
		return 0, fmt.Errorf("injected mid-pack stream break")
	}
	if int64(len(p)) > r.remain {
		p = p[:r.remain]
	}
	n, err := r.rc.Read(p)
	r.remain -= int64(n)
	return n, err
}

func (r *breakingReader) Close() error { return r.rc.Close() }

func msGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := msGitTry(dir, args...)
	require.NoError(t, err, "git %v (dir=%s) failed\n%s", args, dir, out)
	return out
}

func msGitTry(dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "credential.helper=",
		"-c", "user.name=it", "-c", "user.email=it@banyan.dev",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// TestCloneMidStreamBreakAborts: real git, real HTTP, both protocol
// generations. The stream break must surface as a transport failure — the
// signature regression is git's "N bytes of body are still expected", which
// means the break reached the client dressed as a complete response.
func TestCloneMidStreamBreakAborts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI not available")
	}
	ctx := context.Background()
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	objects := storage.NewObjectService(ms, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)

	mux := http.NewServeMux()
	NewServer(ms, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// One push of incompressible data (a small pack would stream whole
	// below the 64 KiB break point). ParseVirtualRepo strips the .git
	// suffix, so the meta repo behind /r/brk.git is plain "brk".
	src := t.TempDir()
	msGit(t, src, "init", "-q", "-b", "main", ".")
	blob := make([]byte, 128<<10)
	_, _ = rand.Read(blob)
	require.NoError(t, os.WriteFile(filepath.Join(src, "blob.bin"), blob, 0o644))
	msGit(t, src, "add", ".")
	msGit(t, src, "commit", "-q", "-m", "big")
	msGit(t, src, "remote", "add", "origin", srv.URL+"/r/brk.git")
	msGit(t, src, "push", "-q", "origin", "main")

	// One pack, so the clone serves the verbatim stream (not an accident
	// of how the push fragmented).
	_, err := objects.Consolidate(ctx, "brk")
	require.NoError(t, err)

	// Fixture sanity: an unbroken clone works.
	ok := t.TempDir()
	msGit(t, ok, "clone", "-q", srv.URL+"/r/brk.git", "repo")

	// Inject the break: every pack stream now dies 64 KiB in.
	objects.Backend = &breakingBackend{ShardBackend: objects.Backend, breakAfter: 64 << 10}

	for _, version := range []string{"2", "0"} {
		dst := t.TempDir()
		out, err := msGitTry(dst, "-c", "protocol.version="+version,
			"clone", srv.URL+"/r/brk.git", "repo")
		require.Error(t, err, "protocol v%s: the clone must fail hard\n%s", version, out)
		require.NotContains(t, out, "body are still expected",
			"protocol v%s: a broken stream reached git dressed as a complete response\n%s", version, out)
	}
}

// TestServeClosurePackEncodeError: a failed encode reports the encode-error
// rung, stores nothing in the response cache, and on the HTTP sink aborts
// the connection — a clean ending would hand the client an empty-or-partial
// packfile section inside a well-formed response, and the cacheable variant
// used to store the partial bytes for later clones.
func TestServeClosurePackEncodeError(t *testing.T) {
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	objects := storage.NewObjectService(ms, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	s := &Server{Meta: ms, Objects: objects, Algo: model.SHA1}

	// A tree whose bytes do not hash to its OID — putObject rejects it at
	// encode time (a deterministic encode failure, no wire bytes yet).
	vr := VirtualRepo{Repo: "enc.git"}
	trees := map[model.OID][]byte{
		model.OID(strings.Repeat("0", 40)): []byte("100644 x\x00not this tree's bytes"),
	}

	// SSH-shaped sink (plain bufWriter): the rung reports encode-error.
	rung := s.serveClosurePack(context.Background(), newBufWriter(io.Discard), vr, nil,
		map[model.OID][]byte{}, trees, map[model.OID][]byte{}, map[model.OID][]byte{}, true, true, nil)
	require.Equal(t, "encode-error", rung)
	_, cached := objects.CachedResponsePack(vr.Repo, vr.Sub, nil, true)
	require.False(t, cached, "a failed encode must not enter the response cache")

	// HTTP sink: the same failure aborts the connection.
	func() {
		defer func() {
			require.Equal(t, http.ErrAbortHandler, recover(), "the HTTP sink must abort, not end clean-short")
		}()
		s.serveClosurePack(context.Background(), newAbortBufWriter(httptest.NewRecorder()), vr, nil,
			map[model.OID][]byte{}, trees, map[model.OID][]byte{}, map[model.OID][]byte{}, true, true, nil)
	}()
}
