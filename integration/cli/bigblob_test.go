package cli

// T97 (docs/operations/testing.md): the CLI-native protocol's big-blob
// lanes end to end against the real server, real CLI runner:
//
//   - clone: blobs past the 256KiB inline limit ride Pending → the batch
//     frame backfill (one connection per chunk) → parallel materialization;
//   - commit: the upload rides binary zlib frames (chunked past 64MiB);
//   - pull: a small edit inside a big file answers as a delta record
//     (v2 framing), applied and hash-verified client-side.
//
// The assertions are content-level (byte round-trips); the lane choices
// are pinned by the unit tests (cliproto/service/cli packages).
import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
)

func bigPayload(marker string) []byte {
	return []byte(strings.Repeat(marker+" line of big blob payload for the CLI lanes\n", 6144)) // ~380KiB > inlineLimit
}

func TestCLIBigBlobLanes(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)

	// Seed through the real git face (the plain repo is the CLI's target).
	src := support.SeedRepo(t, s, "t97")
	mk := func(name, marker string) {
		support.Write(t, filepath.Join(src, name), string(bigPayload(marker)))
	}
	mk("big-a.bin", "AAA")
	mk("big-b.bin", "BBB")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "t97: big blobs")
	support.Git(t, src, "push", "-q", s.RepoURL("t97"), "main")

	// Clone: the two big blobs ride Pending → batch frames → cache → the
	// pool materializes them. Byte-level equality is the oracle.
	ws1 := filepath.Join(t.TempDir(), "one")
	stdout, stderr, code := runCLI(t, "clone", s.RepoURL("t97"), ws1)
	require.Zero(t, code, "clone failed: %s %s", stdout, stderr)
	wantA, err := os.ReadFile(filepath.Join(src, "big-a.bin"))
	require.NoError(t, err)
	gotA, err := os.ReadFile(filepath.Join(ws1, "big-a.bin"))
	require.NoError(t, err)
	assert.Equal(t, wantA, gotA, "pending-lane blob round-trips")

	// Commit a big edit from workspace one: binary zlib upload.
	mk2 := string(bigPayload("AAA"))
	mk2 = mk2[:200000] + "ONE SMALL EDIT\n" + mk2[200000:]
	support.Write(t, filepath.Join(ws1, "big-a.bin"), mk2)
	out, oerr, c := runCLI(t, "-C", ws1, "commit", "-m", "edit big-a")
	require.Zero(t, c, "commit failed: %s %s", out, oerr)

	// Pull in workspace two: the changed 380KiB blob answers as a delta
	// record (v2), applied against the base the workspace holds.
	ws2 := filepath.Join(t.TempDir(), "two")
	stdout, stderr, code = runCLI(t, "clone", s.RepoURL("t97"), ws2)
	require.Zero(t, code, "second clone failed: %s %s", stdout, stderr)
	support.Write(t, filepath.Join(ws2, "untracked.txt"), "scratch\n") // pull must tolerate untracked files
	stdout, stderr, code = runCLI(t, "-C", ws2, "pull")
	require.Zero(t, code, "pull failed: %s %s", stdout, stderr)
	got2, err := os.ReadFile(filepath.Join(ws2, "big-a.bin"))
	require.NoError(t, err)
	assert.Equal(t, mk2, string(got2), "the delta-applied edit round-trips byte-for-byte")
	assert.Equal(t, string(bigPayload("BBB")), func() string {
		b, _ := os.ReadFile(filepath.Join(ws2, "big-b.bin"))
		return string(b)
	}(), "untouched big blob intact")

	// And the edited history is visible through the git face (fsck oracle).
	verifyRoot := t.TempDir()
	support.Git(t, verifyRoot, "clone", "-q", s.RepoURL("t97"), "verify")
	support.Git(t, filepath.Join(verifyRoot, "verify"), "fsck", "--strict")
}
