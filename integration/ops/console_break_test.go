// console_break_test.go (T50, docs/web/console.md §1): the incident-topology
// regression for the mid-stream abort discipline — git → console proxy →
// gateway → storage, exactly the chain behind pitfall 79's CI red ("363
// bytes of body are still expected"). A pack stream that breaks mid-copy
// must reach git as a hard transport error; anything clean-ended means some
// layer dressed the truncation as a complete response again.
package ops

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// breakBackend delegates everything and cuts OpenPack streams after
// breakAfter bytes — the deterministic shape of a storage read dying
// mid-pack behind the whole console chain.
type breakBackend struct {
	storage.ShardBackend
	breakAfter int64
}

func (b *breakBackend) OpenPack(ctx context.Context, repo string, packOID model.OID, holders []string) (io.ReadCloser, int64, error) {
	rc, size, err := b.ShardBackend.OpenPack(ctx, repo, packOID, holders)
	if err != nil {
		return nil, 0, err
	}
	if b.breakAfter <= 0 {
		return rc, size, nil
	}
	return &breakReader{rc: rc, remain: b.breakAfter}, size, nil
}

type breakReader struct {
	rc     io.ReadCloser
	remain int64
}

func (r *breakReader) Read(p []byte) (int, error) {
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

func (r *breakReader) Close() error { return r.rc.Close() }

// TestConsoleCloneMidStreamBreakAborts mirrors TestConsoleMemoryChain's
// fixture (group/project, push, repack through the console passthrough),
// then breaks the pack stream at the storage seam and clones through the
// console plane. The clone must fail as a transport error — the assertion
// is deliberately the NEGATIVE of git's laundering signature.
func TestConsoleCloneMidStreamBreakAborts(t *testing.T) {
	support.NeedGit(t)
	s := newConsoleStack(t)
	root := s.addUser(t, "root", true)
	s.addUser(t, "alice", false)
	code, out := s.call(t, "POST", "/api/v1/groups", root, map[string]string{"name": "brk"})
	require.Equal(t, 201, code, "%v", out)
	code, out = s.call(t, "POST", "/api/v1/repos/brk", root, map[string]string{"name": "repo"})
	require.Equal(t, 201, code, "%v", out)
	code, _ = s.call(t, "PUT", "/api/v1/repos/brk/repo/members/alice", root, map[string]string{"role": "developer"})
	require.Equal(t, 200, code)
	code, _ = s.call(t, "PATCH", "/api/v1/repos/brk/repo", root, map[string][]string{"protected_branches": {}})
	require.Equal(t, 200, code)

	src := t.TempDir()
	support.Git(t, src, "init", "-b", "main", ".")
	// Incompressible and comfortably past the 64 KiB break point.
	blob := make([]byte, 128<<10)
	_, _ = rand.Read(blob)
	require.NoError(t, os.WriteFile(filepath.Join(src, "blob.bin"), blob, 0o644))
	support.Git(t, src, "add", ".")
	support.Git(t, src, "commit", "-m", "big")
	support.Git(t, src, "remote", "add", "origin", s.gitURL("/brk/repo.git", "alice"))
	support.Git(t, src, "push", "origin", "main")
	code, out = s.call(t, "POST", "/api/v1/repack", s.tokens["root"], map[string]any{"repo": "brk/repo"})
	require.Equal(t, 200, code, "%v", out)

	// Fixture sanity: an unbroken console-plane clone works.
	okDir := t.TempDir()
	support.Git(t, okDir, "clone", s.gitURL("/brk/repo.git", "alice"), "repo")
	require.FileExists(t, filepath.Join(okDir, "repo", "blob.bin"))

	// Inject the break at the storage seam; the chain under test is
	// git → console proxy → gateway → storage, exactly the incident's.
	s.objects.Backend = &breakBackend{ShardBackend: s.objects.Backend, breakAfter: 64 << 10}

	dst := t.TempDir()
	out2, err := support.GitTry(dst, "clone", s.gitURL("/brk/repo.git", "alice"), "repo")
	require.Error(t, err, "the clone must fail hard\n%s", out2)
	require.NotContains(t, out2, "body are still expected",
		"a broken stream reached git dressed as a complete response\n%s", out2)
}
