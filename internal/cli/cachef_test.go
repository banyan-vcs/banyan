package cli

// Cache hygiene (docs/operations/performance.md §9 F): sharded layout
// with legacy-flat migration on read, and mtime-ordered eviction to a
// byte cap — correctness never depends on the cache, so eviction only
// costs a re-fetch.
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCacheShardingAndMigration(t *testing.T) {
	dir := t.TempDir()
	ws := &workspace{root: dir}

	// New writes land sharded.
	ws.cachePut("ab"+strings.Repeat("2", 38), []byte("sharded"))
	entries, _ := os.ReadDir(filepath.Join(dir, cacheDir))
	require.Len(t, entries, 1, "one shard directory")
	require.Equal(t, "ab", entries[0].Name())

	// A legacy flat entry reads, and migrates on the hit.
	legacy := "cd" + strings.Repeat("1", 38)
	require.NoError(t, os.WriteFile(filepath.Join(dir, cacheDir, legacy), []byte("flat"), 0o644))
	got, ok := ws.cacheGet(legacy)
	require.True(t, ok)
	require.Equal(t, []byte("flat"), got)
	_, err := os.Stat(filepath.Join(dir, cacheDir, legacy))
	require.True(t, os.IsNotExist(err), "the flat entry migrated away")
	if _, err := os.Stat(filepath.Join(dir, cacheDir, "cd", legacy)); err != nil {
		t.Fatal("the sharded copy exists")
	}
}

func TestPruneCacheEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	ws := &workspace{root: dir}
	write := func(oid string, size int, age time.Duration) {
		ws.cachePut(oid, make([]byte, size))
		p := ws.cachePath(oid)
		old := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(p, old, old))
	}
	pad := strings.Repeat("0", 38)
	write("aa"+pad, 600, 3*time.Hour) // oldest → evicted first
	write("bb"+pad, 600, 2*time.Hour)
	write("cc"+pad, 600, 10*time.Minute)

	removed := ws.pruneCache(1000) // 1800 bytes live; two oldest must go to fit 1000
	require.EqualValues(t, 1200, removed)
	_, ok := ws.cacheGet("aa" + pad)
	require.False(t, ok, "the oldest entry left first")
	_, ok = ws.cacheGet("bb" + pad)
	require.False(t, ok, "and the next oldest until the cap holds")
	_, ok = ws.cacheGet("cc" + pad)
	require.True(t, ok, "the newest survives")
	// A no-op under the cap.
	require.Zero(t, ws.pruneCache(1<<30))
}
