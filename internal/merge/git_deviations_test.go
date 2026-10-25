// Documented deviations from `git merge-file` (found by auditing the
// go-git merge-ort port's xmerge against this implementation; see the
// package comment). Each case pins BOTH sides — what git does on this
// machine (skipped without git) and what banyan does — so the deltas are
// deliberate and visible, never accidental drift. Plus the binary-identity
// shortcut regression: identical sides must be clean even for binaries
// (git decides blob identity before ll_binary_merge; ours used to check
// binary first and ticket identical binaries).
package merge

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gitMergeFileBytes(t *testing.T, base, ours, theirs []byte) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	write := func(n string, b []byte) string {
		p := filepath.Join(dir, n)
		require.NoError(t, os.WriteFile(p, b, 0o644))
		return p
	}
	cmd := exec.Command("git", "merge-file", "-p", "-L", "ours", "-L", "base", "-L", "theirs",
		write("ours", ours), write("base", base), write("theirs", theirs))
	out, err := cmd.Output()
	conflicted := false
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		require.True(t, ok, "git merge-file: %v", err)
		conflicted = ee.ExitCode() > 0
	}
	return string(out), conflicted
}

func haveGit() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// Adjacent single-line edits: git's touch-fuse rule (xdl_append_merge:
// regions that merely touch on either side fuse, and fusion downgrades to
// a conflict) rejects them; banyan applies both — the wider, intended
// posture for convergence (both changes preserved, no overlap).
func TestDeviationAdjacentEditsMergeCleanly(t *testing.T) {
	if !haveGit() {
		t.Skip("git CLI not available")
	}
	base := []byte("l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\n")
	ours := []byte("l1\nOURS-2\nl3\nl4\nl5\nl6\nl7\nl8\n")
	theirs := []byte("l1\nl2\nTHEIRS-3\nl4\nl5\nl6\nl7\nl8\n")

	_, gitConflict := gitMergeFileBytes(t, base, ours, theirs)
	require.True(t, gitConflict, "git 侧应判相邻改动为冲突（前提失效，重验偏差前提）")

	got, clean := Blobs(base, ours, theirs, Options{})
	assert.True(t, clean, "banyan 应干净合并相邻行改动（有意宽于 git）")
	assert.Equal(t, "l1\nOURS-2\nTHEIRS-3\nl4\nl5\nl6\nl7\nl8\n", string(got),
		"两改都保留、按序叠加")
}

// Conflict spans render whole: no ZEALOUS refinement (xdl_refine_conflicts
// shrinks conflicts by inner-diffing the sides, shared runs >3 lines land
// outside the markers) and no ≤3-context fusion (xdl_simplify_non_conflicts).
// banyan keeps one block per conflict — only the ng diagnostics differ.
func TestDeviationConflictSpansRenderWhole(t *testing.T) {
	if !haveGit() {
		t.Skip("git CLI not available")
	}
	// Wide shared middle: git refines into two blocks (shared run >3).
	base := []byte("n1\nn2\nn3\nn4\nn5\nn6\nn7\nn8\nn9\nn10\n")
	ours := []byte("n1\nn2\nn3\nX4\ns1\ns2\ns3\ns4\nX8\nn9\nn10\n")
	theirs := []byte("n1\nn2\nn3\nY4\ns1\ns2\ns3\ns4\nY8\nn9\nn10\n")

	gitOut, gitConflict := gitMergeFileBytes(t, base, ours, theirs)
	require.True(t, gitConflict)
	assert.Equal(t, 2, bytes.Count([]byte(gitOut), []byte("<<<<<<<")),
		"git 侧应精化为两个冲突块（前提失效，重验偏差前提）")

	got, clean := Blobs(base, ours, theirs, Options{})
	assert.False(t, clean)
	assert.Equal(t, 1, bytes.Count(got, []byte("<<<<<<<")), "banyan 应整段一块（无精化）")
	assert.Contains(t, string(got), "<<<<<<< ours\nX4\ns1\ns2\ns3\ns4\nX8\n=======",
		"共同段应在 marker 内（与 git 相反的形态）")

	// Two conflict groups three context lines apart: git fuses to one
	// block; banyan keeps two.
	base2 := []byte("m1\nm2\nm3\nm4\nm5\nm6\nm7\nm8\nm9\nm10\nm11\nm12\n")
	ours2 := []byte("m1\nO2\nm3\nm4\nm5\nO6\nm7\nm8\nm9\nm10\nm11\nm12\n")
	theirs2 := []byte("m1\nT2\nm3\nm4\nm5\nT6\nm7\nm8\nm9\nm10\nm11\nm12\n")
	gitOut2, _ := gitMergeFileBytes(t, base2, ours2, theirs2)
	assert.Equal(t, 1, bytes.Count([]byte(gitOut2), []byte("<<<<<<<")),
		"git 侧应融合为一块（前提失效，重验偏差前提）")
	got2, clean2 := Blobs(base2, ours2, theirs2, Options{})
	assert.False(t, clean2)
	assert.Equal(t, 2, bytes.Count(got2, []byte("<<<<<<<")), "banyan 应保持两块（无 ≤3 行融合）")
}

// Blob identity is decided before the binary check (git merge-ort's
// content-merge entry does the same): identical sides, or one side equal
// to the base, stay clean even for binaries. Regression for the ticketed-
// identical-binaries bug.
func TestBinaryIdentityShortcuts(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0, 0, 0, 1} // NUL ⇒ binary
	other := append([]byte(nil), png...)
	other[7] = 2

	// Both sides pushed the same binary: clean, ours verbatim.
	got, clean := Blobs(png, png, png, Options{})
	assert.True(t, clean, "双侧相同二进制必须干净")
	assert.Equal(t, png, got)

	// Both changed to the same new binary (base differs): clean too.
	got, clean = Blobs(png, other, other, Options{})
	assert.True(t, clean, "双侧改为相同二进制必须干净")
	assert.Equal(t, other, got)

	// One side untouched: the other side's binary wins cleanly.
	got, clean = Blobs(png, png, other, Options{})
	assert.True(t, clean)
	assert.Equal(t, other, got)

	// Genuinely divergent binaries: never merged, ours kept, conflicted.
	got, clean = Blobs(png, other, append([]byte(nil), png[:7]...), Options{})
	assert.Equal(t, other, got, "分歧二进制应保留 ours")
	assert.False(t, clean, "真分歧二进制必须冲突（ll_binary_merge）")
}
