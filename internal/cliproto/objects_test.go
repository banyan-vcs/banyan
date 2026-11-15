package cliproto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObjectsFrameRoundTrip(t *testing.T) {
	objs := []ObjectRecord{
		{OID: strings.Repeat("a", 40), Type: "blob", Data: []byte(strings.Repeat("compressible uplink payload\n", 512))},
		{OID: strings.Repeat("b", 40), Type: "tree", Data: []byte("100644 f\x00" + strings.Repeat("c", 40))},
	}
	var buf bytes.Buffer
	require.NoError(t, WriteObjectsFrame(&buf, "repo1", "sha1", objs))
	// The uplink compression actually pays: 14KiB of repetitive payload
	// must ride far smaller than raw.
	require.Less(t, buf.Len(), len(objs[0].Data)/4, "zlib frames crush the repetitive payload: %d", buf.Len())

	repo, got, err := ParseObjectsFrame(&buf)
	require.NoError(t, err)
	require.Equal(t, "repo1", repo)
	require.Equal(t, objs, got)
}

func TestObjectsFrameRejectsBadInput(t *testing.T) {
	_, _, err := ParseObjectsFrame(bytes.NewReader([]byte("not-banyan\n")))
	require.Error(t, err, "bad magic fails loudly")
	// A hand-crafted frame with an unknown record kind: the writer would
	// never produce it, the parser must still refuse it.
	_, _, err = ParseObjectsFrame(bytes.NewReader([]byte(
		MagicObjectsV1+"\n"+`{"repo":"r","algo":"sha1"}`+"\n"+
			"x"+strings.Repeat("d", 40))))
	require.Error(t, err, "unknown record kinds fail loudly")
	// And the writer side validates types up front.
	var buf bytes.Buffer
	require.Error(t, WriteObjectsFrame(&buf, "r", "sha1", []ObjectRecord{{OID: strings.Repeat("d", 40), Type: "weird", Data: []byte("x")}}))
}
