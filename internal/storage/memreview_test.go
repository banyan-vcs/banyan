package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

// TestMemoizeBudgetEviction pins the decoded-pack LRU byte budget: past
// MemoMaxBytes the least recently used pack drops from the cache while
// packOrder (metadata-only, ExactPackHit input) keeps every pack.
func TestMemoizeBudgetEviction(t *testing.T) {
	ctx := context.Background()
	inner := NewMemoryMetaStore()
	local := NewLocalShardStore(t.TempDir())
	svc := NewObjectService(inner, local, []Node{{ID: "n1", Local: true}}, model.SHA1)
	require.NoError(t, inner.CreateRepo(ctx, "r", model.SHA1))
	svc.MemoMaxBytes = 16 // tiny budget: one small pack at a time

	_, err := svc.IngestPack(ctx, "r", encodeTestPack(t, "alpha-beta-gamma"))
	require.NoError(t, err)
	_, err = svc.IngestPack(ctx, "r", encodeTestPack(t, "one-two-three-four"))
	require.NoError(t, err)

	svc.mu.Lock()
	var cached int
	for _, dp := range svc.cache["r"] {
		cached++
		_ = dp
	}
	orders := len(svc.packOrder["r"])
	lruLen := svc.cacheLRU.Len()
	svc.mu.Unlock()
	assert.Equal(t, 1, cached, "超预算后最旧解码包应被逐出")
	assert.Equal(t, 1, lruLen, "LRU 账目应与缓存一致")
	assert.Equal(t, 2, orders, "packOrder 是元数据（ExactPackHit 输入），逐出不应丢")

	// The evicted pack still reads (miss refetches through the backend).
	evicted := model.ObjectOID(model.BlobType, []byte("alpha-beta-gamma"), model.SHA1)
	_, data, rerr := svc.Raw(ctx, "r", evicted)
	require.NoError(t, rerr)
	assert.Equal(t, "alpha-beta-gamma", string(data))
}

// TestRepackByteCapSplitsGroups pins the merge-group input byte cap: with a
// cap smaller than any pack's bytes the geometric group splits into
// singletons and nothing merges; with the default cap the same repo merges.
func TestRepackByteCapSplitsGroups(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T) *ObjectService {
		inner := NewMemoryMetaStore()
		local := NewLocalShardStore(t.TempDir())
		svc := NewObjectService(inner, local, []Node{{ID: "n1", Local: true}}, model.SHA1)
		require.NoError(t, inner.CreateRepo(ctx, "r", model.SHA1))
		for _, c := range []string{"alpha", "beta", "gamma"} {
			_, err := svc.IngestPack(ctx, "r", encodeTestPack(t, c))
			require.NoError(t, err)
		}
		return svc
	}

	tiny := seed(t)
	res, err := tiny.Repack(ctx, "r", 2, 1) // 1-byte cap: every group splits
	require.NoError(t, err)
	assert.Equal(t, res.PacksBefore, res.PacksAfter, "字节上限应把组拆成单元素——零合并")

	normal := seed(t)
	res, err = normal.Repack(ctx, "r", 2, 0) // default cap: merge as before
	require.NoError(t, err)
	assert.Less(t, res.PacksAfter, res.PacksBefore, "默认上限下碎片应合并")
	for _, c := range []string{"alpha", "beta", "gamma"} {
		oid := model.ObjectOID(model.BlobType, []byte(c), model.SHA1)
		_, data, rerr := normal.Raw(ctx, "r", oid)
		require.NoError(t, rerr)
		assert.Equal(t, c, string(data))
	}
}
