package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

// TestDiskPackCacheLifecycle pins docs/12 P2-1: put/get round-trip, LRU
// eviction past the byte cap, and the restart rescan that rebuilds the
// index from the directory contents.
func TestDiskPackCacheLifecycle(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskPackCache(dir, 1200) // cap keeps exactly the two 600B packs
	require.NoError(t, err)

	p1 := model.OID("1111111111111111111111111111111111111111")
	p2 := model.OID("2222222222222222222222222222222222222222")
	p3 := model.OID("3333333333333333333333333333333333333333")

	require.NoError(t, c.Put("r", p1, []byte("pack-one")))
	raw, ok := c.Get("r", p1)
	require.True(t, ok, "已写入应可读回")
	assert.Equal(t, []byte("pack-one"), raw)

	// Overwrite under the same key does not double-count.
	require.NoError(t, c.Put("r", p1, []byte("pack-one")))
	entries, bytes := c.Stats()
	assert.Equal(t, 1, entries)
	assert.Equal(t, int64(len("pack-one")), bytes)

	// Two larger packs push p1 (least recently used) past the cap.
	big := make([]byte, 600)
	require.NoError(t, c.Put("r", p2, big))
	require.NoError(t, c.Put("r", p3, big))
	_, ok = c.Get("r", p1)
	assert.False(t, ok, "超限后 LRU 应淘汰最久未用条目")
	_, ok = c.Get("r", p2)
	assert.True(t, ok, "较新条目应保留")

	// Restart: a fresh cache instance rescans the same directory.
	c2, err := NewDiskPackCache(dir, 1<<20)
	require.NoError(t, err)
	raw, ok = c2.Get("r", p2)
	require.True(t, ok, "重启后重扫应恢复缓存条目")
	assert.Equal(t, big, raw)
	raw, ok = c2.Get("r", p3)
	require.True(t, ok)

	// Path-shaped names are refused (never written, never read).
	bad := model.OID("../escape")
	require.NoError(t, c2.Put("a/b", p1, []byte("x")))
	_, ok = c2.Get("a/b", p1)
	assert.False(t, ok, "路径形仓库名应被拒绝缓存")
	require.NoError(t, c2.Put("r", bad, []byte("x")))
	_, ok = c2.Get("r", bad)
	assert.False(t, ok, "路径形 packOID 应被拒绝缓存")
}
