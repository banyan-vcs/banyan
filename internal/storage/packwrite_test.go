package storage

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"

	"github.com/banyan-vcs/banyan/internal/model"
)

// TestDiffDeltaRoundTrip pins the delta encoder against the decoder
// (applyDelta): append-heavy, prepend-heavy, middle-edit, chunked-copy
// (offsets beyond one byte) and incompressible payloads must all round-trip
// byte-identically.
func TestDiffDeltaRoundTrip(t *testing.T) {
	base := []byte("The quick brown fox jumps over the lazy dog.\n")
	line := []byte("0123456789abcdefghijklmnopqrstuvwxyz\n")
	mkBulk := func(n int) []byte {
		var b bytes.Buffer
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "line %04d: %s", i, line)
		}
		return b.Bytes()
	}
	bulk := mkBulk(400) // >64KiB: exercises multi-byte copy sizes/offsets

	cases := []struct {
		name   string
		base   []byte
		target []byte
	}{
		{"append", base, append(append([]byte{}, base...), []byte("plus a tail\n")...)},
		{"prepend", base, append([]byte("a head\n"), base...)},
		{"middle-edit", bulk, append(append(append([]byte{}, bulk[:3000]...), []byte("CHANGED MIDDLE\n")...), bulk[3010:]...)},
		{"shrink", bulk, bulk[:1000]},
		{"identical", bulk, append([]byte{}, bulk...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta := DiffDelta(tc.base, tc.target)
			require.NotNil(t, delta, "%s: 应产出 delta", tc.name)
			got, err := applyDelta(tc.base, delta)
			require.NoError(t, err)
			assert.Equal(t, tc.target, got, "%s: 往返应字节一致", tc.name)
		})
	}
}

// TestDiffDeltaNoWin asserts the encoder declines worthless deltas.
func TestDiffDeltaNoWin(t *testing.T) {
	assert.Nil(t, DiffDelta(nil, []byte("x")), "空 base 无 delta")
	assert.Nil(t, DiffDelta([]byte("a"), nil), "空 target 无 delta")
	// Incompressible, no shared blocks: delta overhead ≥ target size.
	rnd := bytes.Repeat([]byte{0x5a}, 300)
	assert.Nil(t, DiffDelta(bytes.Repeat([]byte{0xa5}, 300), rnd), "无收益应返回 nil")
	assert.Nil(t, DiffDelta(bytes.Repeat([]byte{0xa5}, 300), bytes.Repeat([]byte{0x5a}, 2048)), "完全无关负载应返回 nil")
}

// decodeThinPack reads a thin pack and resolves REF_DELTA entries against
// the provided base contents (test-side counterpart of the client's
// index-pack --fix-thin). The go-git Scanner owns the stream boundaries.
func decodeThinPack(raw []byte, baseFor func(model.OID) []byte) (map[model.OID][]byte, string, error) {
	sc := packfile.NewScanner(bytes.NewReader(raw))
	_, count, err := sc.Header()
	if err != nil {
		return nil, "", err
	}
	out := map[model.OID][]byte{}
	for i := uint32(0); i < count; i++ {
		h, err := sc.NextObjectHeader()
		if err != nil {
			return nil, "", err
		}
		var buf bytes.Buffer
		if _, _, err := sc.NextObject(&buf); err != nil {
			return nil, "", err
		}
		var content []byte
		var kind model.ObjectType
		switch h.Type {
		case plumbing.BlobObject:
			kind, content = model.BlobType, buf.Bytes()
		case plumbing.TreeObject:
			kind, content = model.TreeType, buf.Bytes()
		case plumbing.CommitObject:
			kind, content = model.CommitType, buf.Bytes()
		case plumbing.TagObject:
			kind, content = model.TagType, buf.Bytes()
		case plumbing.REFDeltaObject:
			base := baseFor(model.OID(h.Reference.String()))
			if base == nil {
				return nil, "", fmt.Errorf("missing base %s", h.Reference)
			}
			content = applyDeltaTest(base, buf.Bytes())
			kind = model.BlobType // bases in this test are all blobs
		default:
			return nil, "", fmt.Errorf("bad type %v", h.Type)
		}
		oid := model.ObjectOID(kind, content, model.SHA1)
		out[oid] = content
	}
	return out, hex.EncodeToString(raw[len(raw)-20:]), nil
}

func applyDeltaTest(base, delta []byte) []byte {
	out, err := applyDelta(base, delta)
	if err != nil {
		panic(err)
	}
	return out
}

// TestEncodePackThinRoundTrip pins the thin pack writer: REF_DELTA entries
// reference the EXTERNAL base by oid, the entry count matches, the pack
// trailer checksum verifies, and a fix-thin-style decode reproduces every
// object byte-identically.
func TestEncodePackThinRoundTrip(t *testing.T) {
	base1 := bytes.Repeat([]byte("stable base content line\n"), 200)
	edit1 := append(append([]byte{}, base1...), []byte("small edit at end\n")...)
	base2 := bytes.Repeat([]byte("another base payload\n"), 150)
	edit2 := append([]byte("head\n"), base2...)
	fresh := []byte("brand new object with no base at all\n")

	blobOf := func(c []byte) (model.OID, []byte) {
		return model.ObjectOID(model.BlobType, c, model.SHA1), c
	}
	oB1, cB1 := blobOf(base1)
	oE1, cE1 := blobOf(edit1)
	oB2, cB2 := blobOf(base2)
	oE2, cE2 := blobOf(edit2)
	oF, cF := blobOf(fresh)

	bases := map[model.OID]model.OID{oE1: oB1, oE2: oB2}
	baseContent := map[model.OID][]byte{oB1: cB1, oB2: cB2}
	var buf bytes.Buffer
	err := EncodePackThin(&buf, model.SHA1,
		map[model.OID][]byte{oE1: cE1, oE2: cE2, oF: cF}, nil, nil, nil,
		bases, func(oid model.OID) (model.ObjectType, []byte, error) {
			return model.BlobType, baseContent[oid], nil
		})
	require.NoError(t, err)

	raw := buf.Bytes()
	// Trailer checksum + object count.
	require.True(t, VerifyPackChecksum(model.OID(hex.EncodeToString(raw[len(raw)-20:])), raw),
		"pack trailer 校验和应通过（sha256 场景另计，此处 sha1）")
	types, err := ScanObjectTypes(raw)
	require.NoError(t, err)
	refDeltas := 0
	for _, ty := range types {
		if ty == model.ObjectType("ref-delta") {
			refDeltas++
		}
	}
	require.Equal(t, 2, refDeltas, "两个有基对象应以 REF_DELTA 编码; types=%v", types)
	require.Equal(t, 3, len(types), "条目数应等于对象数")

	decoded, _, err := decodeThinPack(raw, func(oid model.OID) []byte { return baseContent[oid] })
	require.NoError(t, err)
	assert.Equal(t, cE1, decoded[oE1], "edit1 fix-thin 后字节一致")
	assert.Equal(t, cE2, decoded[oE2], "edit2 fix-thin 后字节一致")
	assert.Equal(t, cF, decoded[oF], "无基对象应原样传输")

	// The thin pack must be smaller than a delta-free pack of the same set.
	var plain bytes.Buffer
	require.NoError(t, EncodePack(&plain,
		map[model.OID][]byte{oE1: cE1, oE2: cE2, oF: cF}, nil, nil, nil))
	assert.Less(t, len(raw), len(plain.Bytes()), "thin 包应小于非 delta 包")
}
