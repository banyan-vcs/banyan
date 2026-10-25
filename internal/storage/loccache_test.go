package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

// countingMeta wraps a MetaStore and counts LocateObjects round trips (the
// cluster-mode refs RPC, docs/11 B1).
type countingMeta struct {
	MetaStore
	locates int
	oids    int
}

func (c *countingMeta) LocateObjects(ctx context.Context, repo string, oids []model.OID) (map[model.OID]ObjectLocation, error) {
	c.locates++
	c.oids += len(oids)
	return c.MetaStore.LocateObjects(ctx, repo, oids)
}

func encodeTestPack(t *testing.T, contents ...string) []byte {
	t.Helper()
	blobs := map[model.OID][]byte{}
	for _, c := range contents {
		blobs[model.ObjectOID(model.BlobType, []byte(c), model.SHA1)] = []byte(c)
	}
	var buf testBuffer
	require.NoError(t, EncodePack(&buf, blobs, nil, nil, nil))
	return buf.Bytes()
}

type testBuffer struct{ data []byte }

func (b *testBuffer) Write(p []byte) (int, error) { b.data = append(b.data, p...); return len(p), nil }
func (b *testBuffer) Bytes() []byte               { return b.data }

// TestLocationCacheHitsAndInvalidation pins docs/11 P0-1 (B1):
//   - repeated reads of the same object stop re-locating (LRU);
//   - Prefetch batch-locates many oids in ONE Meta round trip;
//   - Known answers existence for a whole have list without object bytes;
//   - repack invalidates the repo's placements (objects only ever move at
//     repack), and the next read re-locates and still resolves correctly.
func TestLocationCacheHitsAndInvalidation(t *testing.T) {
	ctx := context.Background()
	inner := NewMemoryMetaStore()
	meta := &countingMeta{MetaStore: inner}
	local := NewLocalShardStore(t.TempDir())
	svc := NewObjectService(meta, local, []Node{{ID: "n1", Local: true}}, model.SHA1)
	require.NoError(t, inner.CreateRepo(ctx, "r", model.SHA1))

	pack1 := encodeTestPack(t, "alpha")
	pack2 := encodeTestPack(t, "beta")
	pack3 := encodeTestPack(t, "gamma")
	_, err := svc.IngestPack(ctx, "r", pack1)
	require.NoError(t, err)
	_, err = svc.IngestPack(ctx, "r", pack2)
	require.NoError(t, err)
	_, err = svc.IngestPack(ctx, "r", pack3)
	require.NoError(t, err)
	alpha := model.ObjectOID(model.BlobType, []byte("alpha"), model.SHA1)
	beta := model.ObjectOID(model.BlobType, []byte("beta"), model.SHA1)
	gamma := model.ObjectOID(model.BlobType, []byte("gamma"), model.SHA1)

	// First read locates once; the repeat is an LRU hit.
	_, data, err := svc.Raw(ctx, "r", alpha)
	require.NoError(t, err)
	assert.Equal(t, "alpha", string(data))
	first := meta.locates
	_, data, err = svc.Raw(ctx, "r", alpha)
	require.NoError(t, err)
	assert.Equal(t, "alpha", string(data))
	assert.Equal(t, first, meta.locates, "重复读同一对象不应重复定位")

	// Prefetch: one batched locate covers many oids.
	meta.locates = 0
	svc.Prefetch(ctx, "r", []model.OID{beta, gamma})
	assert.Equal(t, 1, meta.locates, "Prefetch 应一次 RPC 批量定位")
	_, data, err = svc.Raw(ctx, "r", beta)
	require.NoError(t, err)
	assert.Equal(t, "beta", string(data))
	assert.Equal(t, 1, meta.locates, "预取后读不应再定位")

	// Known: batch existence without object fetches.
	meta.locates = 0
	unknown := model.OID("deadbeefcafedeadbeefcafedeadbeefcafe")
	known := svc.Known(ctx, "r", []model.OID{alpha, beta, unknown})
	assert.Equal(t, 1, meta.locates, "Known 应一次批量定位")
	assert.True(t, known[alpha] && known[beta], "已知对象")
	assert.False(t, known[unknown], "未知对象")

	// Repack merges the packs and moves every object: the cache
	// invalidates, the next read re-locates and still resolves.
	_, rerr := svc.Repack(ctx, "r", 2, 0)
	require.NoError(t, rerr)
	meta.locates = 0
	_, data, err = svc.Raw(ctx, "r", alpha)
	require.NoError(t, err)
	assert.Equal(t, "alpha", string(data))
	assert.Greater(t, meta.locates, 0, "repack 失效后应重新定位")
	_, data, err = svc.Raw(ctx, "r", gamma)
	require.NoError(t, err)
	assert.Equal(t, "gamma", string(data))
}

// TestSynthBatchPacksOnce pins docs/11 P0-2 (B3): inside a batch, N
// synthetics persist as ONE pack; the pre-CAS flush (FlushSynthBatch)
// persists mid-batch without changing depth; the same objects are all
// readable afterwards.
func TestSynthBatchPacksOnce(t *testing.T) {
	ctx := context.Background()
	meta := NewMemoryMetaStore()
	local := NewLocalShardStore(t.TempDir())
	svc := NewObjectService(meta, local, []Node{{ID: "n1", Local: true}}, model.SHA1)
	require.NoError(t, meta.CreateRepo(ctx, "r", model.SHA1))

	svc.BeginSynthBatch("r")
	for i := 0; i < 5; i++ {
		body := []byte("synthetic commit body " + string(rune('a'+i)))
		oid := model.ObjectOID(model.CommitType, body, model.SHA1)
		svc.PutSynthetic("r", oid, model.CommitType, body)
	}
	// Mid-batch durability point (the pre-CAS flush): buffered objects
	// persist while the batch stays open.
	svc.FlushSynthBatch("r")
	packs, err := meta.ListPacks(ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, 1, len(packs), "pre-CAS flush 应产生一个合成 pack")

	for i := 5; i < 8; i++ {
		body := []byte("synthetic commit body " + string(rune('a'+i)))
		oid := model.ObjectOID(model.CommitType, body, model.SHA1)
		svc.PutSynthetic("r", oid, model.CommitType, body)
	}
	svc.EndSynthBatch("r")
	packs, err = meta.ListPacks(ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, 2, len(packs), "批次结束后余量应合为一个 pack（共 2 个）")

	// Every synthetic is readable and located (persisted, not memory-only).
	for i := 0; i < 8; i++ {
		body := []byte("synthetic commit body " + string(rune('a'+i)))
		oid := model.ObjectOID(model.CommitType, body, model.SHA1)
		svc.synthMu.Lock()
		delete(svc.synth["r"], oid) // force the pack path — prove persistence
		svc.synthMu.Unlock()
		typ, data, err := svc.Raw(ctx, "r", oid)
		require.NoError(t, err, "synthetic %d 应已持久化", i)
		assert.Equal(t, model.CommitType, typ)
		assert.Equal(t, body, data)
	}
}
