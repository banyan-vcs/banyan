package cli

// The scan stat cache (docs/operations/performance.md §9 B): a tracked
// file whose size+mtime match the manifest skips the read and the hash.
// The observable contract is behavioral — a stale-stat file (content
// clean) refreshes the manifest, a stat-hit file never enters Modified
// even when the manifest's stat was just learned, and a real content
// change (stat differs, hash differs) still reports Modified.
import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScanStatCache(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}

	// A clean file with a learned stat: scan must not report it.
	p := mk("a.txt", "hello\n")
	info, err := os.Stat(p)
	require.NoError(t, err)
	ws := &workspace{root: dir, man: manifest{Files: map[string]fileState{
		"a.txt": {OID: blobHash([]byte("hello\n")), Mode: "100644", Size: info.Size(), Mtime: info.ModTime().UnixNano()},
	}}}
	st, err := ws.scan()
	require.NoError(t, err)
	require.False(t, st.dirty(), "stat hit: no read, no hash, clean")
	require.False(t, ws.statTouched, "nothing to learn on a stat hit")

	// Touch mtime only (content identical): scan re-hashes, finds it
	// clean and LEARNS the fresh stat — flushStat persists it.
	future := info.ModTime().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(p, future, future))
	st, err = ws.scan()
	require.NoError(t, err)
	require.False(t, st.dirty(), "content clean after a touch")
	require.True(t, ws.statTouched, "stale stat learned")
	ws.flushStat()
	require.False(t, ws.statTouched)
	// And the learned stat now hits:
	st, err = ws.scan()
	require.NoError(t, err)
	require.False(t, st.dirty() || ws.statTouched, "learned stat hits")

	// A real edit (size changes): Modified, and the learned stat of the
	// OTHER file survives untouched.
	require.NoError(t, os.WriteFile(p, []byte("hello world\n"), 0o644))
	st, err = ws.scan()
	require.NoError(t, err)
	require.Equal(t, []string{"a.txt"}, st.Modified)

	// An older stat-less manifest entry (no size/mtime): first scan
	// hashes, finds it clean and backfills the stat.
	dir2 := t.TempDir()
	p2 := filepath.Join(dir2, "b.txt")
	require.NoError(t, os.WriteFile(p2, []byte("x"), 0o644))
	ws2 := &workspace{root: dir2, man: manifest{Files: map[string]fileState{
		"b.txt": {OID: blobHash([]byte("x")), Mode: "100644"},
	}}}
	st, err = ws2.scan()
	require.NoError(t, err)
	require.False(t, st.dirty(), "clean content, stat-less manifest")
	require.True(t, ws2.statTouched, "stat backfilled for the migration case")
	learned := ws2.man.Files["b.txt"]
	require.NotZero(t, learned.Mtime)
	require.EqualValues(t, 1, learned.Size)

	// Deletion still reports by stat alone.
	require.NoError(t, os.Remove(p2))
	st, err = ws2.scan()
	require.NoError(t, err)
	require.Equal(t, []string{"b.txt"}, st.Deleted)
}
