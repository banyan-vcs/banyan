package storage

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
)

// buildTestPack builds a small root repo via the model layer and encodes it as a pack.
func buildTestPack(t *testing.T) (blobOID, treeOID, commitOID model.OID, pack []byte) {
	t.Helper()
	algo := model.SHA1
	who := model.Identity{Name: "t", Email: "t@banyan.dev", When: time.Unix(1760000000, 0).UTC()}

	blobOID = model.ObjectOID(model.BlobType, []byte("hello index\n"), algo)
	treeRaw, err := (&model.Tree{Entries: []model.TreeEntry{
		{Name: "a.txt", Mode: "100644", Target: blobOID},
	}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	treeOID = model.ObjectOID(model.TreeType, treeRaw, algo)

	commit := &model.AnchoredCommit{
		Anchor:    model.RootAnchor,
		TreeOID:   treeOID,
		Author:    who,
		Committer: who,
		Message:   "index test\n",
	}
	commitOID = commit.OID(algo)

	var buf bytes.Buffer
	if err := EncodePack(&buf,
		map[model.OID][]byte{blobOID: []byte("hello index\n")},
		map[model.OID][]byte{treeOID: treeRaw},
		map[model.OID][]byte{commitOID: commit.Encode()},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	return blobOID, treeOID, commitOID, buf.Bytes()
}

func TestIngestPackBuildsIndexesAndWritesIdx(t *testing.T) {
	blobOID, treeOID, commitOID, pack := buildTestPack(t)

	meta := NewMemoryMetaStore()
	local := NewLocalShardStore(t.TempDir())
	objs := NewObjectService(meta, local, []Node{{ID: "n1", Local: true}}, model.SHA1)
	ctx := context.Background()
	if err := meta.CreateRepo(ctx, "r", model.SHA1); err != nil {
		t.Fatal(err)
	}

	packOID, err := objs.IngestPack(ctx, "r", pack)
	if err != nil {
		t.Fatal(err)
	}

	// offsets pair up fully + the idx file is on disk and matches entry by entry.
	dp, err := DecodePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	if len(dp.Offsets) != len(dp.Order) {
		t.Fatalf("offsets not paired: %d vs %d", len(dp.Offsets), len(dp.Order))
	}
	idxRaw, err := os.ReadFile(local.idxPath("r", packOID))
	if err != nil {
		t.Fatalf("idx file missing: %v", err)
	}
	pi, err := DecodeIdx(idxRaw, model.SHA1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pi.Entries) != 3 {
		t.Fatalf("idx entries = %d, want 3", len(pi.Entries))
	}
	for oid, off := range dp.Offsets {
		if pi.Entries[oid] != off {
			t.Fatalf("idx offset mismatch for %s: %d != %d", oid, pi.Entries[oid], off)
		}
	}

	// commit-graph index.
	ci, err := objs.CommitInfo("r", commitOID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.Tree != treeOID || len(ci.Parents) != 0 || ci.When != 1760000000 {
		t.Fatalf("CommitInfo wrong: %+v", ci)
	}

	// tree-entry index.
	entries, err := objs.TreeEntries("r", treeOID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Target != blobOID {
		t.Fatalf("TreeEntries wrong: %+v", entries)
	}

	// pack file read-back (miss path) matches the original bytes.
	raw, err := local.GetPackBytes("r", packOID)
	if err != nil || !bytes.Equal(raw, pack) {
		t.Fatalf("raw pack roundtrip failed: %v", err)
	}
}

// TestGenerationDiamond builds A → (B, C) → D plus a dangling-parent E and
// verifies git commit-graph generation semantics: exact generations on a
// resolvable DAG, taint (exact=false) when an ancestor is missing.
func TestGenerationDiamond(t *testing.T) {
	algo := model.SHA1
	who := model.Identity{Name: "t", Email: "t@banyan.dev", When: time.Unix(1760000000, 0).UTC()}
	meta := NewMemoryMetaStore()
	local := NewLocalShardStore(t.TempDir())
	svc := NewObjectService(meta, local, []Node{{ID: "n", Local: true}}, algo)
	ctx := context.Background()
	if err := meta.CreateRepo(ctx, "gen", algo); err != nil {
		t.Fatal(err)
	}

	blobs := map[model.OID][]byte{}
	trees := map[model.OID][]byte{}
	commits := map[model.OID][]byte{}
	mkTree := func(name string, target model.OID) model.OID {
		raw, err := (&model.Tree{Entries: []model.TreeEntry{{Name: name, Mode: "100644", Target: target}}}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		oid := model.ObjectOID(model.TreeType, raw, algo)
		trees[oid] = raw
		return oid
	}
	mkBlob := func(content string) model.OID {
		oid := model.ObjectOID(model.BlobType, []byte(content), algo)
		blobs[oid] = []byte(content)
		return oid
	}
	mkCommit := func(tree model.OID, parents ...model.OID) model.OID {
		c := &model.AnchoredCommit{Anchor: model.RootAnchor, TreeOID: tree, Parents: parents, Author: who, Committer: who, Message: "gen\n"}
		oid := c.OID(algo)
		commits[oid] = c.Encode()
		return oid
	}

	ta, tb, tc, td := mkTree("a", mkBlob("a")), mkTree("b", mkBlob("b")), mkTree("c", mkBlob("c")), mkTree("d", mkBlob("d"))
	a := mkCommit(ta)
	b := mkCommit(tb, a)
	c := mkCommit(tc, a)
	d := mkCommit(td, b, c) // diamond: gen = 1 + max(2, 2) = 3
	missing := model.ObjectOID(model.BlobType, []byte("not ingested"), algo)
	e := mkCommit(td, missing) // parent never ingested: tainted lower bound

	var buf bytes.Buffer
	if err := EncodePack(&buf, blobs, trees, commits, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IngestPack(ctx, "gen", buf.Bytes()); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		oid   model.OID
		gen   uint32
		exact bool
	}{
		{a, 1, true},
		{b, 2, true},
		{c, 2, true},
		{d, 3, true},
		{e, 1, false},
	} {
		g, exact := svc.Generation("gen", tc.oid)
		require.Equal(t, tc.gen, g, "generation of %s", tc.oid)
		require.Equal(t, tc.exact, exact, "exactness of %s", tc.oid)
	}
	// Reverse tree index: td is recorded by D and E; ta only by A.
	require.ElementsMatch(t, []model.OID{d, e}, svc.CommitsWithTree("gen", td))
	require.Equal(t, []model.OID{a}, svc.CommitsWithTree("gen", ta))
	require.Empty(t, svc.CommitsWithTree("gen", model.OID("0000000000000000000000000000000000000000")))
}
