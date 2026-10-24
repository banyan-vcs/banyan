package storage

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
