package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// IngestPackFile (the streaming path) must land exactly what IngestPack
// (the in-memory path) lands: same object set registered, same objects
// readable, idx persisted — and the decoded-pack cache stays cold (reads
// resolve through the idx, per the large-push design).
func TestIngestPackFileEquivalence(t *testing.T) {
	ctx := context.Background()
	blobs := map[model.OID][]byte{}
	for _, s := range []string{"alpha", "beta", "gamma", strings_Repeat()} {
		blobs[model.ObjectOID(model.BlobType, []byte(s), model.SHA1)] = []byte(s)
	}
	var buf testBuffer
	require.NoError(t, EncodePack(&buf, blobs, nil, nil, nil))
	raw := buf.Bytes()

	mk := func(name string) *ObjectService {
		inner := NewMemoryMetaStore()
		require.NoError(t, inner.CreateRepo(ctx, name, model.SHA1))
		return NewObjectService(inner, NewLocalShardStore(t.TempDir()), []Node{{ID: "n1", Local: true}}, model.SHA1)
	}
	mem := mk("mem")
	_, err := mem.IngestPack(ctx, "mem", raw)
	require.NoError(t, err)

	stream := mk("stream")
	dir := t.TempDir()
	p := filepath.Join(dir, "push.pack")
	require.NoError(t, os.WriteFile(p, raw, 0o644))
	packOID, err := stream.IngestPackFile(ctx, "stream", p, int64(len(raw)))
	require.NoError(t, err)

	for oid, want := range blobs {
		_, got, rerr := stream.Raw(ctx, "stream", oid)
		require.NoError(t, rerr, "流式摄入后对象应可经 idx 读取: %s", oid)
		assert.Equal(t, string(want), string(got))
		_, got2, rerr := mem.Raw(ctx, "mem", oid)
		require.NoError(t, rerr)
		assert.Equal(t, string(got2), string(got), "两路径产物一致")
	}
	// The decoded-pack cache stays cold after a streaming ingest: reads
	// resolve through the idx offsets (the large-push memory contract).
	stream.mu.Lock()
	cold := len(stream.cache["stream"]) == 0
	stream.mu.Unlock()
	assert.True(t, cold, "流式摄入不应预热整包解码缓存")
	_ = packOID
}

func strings_Repeat() string { return "delta-delta-delta-payload" }
