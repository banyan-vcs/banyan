package storage

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

// The geometric split must mirror builtin/repack.c: absorb while the next
// pack is smaller than factor × the group's cumulative count; a fitting
// pack closes the group. Large packs never enter a merge group.
func TestGeometricGroups(t *testing.T) {
	cases := []struct {
		name   string
		counts []int
		want   [][2]int
	}{
		{"already-geometric", []int{1, 3, 10, 40}, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}}},
		{"two-small-merge", []int{1, 1, 100, 100}, [][2]int{{0, 2}, {2, 4}}},
		{"all-equal-merge-all", []int{5, 5, 5, 5}, [][2]int{{0, 4}}},
		{"new-fragment-below-peak", []int{1, 2, 4, 8, 16, 1}, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 6}}},
		{"single", []int{7}, [][2]int{{0, 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, geometricGroups(tc.counts, 2))
		})
	}
}

// "new-fragment-below-peak" deserves the walk: sorted [1,2,4,8,16,1] is
// [1,1,2,4,8,16]; the two 1s merge (1 < 2×1), then 2 ≥ 2×2 fits, and each
// subsequent pack stands alone — only the tiny pair consolidates.
func TestGeometricNewFragmentWalk(t *testing.T) {
	assert.Equal(t, [][2]int{{0, 2}, {2, 3}, {3, 4}, {4, 5}, {5, 6}},
		geometricGroups([]int{1, 1, 2, 4, 8, 16}, 2))
}

// An unreadable fragment must survive a repack round: its objects are not
// covered by the merged pack, so dropping its registration would delete the
// only rows locating them — permanent loss for objects nothing else holds.
// The fix: skipped fragments never enter the drop loop (the next round, or
// a returning holder, retries). Regression shape: two fragments, one made
// unreadable (all holders fail), Repack runs, the unreadable pack stays
// registered while the readable one merges away.
func TestRepackUnreadableFragmentSurvives(t *testing.T) {
	ctx := context.Background()
	meta := NewMemoryMetaStore()
	local := NewLocalShardStore(t.TempDir())
	objects := NewObjectServiceWithBackend(meta, &LocalBackend{Local: local, NodeID: "n1"}, model.SHA1)
	require.NoError(t, meta.CreateRepo(ctx, "rr", model.SHA1))

	mk := func(blob string) model.OID {
		bodies := map[model.OID][]byte{}
		bodies[model.ObjectOID(model.BlobType, []byte(blob), model.SHA1)] = []byte(blob)
		var buf bytes.Buffer
		require.NoError(t, EncodePack(&buf, bodies, nil, nil, nil))
		oid, err := objects.IngestPack(ctx, "rr", buf.Bytes())
		require.NoError(t, err)
		return oid
	}
	p1 := mk("alpha") // 1-object fragment
	p2 := mk("beta")
	_ = p2

	packsBefore, err := meta.ListPacks(ctx, "rr")
	require.NoError(t, err)
	require.Len(t, packsBefore, 2)

	// Make p1 unreadable the honest way a shard goes dark: its bytes vanish
	// from the backend while the registry still lists it.
	require.NoError(t, os.RemoveAll(local.packPath("rr", p1)))

	// Two fragments of 1 object each merge geometrically; threshold=2.
	_, err = objects.Repack(ctx, "rr", 2, 0)
	require.NoError(t, err)

	packsAfter, err := meta.ListPacks(ctx, "rr")
	require.NoError(t, err)
	oids := map[model.OID]bool{}
	for _, p := range packsAfter {
		oids[p.PackOID] = true
	}
	assert.True(t, oids[p1], "the unreadable fragment must keep its registration")
	assert.Len(t, packsAfter, 2, "nothing merged away: the merged pack replaced nothing")

	// alpha's locating row still points somewhere registered — the loss
	// window is closed even though the bytes are (for now) unreachable.
	alpha := model.ObjectOID(model.BlobType, []byte("alpha"), model.SHA1)
	locs, err := meta.LocateObjects(ctx, "rr", []model.OID{alpha})
	require.NoError(t, err)
	assert.NotEmpty(t, locs[alpha].PackOID, "alpha keeps a locating row")
}
