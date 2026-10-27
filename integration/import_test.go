package integration

// T59 (docs/13): banyan import — bulk migration of an existing git
// repository, segment by segment (docs/03 §5 分批导入). A six-commit repo of
// incompressible 40 KiB blobs under a 60 KiB segment budget must arrive as
// ≥2 segments (each push its own pack — the fragments are first-class, no
// repack), the imported history must be byte-faithful to the source, and a
// rerun must resume to a no-op. This closes the "way in" half of the
// large-repository story: one command, bounded server memory at any scale.

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"strings"
	"testing"

	"github.com/banyan-vcs/banyan/internal/cli"
	"github.com/stretchr/testify/require"
)

func TestImportSegmented(t *testing.T) {
	needGit(t)
	srv := newServer(t)

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	for i := 0; i < 6; i++ {
		blob := make([]byte, 40<<10)
		_, _ = rand.Read(blob)
		require.NoError(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d.bin", i)), blob, 0o644))
		git(t, src, "add", ".")
		git(t, src, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}

	var out, errb bytes.Buffer
	code := cli.Run([]string{"import", src, srv.repoURL("t59"), "--branch", "main", "--segment-bytes", "60KiB"}, &out, &errb)
	require.Equal(t, 0, code, "import exit code\nstdout:\n%s\nstderr:\n%s", out.String(), errb.String())
	require.Contains(t, out.String(), "6 commits", "plan line: %s", out.String())
	require.Contains(t, out.String(), "segment 2/", "the budget must force multiple segments: %s", out.String())

	// The imported history is byte-faithful and complete.
	dst := t.TempDir()
	git(t, dst, "clone", "-q", srv.repoURL("t59"), "r")
	c := filepath.Join(dst, "r")
	require.Equal(t, "6", strings.TrimSpace(git(t, c, "rev-list", "--count", "HEAD")))
	git(t, c, "fsck", "--no-dangling")
	for i := 0; i < 6; i++ {
		want, err := os.ReadFile(filepath.Join(src, fmt.Sprintf("f%d.bin", i)))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(c, fmt.Sprintf("f%d.bin", i)))
		require.NoError(t, err)
		require.Equal(t, want, got, "f%d.bin byte-identical", i)
	}

	// Segments are real server-side fragments: each push its own pack.
	resp, err := http.Get(srv.URL + "/api/v1/repos/t59/size")
	require.NoError(t, err)
	defer resp.Body.Close()
	var size struct {
		Packs int `json:"packs"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&size))
	require.GreaterOrEqual(t, size.Packs, 2, "segmented import must leave multi-pack state, got %d", size.Packs)
	t.Logf("imported 6 commits as %d packs", size.Packs)

	// A rerun resumes to a no-op.
	out.Reset()
	errb.Reset()
	code = cli.Run([]string{"import", src, srv.repoURL("t59"), "--branch", "main", "--segment-bytes", "60KiB"}, &out, &errb)
	require.Equal(t, 0, code, "rerun exit code\n%s", errb.String())
	require.Contains(t, out.String(), "up to date", "rerun: %s", out.String())

	// Dry-run reports the plan and pushes nothing.
	more := t.TempDir()
	git(t, more, "init", "-q", "-b", "main", ".")
	blob := make([]byte, 40<<10)
	_, _ = rand.Read(blob)
	require.NoError(t, os.WriteFile(filepath.Join(more, "extra.bin"), blob, 0o644))
	git(t, more, "add", ".")
	git(t, more, "commit", "-q", "-m", "extra")
	out.Reset()
	errb.Reset()
	code = cli.Run([]string{"import", more, srv.repoURL("t59x"), "--segment-bytes", "1MiB", "--dry-run"}, &out, &errb)
	require.Equal(t, 0, code, "dry-run exit\n%s", errb.String())
	require.Contains(t, out.String(), "1 commits, 1 segments", "dry-run plan: %s", out.String())
	dry, err := http.Get(srv.URL + "/api/v1/repos/t59x/size")
	require.NoError(t, err)
	defer dry.Body.Close()
	require.Equal(t, http.StatusNotFound, dry.StatusCode, "dry-run must not create the repo")
}
