package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
