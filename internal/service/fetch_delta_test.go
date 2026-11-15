package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// TestFetchTreesDelta — the pull lane's delta decision (docs/operations/
// performance.md §9 C): a changed blob past deltaMinBytes answers as a
// delta against the version the client provably holds, ONLY when the
// client asked for the v2 framing ("binz"); the v1/JSON ladder keeps
// whole blobs untouched.
func TestFetchTreesDelta(t *testing.T) {
	s := newService(t)
	base := []byte(strings.Repeat("base block for the delta lane test\n", 4096)) // ~148KiB ≥ 64KiB
	edited := append(append(append([]byte{}, base[:100000]...),
		[]byte("ONE SMALL EDIT\n")...), base[100000:]...)
	files := map[string]string{"big.bin": string(base)}
	head1, tree1 := commitRoot(t, s, "deltarepo", files)

	putFiles(t, s, "deltarepo", map[string]string{"big.bin": string(edited)})
	res2, err := s.CreateCommit(context.Background(), CommitRequest{
		Repo: "deltarepo", Base: head1, Message: "edit big", Changes: changesFor(map[string]string{"big.bin": string(edited)}),
	})
	require.NoError(t, err)
	newOID := string(model.ObjectOID(model.BlobType, edited, model.SHA1))
	oldOID := string(model.ObjectOID(model.BlobType, base, model.SHA1))

	// v2 request: the changed blob arrives as a delta, not a whole blob.
	r2, err := s.FetchTrees(context.Background(), FetchRequest{Repo: "deltarepo", HaveTree: tree1, Format: "binz"})
	require.NoError(t, err)
	d, ok := r2.Deltas[newOID]
	require.True(t, ok, "binz pull answers the big edit as a delta record")
	assert.Equal(t, oldOID, string(d.Base))
	assert.NotContains(t, r2.Blobs, newOID, "the whole blob does not ALSO ride inline")
	assert.Less(t, len(d.Delta), 2048, "a small edit in a 148KiB file deltas tiny")

	// The client contract: base + delta reproduces the new blob exactly.
	got, err := storage.ApplyDelta(base, d.Delta)
	require.NoError(t, err)
	assert.Equal(t, edited, got)
	assert.Equal(t, res2.Tree, r2.Tree, "the diff still lands the new view tree")

	// v1 request on the same state: no delta records — whole blobs only.
	r1, err := s.FetchTrees(context.Background(), FetchRequest{Repo: "deltarepo", HaveTree: tree1, Format: "bin"})
	require.NoError(t, err)
	assert.Empty(t, r1.Deltas)
	assert.Contains(t, r1.Blobs, newOID, "v1 keeps the whole-blob ladder")

	// A small changed blob (below deltaMinBytes) never deltifies —
	// the frame's zlib already carries it.
	small, smallEdit := "tiny payload v1\n", "tiny payload v2\n"
	headS1, treeS1 := commitRoot(t, s, "smallrepo", map[string]string{"s.txt": small})
	putFiles(t, s, "smallrepo", map[string]string{"s.txt": smallEdit})
	_, err = s.CreateCommit(context.Background(), CommitRequest{
		Repo: "smallrepo", Base: headS1, Message: "tiny", Changes: changesFor(map[string]string{"s.txt": smallEdit}),
	})
	require.NoError(t, err)
	rs, err := s.FetchTrees(context.Background(), FetchRequest{Repo: "smallrepo", HaveTree: treeS1, Format: "binz"})
	require.NoError(t, err)
	assert.Empty(t, rs.Deltas, "small payloads stay whole — delta is for the big-edit lane")
}
