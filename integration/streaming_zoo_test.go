package integration

// T57 (docs/13): the type zoo through the multi-pack streaming rung — the
// correctness counterpart to T55's memory proof. Two pushes (~18 MiB of
// incompressible blobs each, no repack) keep the closure spanning two packs
// past the streamServeMinPackBytes cutoff, and the second commit plants the
// entries whose enumeration is mode/type-directed: an executable, a symlink,
// a gitlink into another repo, and an annotated tag at the tip. The clone
// must arrive complete — tag object included (git clones tag objects),
// gitlink excluded (this store never holds it), modes intact.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banyan-vcs/banyan/internal/debug"
	"github.com/stretchr/testify/require"
)

func TestStreamingCloneTypeZoo(t *testing.T) {
	needGit(t)

	srv := newServer(t)
	ops := httptest.NewServer(debug.Handler())
	t.Cleanup(ops.Close)
	counter := func(name string) int64 {
		resp, err := http.Get(ops.URL + "/debug/vars")
		require.NoError(t, err)
		defer resp.Body.Close()
		var vars map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&vars))
		v, ok := vars[name].(float64)
		require.True(t, ok, "counter %s missing: %v", name, vars)
		return int64(v)
	}

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	git(t, src, "remote", "add", "origin", srv.repoURL("t57"))

	blobRound := func(round int) {
		for i := 0; i < 9; i++ {
			blob := make([]byte, 2<<20)
			_, _ = rand.Read(blob)
			require.NoError(t, os.WriteFile(
				filepath.Join(src, fmt.Sprintf("r%d-b%02d.bin", round, i)), blob, 0o644))
		}
	}
	blobRound(0)
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "round 0")
	git(t, src, "push", "origin", "main")

	// Round 1: the zoo. Executable bit, symlink entry (target path as a
	// blob), gitlink to a commit in a repo this store never sees, and an
	// annotated tag at the tip.
	blobRound(1)
	write(t, filepath.Join(src, "script.sh"), "#!/bin/sh\necho zoo\n")
	git(t, src, "add", ".")
	git(t, src, "update-index", "--chmod=+x", "script.sh")

	// The symlink blob is hashed from outside the worktree so `git add .`
	// never sees the staging file.
	staging := t.TempDir()
	stagingPath := filepath.Join(staging, "link-target")
	require.NoError(t, os.WriteFile(stagingPath, []byte("r0-b00.bin"), 0o644))
	linkOID := strings.TrimSpace(git(t, src, "hash-object", "-w", stagingPath))
	git(t, src, "update-index", "--add", "--cacheinfo", "120000,"+linkOID+",link")

	sub := t.TempDir()
	git(t, sub, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(sub, "f.txt"), "external repo content\n")
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-q", "-m", "external")
	subOID := strings.TrimSpace(git(t, sub, "rev-parse", "HEAD"))
	git(t, src, "update-index", "--add", "--cacheinfo", "160000,"+subOID+",vendor")

	git(t, src, "commit", "-q", "-m", "round 1: exec + symlink + gitlink")
	git(t, src, "tag", "-a", "v1", "-m", "zoo tag")
	git(t, src, "push", "origin", "main", "refs/tags/v1")
	tip := strings.TrimSpace(git(t, src, "rev-parse", "HEAD"))

	streamed0 := counter("banyan_fetch_streamed")
	dst := t.TempDir()
	git(t, dst, "clone", srv.repoURL("t57"), "r")
	c := filepath.Join(dst, "r")
	require.Equal(t, streamed0+1, counter("banyan_fetch_streamed"),
		"the type-zoo clone took the streaming rung (closure spans both packs)")

	// The annotated tag object itself must arrive — git clones tag objects,
	// and the type-directed enumeration must keep it in the served set.
	require.Equal(t, "tag", strings.TrimSpace(git(t, c, "cat-file", "-t", "v1")))
	require.Equal(t, tip, strings.TrimSpace(git(t, c, "rev-parse", "v1^{commit}")),
		"the tag peels to the source tip")

	// Mode-directed entries survive the round trip (ls-files -s: mode oid stage\t path).
	ls := git(t, c, "ls-files", "-s")
	scriptOID := strings.TrimSpace(git(t, c, "rev-parse", "HEAD:script.sh"))
	require.Contains(t, ls, "100755 "+scriptOID+" 0\tscript.sh", "exec bit intact:\n%s", ls)
	require.Contains(t, ls, "120000 "+linkOID+" 0\tlink", "symlink entry intact:\n%s", ls)
	require.Contains(t, ls, "160000 "+subOID+" 0\tvendor", "gitlink pin intact:\n%s", ls)

	// Content spot checks: one incompressible blob per round plus the zoo text.
	for round := 0; round < 2; round++ {
		want, err := os.ReadFile(filepath.Join(src, fmt.Sprintf("r%d-b00.bin", round)))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(c, fmt.Sprintf("r%d-b00.bin", round)))
		require.NoError(t, err)
		require.Equal(t, want, got, "round %d blob round-trips byte-for-byte", round)
	}
	got, err := os.ReadFile(filepath.Join(c, "script.sh"))
	require.NoError(t, err)
	require.Equal(t, "#!/bin/sh\necho zoo\n", string(got))
}
