package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
)

// TestExtractObjectRoundTrip pins docs/11 P1-1 (B2): every object of a
// delta-encoded pack (bases AND delta entries) extracts byte-identically via
// idx offsets, without a full-pack decode.
func TestExtractObjectRoundTrip(t *testing.T) {
	base := []byte("alpha line of a fairly compressible blob payload\n")
	blobs := map[model.OID][]byte{}
	contents := [][]byte{base}
	for i := 1; i <= 6; i++ {
		ed := append([]byte{}, base...)
		for j := 0; j < i*3; j++ {
			ed = append(ed, []byte("tail variant segment\n")...)
		}
		contents = append(contents, ed)
	}
	for _, c := range contents {
		blobs[model.ObjectOID(model.BlobType, c, model.SHA1)] = c
	}
	treeBody := []byte("100644 big\n")
	trees := map[model.OID][]byte{
		model.ObjectOID(model.TreeType, treeBody, model.SHA1): treeBody,
	}
	commitBody := []byte("tree somewhere\nauthor it <it@banyan.dev> 1 +0000\n\nmsg")
	commits := map[model.OID][]byte{
		model.ObjectOID(model.CommitType, commitBody, model.SHA1): commitBody,
	}

	var pack testBuffer
	require.NoError(t, EncodePackDelta(&pack, blobs, trees, commits, nil))
	raw := pack.Bytes()

	// The pack must actually deltify for this test to cover the delta path.
	types, err := ScanObjectTypes(raw)
	require.NoError(t, err)
	hasDelta := false
	for _, ty := range types {
		if ty == model.ObjectType("ofs-delta") {
			hasDelta = true
		}
	}
	require.True(t, hasDelta, "测试前置：相似 blob 应已 delta 化; types=%v", types)

	// Ground truth + idx from the same pack.
	dp, err := DecodePack(raw)
	require.NoError(t, err)
	require.NotEmpty(t, dp.Offsets, "测试前置：idx 偏移配对应成功")
	idxRaw, err := EncodeIdx(model.SHA1, dp.PackOID, dp.Offsets, dp.CRCs)
	require.NoError(t, err)
	idx, err := DecodeIdx(idxRaw, model.SHA1)
	require.NoError(t, err)

	for oid, want := range blobs {
		typ, got, err := ExtractObject(raw, idx, model.SHA1, oid)
		require.NoError(t, err, "blob %s 提取失败", oid)
		assert.Equal(t, model.BlobType, typ)
		assert.Equal(t, want, got, "blob %s 字节往返", oid)
	}
	for oid, want := range trees {
		typ, got, err := ExtractObject(raw, idx, model.SHA1, oid)
		require.NoError(t, err)
		assert.Equal(t, model.TreeType, typ)
		assert.Equal(t, want, got)
	}
	for oid, want := range commits {
		typ, got, err := ExtractObject(raw, idx, model.SHA1, oid)
		require.NoError(t, err)
		assert.Equal(t, model.CommitType, typ)
		assert.Equal(t, want, got)
	}

	// Unknown oid → a clean not-found.
	_, _, err = ExtractObject(raw, idx, model.SHA1, model.OID("deadbeefcafedeadbeefcafedeadbeefcafe"))
	assert.ErrorIs(t, err, model.ErrObjectNotFound)
}

// TestLocalGetObjectViaDisk runs the extraction through the real local
// shard store (pack + idx files on disk) — the all-in-one miss path.
func TestLocalGetObjectViaDisk(t *testing.T) {
	ctx := t.Context()
	meta := meta.NewMemoryMetaStore()
	require.NoError(t, meta.CreateRepo(ctx, "r", model.SHA1))
	local := NewLocalShardStore(t.TempDir())
	svc := NewObjectService(meta, local, []Node{{ID: "n1", Local: true}}, model.SHA1)

	base := []byte("payload that repeats enough to deltify nicely\n")
	blobs := map[model.OID][]byte{
		model.ObjectOID(model.BlobType, base, model.SHA1): base,
	}
	edit := append(append([]byte{}, base...), []byte("plus a small edit\n")...)
	blobs[model.ObjectOID(model.BlobType, edit, model.SHA1)] = edit
	var pack testBuffer
	require.NoError(t, EncodePackDelta(&pack, blobs, nil, nil, nil))
	_, err := svc.IngestPack(ctx, "r", pack.Bytes())
	require.NoError(t, err)

	// Cold path: decoded cache holds the pack from ingest; force the
	// extraction path by clearing it (simulates a restarted process).
	svc.mu.Lock()
	svc.cache["r"] = map[model.OID]*DecodedPack{}
	svc.mu.Unlock()

	for _, c := range [][]byte{base, edit} {
		oid := model.ObjectOID(model.BlobType, c, model.SHA1)
		typ, data, err := svc.Raw(ctx, "r", oid)
		require.NoError(t, err, "提取 %s", oid)
		assert.Equal(t, model.BlobType, typ)
		assert.Equal(t, c, data)
	}
}

// TestExtractVerifiesContentHash pins the extraction path's final content
// verification: the idx-guided inflate reads raw pack bytes, and a corrupted
// copy can inflate into WRONG-but-parseable bytes (a flip inside a tree's
// entry region parses into a garbage child oid — the flake the HA corrupt
// test caught in the wild). The requested oid is the truth: a mismatch is
// an error so the scattered-store caller falls through to the next holder.
// Simulated here by pointing the idx at the wrong offset — inflate succeeds,
// content is not the requested object.
func TestExtractVerifiesContentHash(t *testing.T) {
	b1 := []byte("first blob body, quite compressible\n")
	b2 := []byte("second blob body, also quite compressible\n")
	o1 := model.ObjectOID(model.BlobType, b1, model.SHA1)
	o2 := model.ObjectOID(model.BlobType, b2, model.SHA1)
	var pack testBuffer
	require.NoError(t, EncodePackDelta(&pack, map[model.OID][]byte{o1: b1, o2: b2}, nil, nil, nil))
	raw := pack.Bytes()
	dp, err := DecodePack(raw)
	require.NoError(t, err)
	idxRaw, err := EncodeIdx(model.SHA1, dp.PackOID, dp.Offsets, dp.CRCs)
	require.NoError(t, err)
	idx, err := DecodeIdx(idxRaw, model.SHA1)
	require.NoError(t, err)

	// Sanity: the honest idx round-trips.
	typ, got, err := ExtractObject(raw, idx, model.SHA1, o1)
	require.NoError(t, err)
	assert.Equal(t, model.BlobType, typ)
	assert.Equal(t, b1, got)

	// The corruption shape: o1's idx entry points at o2's bytes. Inflate
	// succeeds, the content is parseable — only the hash check stands
	// between it and the caller.
	lied := &PackIndex{Entries: map[model.OID]uint64{
		o1: idx.Entries[o2],
		o2: idx.Entries[o1],
	}, CRCs: idx.CRCs, OIDOrder: idx.OIDOrder, PackOID: idx.PackOID}
	_, _, err = ExtractObject(raw, lied, model.SHA1, o1)
	require.Error(t, err, "错内容的成功 inflate 必须被内容哈希终验击落")
	assert.Contains(t, err.Error(), "hash mismatch")
}
