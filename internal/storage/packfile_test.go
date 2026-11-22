package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
)

func blobOIDs(contents ...[]byte) map[model.OID][]byte {
	out := map[model.OID][]byte{}
	for _, c := range contents {
		out[model.ObjectOID(model.BlobType, c, model.SHA1)] = c
	}
	return out
}

// TestEncodePackDeltaDeltifies pins the P1-4 encoder-semantics fix: with a
// real window, near-identical blobs come out as one base + delta entries,
// and the pack still decodes to the exact original objects. (The previous
// call shape — delta flag passed as useRefDeltas with window 0 — produced
// delta-free packs while claiming the opposite.)
func TestEncodePackDeltaDeltifies(t *testing.T) {
	base := []byte(strings.Repeat("banyan pack encode test line\n", 1200)) // ~32KiB
	edit := append(append([]byte{}, base...), []byte("appended tail\n")...)

	var buf bytes.Buffer
	require.NoError(t, EncodePackDelta(&buf, blobOIDs(base, edit), nil, nil, nil))
	types, err := ScanObjectTypes(buf.Bytes())
	require.NoError(t, err)
	hasDelta := false
	for _, ty := range types {
		if ty == model.ObjectType("ofs-delta") {
			hasDelta = true
		}
	}
	assert.True(t, hasDelta, "near-identical blobs should deltify; types=%v", types)

	dp, err := DecodePack(buf.Bytes())
	require.NoError(t, err)
	require.Len(t, dp.Blobs, 2)
	for oid, content := range blobOIDs(base, edit) {
		assert.Equal(t, content, dp.Blobs[oid], "blob %s round-trip", oid)
	}
}

// TestEncodePackRefDelta mirrors the ofs-delta test for the REF_DELTA
// variant (v2 clients that did not send ofs-delta).
func TestEncodePackRefDelta(t *testing.T) {
	base := []byte(strings.Repeat("ref delta payload ", 2000))
	edit := append([]byte("head\n"), base...)
	var buf bytes.Buffer
	require.NoError(t, EncodePackDeltaRef(&buf, blobOIDs(base, edit), nil, nil, nil))
	types, err := ScanObjectTypes(buf.Bytes())
	require.NoError(t, err)
	hasRef := false
	for _, ty := range types {
		if ty == model.ObjectType("ref-delta") {
			hasRef = true
		}
	}
	assert.True(t, hasRef, "expected REF_DELTA entries; types=%v", types)

	dp, err := DecodePack(buf.Bytes())
	require.NoError(t, err)
	require.Len(t, dp.Blobs, 2)
	for oid, content := range blobOIDs(base, edit) {
		assert.Equal(t, content, dp.Blobs[oid], "blob %s round-trip", oid)
	}
}

// TestEncodePackDeltaFreeIsPlain asserts the small-fetch path stays
// delta-free (window 0) — the serve threshold keeps tiny packs cheap.
func TestEncodePackDeltaFreeIsPlain(t *testing.T) {
	treeContent := []byte("100644 small")
	commitContent := []byte("tree x")
	var buf bytes.Buffer
	require.NoError(t, EncodePack(&buf,
		blobOIDs([]byte("small blob")),
		map[model.OID][]byte{model.ObjectOID(model.TreeType, treeContent, model.SHA1): treeContent},
		map[model.OID][]byte{model.ObjectOID(model.CommitType, commitContent, model.SHA1): commitContent},
		nil))
	types, err := ScanObjectTypes(buf.Bytes())
	require.NoError(t, err)
	for _, ty := range types {
		assert.NotContains(t, []string{"ofs-delta", "ref-delta"}, string(ty), "EncodePack must stay delta-free")
	}
}

// TestVerifyPackShapeMath pins the shape auditor against the corruption the
// demo stack really produced: an entry stream that overshoots its header
// count (duplicate entries under an undercounted header), wrapped with a
// SELF-CONSISTENT trailer — so VerifyPackChecksum passes and go-git's
// Parser happily stops at the header count. Only the entry walk sees it.
func TestVerifyPackShapeMath(t *testing.T) {
	b := func(s string) (model.OID, []byte) {
		body := []byte("shape " + s)
		return model.ObjectOID(model.BlobType, body, model.SHA1), body
	}
	oidA, bA := b("A")
	oidB, bB := b("B")
	oidD, bD := b("D")

	var p1, p2 bytes.Buffer
	require.NoError(t, EncodePack(&p1, map[model.OID][]byte{oidA: bA, oidB: bB}, nil, nil, nil))
	require.NoError(t, EncodePack(&p2, map[model.OID][]byte{oidD: bD}, nil, nil, nil))

	// Clean packs pass.
	require.NoError(t, VerifyPackShape(p1.Bytes()))
	require.NoError(t, VerifyPackShape(p2.Bytes()))

	// The corrupted-merge shape: header says 2, entry stream carries 3
	// (p1's entries verbatim + p2's), trailer hashes the whole body — the
	// exact bytes tryAppendMerge used to write when its dedup trusted meta
	// rows that SQL meta had re-pointed away.
	var wbuf bytes.Buffer
	w := newPackWriter(&wbuf)
	w.write([]byte("PACK"))
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], 2) // undercount on purpose
	w.write(hdr[:])
	w.write(p1.Bytes()[12 : len(p1.Bytes())-20])
	w.write(p2.Bytes()[12 : len(p2.Bytes())-20])
	require.NoError(t, w.finish())
	err := VerifyPackShape(wbuf.Bytes())
	require.Error(t, err, "the undercounted stream must be rejected")
	assert.Contains(t, err.Error(), "trailing bytes")

	// Truncation fails too (entry body cut short).
	require.Error(t, VerifyPackShape(p1.Bytes()[:len(p1.Bytes())-25]))
}

// TestIngestPackRejectsUndercountedStream: registration is the last line of
// defense — a pack whose entry stream overshoots its header must never
// reach storage, where it would serve verbatim and break every clone.
func TestIngestPackRejectsUndercountedStream(t *testing.T) {
	ctx := context.Background()
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(ctx, "r", model.SHA1))
	root := t.TempDir()
	svc := NewObjectService(ms, NewLocalShardStore(root), []Node{{ID: "n1", Local: true}}, model.SHA1)

	b := func(s string) (model.OID, []byte) {
		body := []byte("gate " + s)
		return model.ObjectOID(model.BlobType, body, model.SHA1), body
	}
	oidA, bA := b("A")
	oidB, bB := b("B")
	oidD, bD := b("D")
	var p1, p2 bytes.Buffer
	require.NoError(t, EncodePack(&p1, map[model.OID][]byte{oidA: bA, oidB: bB}, nil, nil, nil))
	require.NoError(t, EncodePack(&p2, map[model.OID][]byte{oidD: bD}, nil, nil, nil))

	var wbuf bytes.Buffer
	w := newPackWriter(&wbuf)
	w.write([]byte("PACK"))
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	w.write(hdr[:])
	w.write(p1.Bytes()[12 : len(p1.Bytes())-20])
	w.write(p2.Bytes()[12 : len(p2.Bytes())-20])
	require.NoError(t, w.finish())

	_, err := svc.IngestPack(ctx, "r", wbuf.Bytes())
	require.Error(t, err, "ingest must reject the undercounted stream")
	assert.Contains(t, err.Error(), "trailing bytes")
	packs, perr := ms.ListPacks(ctx, "r")
	require.NoError(t, perr)
	assert.Empty(t, packs, "nothing registered")

	// And the spooled-file path rejects the same shape.
	tmp := filepath.Join(t.TempDir(), "bad.pack")
	require.NoError(t, os.WriteFile(tmp, wbuf.Bytes(), 0o644))
	_, err = svc.IngestPackFile(ctx, "r", tmp, int64(wbuf.Len()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing bytes")
}
