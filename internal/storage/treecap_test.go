package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The decoded-tree cache is byte-bounded (docs/operations/performance.md §3b
// 发现⑤: the unbounded original retained every tree ever decoded — 79% of a
// 9GB heap at git.git scale sat in DecodeTree's entry tables). A tiny cap
// must cycle the FIFO, keep the accounting honest, and never break reads:
// an evicted tree re-reads through Raw with identical entries.
func TestTreeCacheByteBound(t *testing.T) {
	ctx := context.Background()
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(ctx, "r", model.SHA1))
	svc := NewObjectService(ms, NewLocalShardStore(t.TempDir()), []Node{{ID: "n1", Local: true}}, model.SHA1)
	svc.IndexTreesMaxBytes = 2 << 10 // 2KiB: a handful of trees

	firstOID := model.OID("")
	for i := 0; i < 40; i++ {
		tree := &model.Tree{Entries: []model.TreeEntry{{
			Name: fmt.Sprintf("file-%03d", i), Mode: "100644",
			Target: model.ObjectOID(model.BlobType, []byte(fmt.Sprintf("body %02d", i)), model.SHA1),
		}}}
		raw, err := tree.Encode()
		require.NoError(t, err)
		oid := model.ObjectOID(model.TreeType, raw, model.SHA1)
		svc.PutSynthetic("r", oid, model.TreeType, raw)
		if i == 0 {
			firstOID = oid
		}
		entries, terr := svc.TreeEntries("r", oid)
		require.NoError(t, terr, "tree %d reads", i)
		assert.Equal(t, fmt.Sprintf("file-%03d", i), entries[0].Name)
	}

	svc.idxMu.Lock()
	ri := svc.idx["r"]
	n := len(ri.trees)
	bytes := ri.treeBytes
	svc.idxMu.Unlock()
	assert.LessOrEqual(t, bytes, int64(4<<10), "tree bytes stay near the cap")
	assert.Less(t, n, 40, "the FIFO evicted early trees")
	assert.Greater(t, n, 0)

	// An evicted tree re-reads correctly through the authoritative path.
	entries, err := svc.TreeEntries("r", firstOID)
	require.NoError(t, err)
	assert.Equal(t, "file-000", entries[0].Name)
}
