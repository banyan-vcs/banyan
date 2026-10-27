package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Unit coverage for the streaming-serve primitives (docs/03 §3a): the
// delta-free streaming pack writer and the placement/size probes that pick
// the rung. Multi-pack serving over the wire is T55/T57 (integration).

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI not available")
	}
	full := append([]string{"-c", "core.autocrlf=false"}, args...)
	cmd := exec.Command("git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	require.NoError(t, cmd.Run(), "git %v (dir=%s) failed\n%s", args, dir, buf.String())
	return buf.String()
}

func TestStreamEncodePackRoundTrip(t *testing.T) {
	// A type zoo sized to cross any copy window: empty blob, small blobs,
	// a 200 KiB poorly-compressible blob, a tree and a commit.
	big := make([]byte, 200<<10)
	for i := range big {
		big[i] = byte(i*31 + i/251)
	}
	objs := []struct {
		t    model.ObjectType
		body []byte
	}{
		{model.BlobType, []byte("tiny blob\n")},
		{model.BlobType, []byte{}},
		{model.BlobType, big},
		{model.TreeType, []byte("100644 tiny.txt\x00\x11\x22\x33\x44\x55\x66\x77\x88\x99\xaa\xbb\xcc\xdd\xee\xff\x00\x11\x22\x33")},
		{model.CommitType, []byte("tree 0123456789012345678901234567890123456789\nauthor it <it@banyan.dev> 1 +0000\ncommitter it <it@banyan.dev> 1 +0000\n\nzoo\n")},
	}

	var buf bytes.Buffer
	require.NoError(t, StreamEncodePack(&buf, model.SHA1, len(objs), func(i int) (model.ObjectType, []byte, error) {
		return objs[i].t, objs[i].body, nil
	}))
	raw := buf.Bytes()

	dp, err := DecodePack(raw)
	require.NoError(t, err)
	require.Len(t, dp.Order, len(objs), "pack must carry exactly the enumerated objects")
	for _, o := range objs {
		oid := model.ObjectOID(o.t, o.body, model.SHA1)
		var got []byte
		switch o.t {
		case model.BlobType:
			got = dp.Blobs[oid]
		case model.TreeType:
			got = dp.Trees[oid]
		case model.CommitType:
			got = dp.Commits[oid]
		}
		assert.Equal(t, o.body, got, "%s body must round-trip byte-identical", o.t)
	}
	assert.True(t, VerifyPackChecksum(dp.PackOID, raw), "identity equals integrity: trailer checksum")

	// git oracle: real index-pack must accept the exact bytes, and the idx
	// it writes must carry one entry per object (show-index lists per line).
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", ".")
	packPath := filepath.Join(dir, "zoo.pack")
	require.NoError(t, os.WriteFile(packPath, raw, 0o644))
	gitRun(t, dir, "index-pack", packPath)
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(gitShowIndex(t, filepath.Join(dir, "zoo.idx"))), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	assert.Equal(t, len(objs), n, "git-accepted pack must expose every object via its idx")
}

// gitShowIndex runs `git show-index` with the idx on stdin (it reads only
// stdin, no path argument).
func gitShowIndex(t *testing.T, idxPath string) string {
	t.Helper()
	f, err := os.Open(idxPath)
	require.NoError(t, err)
	defer f.Close()
	var buf bytes.Buffer
	cmd := exec.Command("git", "show-index")
	cmd.Stdin = f
	cmd.Stdout, cmd.Stderr = &buf, &buf
	require.NoError(t, cmd.Run(), "git show-index failed\n%s", buf.String())
	return buf.String()
}

func TestStreamEncodePackEmpty(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, StreamEncodePack(&buf, model.SHA1, 0, func(int) (model.ObjectType, []byte, error) {
		t.Fatal("next must not be called for an empty pack")
		return "", nil, nil
	}))
	raw := buf.Bytes()
	assert.Len(t, raw, 32, "12-byte header + 20-byte trailer, nothing else")

	dp, err := DecodePack(raw)
	require.NoError(t, err)
	assert.Empty(t, dp.Order)

	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", ".")
	packPath := filepath.Join(dir, "empty.pack")
	require.NoError(t, os.WriteFile(packPath, raw, 0o644))
	gitRun(t, dir, "index-pack", packPath) // negotiation sends empty packs; git accepts
}

func TestStreamEncodePackRejectsBadInput(t *testing.T) {
	var buf bytes.Buffer
	assert.Error(t, StreamEncodePack(&buf, model.SHA1, -1, nil), "negative count")
	buf.Reset()
	assert.Error(t, StreamEncodePack(&buf, model.SHA1, 1<<32, nil), "count beyond pack range")

	sentinel := errors.New("boom")
	err := StreamEncodePack(io.Discard, model.SHA1, 3, func(i int) (model.ObjectType, []byte, error) {
		if i == 1 {
			return "", nil, sentinel
		}
		return model.BlobType, []byte("x"), nil
	})
	assert.ErrorIs(t, err, sentinel, "a next() error must propagate, not be swallowed mid-pack")

	err = StreamEncodePack(io.Discard, model.SHA1, 1, func(int) (model.ObjectType, []byte, error) {
		return model.ObjectType("ghost"), []byte("x"), nil
	})
	assert.ErrorContains(t, err, "ghost", "unsupported object types are rejected before any byte is written")
}

func TestPlacementsAndPackSizes(t *testing.T) {
	ctx := t.Context()
	meta := NewMemoryMetaStore()
	require.NoError(t, meta.CreateRepo(ctx, "r", model.SHA1))
	local := NewLocalShardStore(t.TempDir())
	svc := NewObjectService(meta, local, []Node{{ID: "n1", Local: true}}, model.SHA1)

	// One pack per blob: disjoint object sets, like a fragmented repo.
	ingestOne := func(body string) model.OID {
		blobOID := model.ObjectOID(model.BlobType, []byte(body), model.SHA1)
		var pack testBuffer
		require.NoError(t, EncodePackDelta(&pack, map[model.OID][]byte{blobOID: []byte(body)}, nil, nil, nil))
		packOID, err := svc.IngestPack(ctx, "r", pack.Bytes())
		require.NoError(t, err)
		return packOID
	}
	p1, p2 := ingestOne("alpha\n"), ingestOne("beta\n")
	require.NotEqual(t, p1, p2)
	aOID := model.ObjectOID(model.BlobType, []byte("alpha\n"), model.SHA1)
	bOID := model.ObjectOID(model.BlobType, []byte("beta\n"), model.SHA1)

	// ExactPackHit probes set equality over the typed enumeration (the map's
	// value is incidental — membership is what matters).
	hit, ok := svc.ExactPackHit("r", map[model.OID]model.ObjectType{aOID: model.BlobType})
	require.True(t, ok)
	assert.Equal(t, p1, hit)
	_, ok = svc.ExactPackHit("r", map[model.OID]model.ObjectType{aOID: model.BlobType, bOID: model.BlobType})
	assert.False(t, ok, "a closure spanning packs is by construction not an exact pack")

	// Placements: per-object pack grouping; the zero OID marks unplaced
	// (synthetic or unknown) — the streaming serve keys its groups on this.
	ghost := model.OID("deadbeefcafedeadbeefcafedeadbeefcafe")
	pl := svc.Placements(ctx, "r", []model.OID{aOID, bOID, ghost})
	assert.Equal(t, p1, pl[aOID])
	assert.Equal(t, p2, pl[bOID])
	assert.Equal(t, model.OID(""), pl[ghost], "unknown placement reports the zero OID")

	// CachedPackSize: the zero OID answers -1 without probing the backend
	// (regression: the pre-guard path built a backend file path from "" and
	// sliced out of range on the synthetic group).
	assert.EqualValues(t, -1, svc.CachedPackSize(ctx, "r", ""))
	s1 := svc.CachedPackSize(ctx, "r", p1)
	assert.Greater(t, s1, int64(0))
	assert.Equal(t, s1, svc.CachedPackSize(ctx, "r", p1), "sizes are content-addressed and cached forever")
	assert.EqualValues(t, -1, svc.CachedPackSize(ctx, "r", ghost), "never-ingested pack: unknown, not zero")
}

func TestPrefersWholePackCapabilityBits(t *testing.T) {
	assert.False(t, (&LocalBackend{}).PrefersWholePack(),
		"local single-object reads are file reads — escalating to whole-pack decode would only park the pack in memory")
	assert.True(t, (&ScatteredStore{}).PrefersWholePack(),
		"on the cluster wire each single-object read is an RPC; the read-amplify flipover is the cheaper path")
}
