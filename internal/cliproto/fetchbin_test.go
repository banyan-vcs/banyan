package cliproto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/stretchr/testify/require"
)

func sampleResult() *FetchResult {
	big := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog\n", 2048)) // ~88KiB
	small := []byte("hello\n")
	return &FetchResult{
		Branch: "main", Head: "aa" + strings.Repeat("0", 38), Tree: "bb" + strings.Repeat("1", 38),
		Trees: map[string][]byte{"cc" + strings.Repeat("2", 38): []byte("100644 file\x00" + strings.Repeat("3", 40))},
		Blobs: map[string][]byte{
			"dd" + strings.Repeat("4", 38): big,
			"ee" + strings.Repeat("5", 38): small,
		},
		Pending: []string{},
	}
}

// The framing round-trips both ways; the parser sniffs the magic, so the
// client accepts whatever version the server negotiated (the multi-version
// contract: newer client + older server → v1 parses identically).
func TestFrameRoundTripBothVersions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(w *bytes.Buffer, res *FetchResult) error
	}{
		{"v1", func(w *bytes.Buffer, res *FetchResult) error { return WriteV1(w, res, model.SHA1) }},
		{"v2", func(w *bytes.Buffer, res *FetchResult) error { return WriteV2(w, res, model.SHA1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := sampleResult()
			var buf bytes.Buffer
			require.NoError(t, tc.write(&buf, src))
			got, err := Parse(&buf)
			require.NoError(t, err)
			require.Equal(t, src.Branch, got.Branch)
			require.Equal(t, src.Head, got.Head)
			require.Equal(t, src.Tree, got.Tree)
			require.Equal(t, src.Trees, got.Trees)
			require.Equal(t, src.Blobs, got.Blobs)
		})
	}
}

// v2's per-record zlib must actually shrink a compressible payload — the
// whole point of A1 (docs/operations/performance.md §9).
func TestV2Compresses(t *testing.T) {
	src := sampleResult()
	var v1, v2 bytes.Buffer
	require.NoError(t, WriteV1(&v1, src, model.SHA1))
	require.NoError(t, WriteV2(&v2, src, model.SHA1))
	require.Less(t, v2.Len(), v1.Len()/4, "v2 zlib frame should crush the repetitive payload: %d vs %d", v2.Len(), v1.Len())
	// And it is byte-stable: same input, same output (record order sorted).
	var again bytes.Buffer
	require.NoError(t, WriteV2(&again, src, model.SHA1))
	require.Equal(t, v2.Bytes(), again.Bytes())
}

// Delta records round-trip: the frame carries base oid + result size +
// the delta program, and storage.ApplyDelta(base, delta) reproduces the
// new blob exactly (the client-side contract of §9 C).
func TestDeltaRecordRoundTrip(t *testing.T) {
	base := []byte(strings.Repeat("line of base content for delta testing\n", 4096)) // ~148KiB
	edited := append(append(append([]byte{}, base[:60000]...), []byte("SMALL EDIT\n")...), base[60000:]...)
	newOID := string(model.ObjectOID(model.BlobType, edited, model.SHA1))
	baseOID := string(model.ObjectOID(model.BlobType, base, model.SHA1))
	delta := storage.DiffDelta(base, edited)
	require.NotNil(t, delta)
	require.Less(t, len(delta), 1024, "a small edit in a big file deltas small")

	src := &FetchResult{
		Branch: "main", Trees: map[string][]byte{}, Blobs: map[string][]byte{baseOID: base},
		Deltas: map[string]Delta{newOID: {Base: baseOID, Size: len(edited), Delta: delta}},
	}
	var buf bytes.Buffer
	require.NoError(t, WriteV2(&buf, src, model.SHA1))
	got, err := Parse(&buf)
	require.NoError(t, err)
	d, ok := got.Deltas[newOID]
	require.True(t, ok)
	require.Equal(t, baseOID, d.Base)
	require.Equal(t, len(edited), d.Size)
	applied, err := storage.ApplyDelta(base, d.Delta)
	require.NoError(t, err)
	require.Equal(t, edited, applied, "delta reproduces the new blob byte-for-byte")
}

// A corrupt magic fails loudly — the version sniff is the guard against
// feeding the parser a body that isn't ours.
func TestParseRejectsBadMagic(t *testing.T) {
	_, err := Parse(bytes.NewReader([]byte("not-banyan\nrest")))
	require.Error(t, err)
}
