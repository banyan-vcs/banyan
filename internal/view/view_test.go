package view

import (
	"fmt"
	"testing"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The headsCache FIFO bound (docs/operations/performance.md §3b 发现⑤
// hygiene): the old ≥16 whole-map reset threw away EVERY branch's shared
// walk once a repo crossed it — a multi-branch monorepo re-derived the
// full projection walk per branch per advertisement. The bound must evict
// ONE entry (insertion order), never the map; an evicted tip re-derives
// on demand.
func TestHeadsCacheFIFOBound(t *testing.T) {
	ms := meta.NewMemoryMetaStore()
	if err := ms.CreateRepo(t.Context(), "r", model.SHA1); err != nil {
		t.Fatal(err)
	}
	svc := storage.NewObjectService(ms, storage.NewLocalShardStore(t.TempDir()), []storage.Node{{ID: "n1", Local: true}}, model.SHA1)
	s := New(ms, svc, model.SHA1)

	// Distinct root-local tips via the synthetic table: HeadsFor only reads
	// them (ProjectionHeads over the lookup), so synthetic objects suffice —
	// a real one-file tree (the walk reads the head's root tree).
	blob := []byte("heads cache fixture")
	blobOID := model.ObjectOID(model.BlobType, blob, model.SHA1)
	tree := &model.Tree{Entries: []model.TreeEntry{{Name: "f", Mode: "100644", Target: blobOID}}}
	treeRaw, err := tree.Encode()
	if err != nil {
		t.Fatal(err)
	}
	treeOID := model.ObjectOID(model.TreeType, treeRaw, model.SHA1)
	tips := make([]model.OID, headsCacheMax+64)
	svc.BeginSynthBatch("r")
	svc.PutSynthetic("r", blobOID, model.BlobType, blob)
	svc.PutSynthetic("r", treeOID, model.TreeType, treeRaw)
	for i := range tips {
		when := time.Unix(int64(1700000000+i), 0).UTC()
		pc := &model.AnchoredCommit{
			TreeOID:   treeOID,
			Author:    model.Identity{Name: "v", Email: "v@x", When: when},
			Committer: model.Identity{Name: "v", Email: "v@x", When: when},
			Message:   fmt.Sprintf("tip %d\n", i),
		}
		tips[i] = pc.OID(model.SHA1)
		svc.PutSynthetic("r", tips[i], model.CommitType, pc.Encode())
	}
	svc.EndSynthBatch("r")

	for i, tip := range tips {
		if m := s.HeadsFor("r", "main", tip); m == nil {
			t.Fatalf("tip %d: nil heads", i)
		}
	}
	s.mu.Lock()
	n := len(s.headsCache)
	s.mu.Unlock()
	if n > headsCacheMax {
		t.Fatalf("headsCache holds %d entries, bound is %d", n, headsCacheMax)
	}

	// An evicted tip re-derives on demand (correctness never depends on the cache).
	if m := s.HeadsFor("r", "main", tips[0]); m == nil {
		t.Fatal("evicted tip re-derives")
	}
}

// The /.woven/proj snapshot (发现⑤ ①): HeadsFor persists its derivation as
// timeline rows so a COLD process answers the advertisement from meta reads
// instead of re-walking the history. Complete coverage answers; a missing
// dir, a stale base, or a ResetWoven sweep answers nil (derive).
func TestProjectionSnapshotRoundTrip(t *testing.T) {
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(t.Context(), "r", model.SHA1))
	svc := storage.NewObjectService(ms, storage.NewLocalShardStore(t.TempDir()), []storage.Node{{ID: "n1", Local: true}}, model.SHA1)
	s := New(ms, svc, model.SHA1)

	// Two commits where lib/ changes: the /lib projection has a chain.
	blob1 := model.ObjectOID(model.BlobType, []byte("lib v1"), model.SHA1)
	blob2 := model.ObjectOID(model.BlobType, []byte("lib v2"), model.SHA1)
	lib1 := &model.Tree{Entries: []model.TreeEntry{{Name: "a.txt", Mode: "100644", Target: blob1}}}
	lib2 := &model.Tree{Entries: []model.TreeEntry{{Name: "a.txt", Mode: "100644", Target: blob2}}}
	libRaw1, _ := lib1.Encode()
	libRaw2, _ := lib2.Encode()
	libOID1 := model.ObjectOID(model.TreeType, libRaw1, model.SHA1)
	libOID2 := model.ObjectOID(model.TreeType, libRaw2, model.SHA1)
	root1 := &model.Tree{Entries: []model.TreeEntry{{Name: "lib", Mode: "40000", Target: libOID1, IsDir: true}, {Name: "README", Mode: "100644", Target: blob1}}}
	root2 := &model.Tree{Entries: []model.TreeEntry{{Name: "lib", Mode: "40000", Target: libOID2, IsDir: true}, {Name: "README", Mode: "100644", Target: blob1}}}
	rootRaw1, _ := root1.Encode()
	rootRaw2, _ := root2.Encode()
	rootOID1 := model.ObjectOID(model.TreeType, rootRaw1, model.SHA1)
	rootOID2 := model.ObjectOID(model.TreeType, rootRaw2, model.SHA1)
	when := time.Unix(1760000000, 0).UTC()
	pc1 := &model.AnchoredCommit{TreeOID: rootOID1,
		Author: model.Identity{Name: "v", Email: "v@x", When: when}, Committer: model.Identity{Name: "v", Email: "v@x", When: when}, Message: "one\n"}
	pc2 := &model.AnchoredCommit{TreeOID: rootOID2, Parents: []model.OID{pc1.OID(model.SHA1)},
		Author: model.Identity{Name: "v", Email: "v@x", When: when}, Committer: model.Identity{Name: "v", Email: "v@x", When: when}, Message: "two\n"}
	c1 := pc1.OID(model.SHA1)
	c2 := pc2.OID(model.SHA1)
	svc.BeginSynthBatch("r")
	svc.PutSynthetic("r", blob1, model.BlobType, []byte("lib v1"))
	svc.PutSynthetic("r", blob2, model.BlobType, []byte("lib v2"))
	svc.PutSynthetic("r", libOID1, model.TreeType, libRaw1)
	svc.PutSynthetic("r", libOID2, model.TreeType, libRaw2)
	svc.PutSynthetic("r", rootOID1, model.TreeType, rootRaw1)
	svc.PutSynthetic("r", rootOID2, model.TreeType, rootRaw2)
	svc.PutSynthetic("r", c1, model.CommitType, pc1.Encode())
	svc.PutSynthetic("r", c2, model.CommitType, pc2.Encode())
	svc.EndSynthBatch("r")

	heads := s.HeadsFor("r", "main", c2)
	require.NotEmpty(t, heads, "the lib projection derives a head")
	snap := s.SnapshotHeads(t.Context(), "r", "main", c2, []string{"lib"})
	assert.Equal(t, heads["lib"], snap["lib"], "the cold-process read answers the same head")

	assert.Nil(t, s.SnapshotHeads(t.Context(), "r", "main", c2, []string{"lib", "docs"}), "a dir without a row means derive")
	assert.Nil(t, s.SnapshotHeads(t.Context(), "r", "main", c1, []string{"lib"}), "a stale base means derive")

	require.NoError(t, s.ResetWoven(t.Context(), "r", "main"))
	assert.Nil(t, s.SnapshotHeads(t.Context(), "r", "main", c2, []string{"lib"}), "ResetWoven sweeps the snapshot with the family")
}
