package view

import (
	"fmt"
	"testing"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
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
