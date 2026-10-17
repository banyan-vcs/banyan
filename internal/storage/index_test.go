package storage

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
)

// buildTestPack 用模型层构造一个小型根仓库并编码为 pack。
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

	// offset 配对齐全 + idx 文件落盘且逐条一致。
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

	// commit-graph 索引。
	ci, err := objs.CommitInfo("r", commitOID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.Tree != treeOID || len(ci.Parents) != 0 || ci.When != 1760000000 {
		t.Fatalf("CommitInfo wrong: %+v", ci)
	}

	// tree-entry 索引。
	entries, err := objs.TreeEntries("r", treeOID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Target != blobOID {
		t.Fatalf("TreeEntries wrong: %+v", entries)
	}

	// pack 文件回读（miss 路径）与原始字节一致。
	raw, err := local.GetPackBytes("r", packOID)
	if err != nil || !bytes.Equal(raw, pack) {
		t.Fatalf("raw pack roundtrip failed: %v", err)
	}
}
