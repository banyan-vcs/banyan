package storage

import (
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 64-bit offsets must survive the codec round trip: idx v2's 4-byte column
// carries offsets < 2^31 directly and marks larger ones (MSB=1) as indexes
// into the trailing 8-byte large-offset table — packs beyond 4GiB are
// first-class, git has no single-pack size limit.
func TestIdxLargeOffsetRoundTrip(t *testing.T) {
	offsets := map[model.OID]uint64{
		"0000000000000000000000000000000000000001": 12,           // inline
		"0000000000000000000000000000000000000002": 0x7fffffff,   // last inline value
		"0000000000000000000000000000000000000003": 0x80000000,   // first large
		"0000000000000000000000000000000000000004": 0xffffffff,   // 4GiB boundary
		"0000000000000000000000000000000000000005": 0x1234567890, // deep into 64-bit
	}
	crcs := map[model.OID]uint32{}
	for oid := range offsets {
		crcs[oid] = 0xdeadbeef
	}
	raw, err := EncodeIdx(model.SHA1, "aaaabbbbccccdddd000000000000000000000000", offsets, crcs)
	require.NoError(t, err)

	pi, err := DecodeIdx(raw, model.SHA1)
	require.NoError(t, err)
	assert.Equal(t, offsets, pi.Entries, "64 位偏移编解码往返（含大偏移表）")
	for oid := range offsets {
		assert.Equal(t, uint32(0xdeadbeef), pi.CRCs[oid])
	}
}

// Mixed shapes must not regress: inline-only packs decode exactly as before.
func TestIdxInlineOnly(t *testing.T) {
	offsets := map[model.OID]uint64{
		"0100000000000000000000000000000000000001": 12,
		"0100000000000000000000000000000000000002": 512,
	}
	raw, err := EncodeIdx(model.SHA1, "aaaabbbbccccdddd000000000000000000000000", offsets, nil)
	require.NoError(t, err)
	pi, err := DecodeIdx(raw, model.SHA1)
	require.NoError(t, err)
	assert.Equal(t, offsets, pi.Entries)
}
