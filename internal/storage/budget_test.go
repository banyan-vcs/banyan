package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// packMemoCap is the whole-pack memoize ceiling under the per-request
// fetch memory budget (docs/operations/performance.md §8.2): the same
// quarter-share the gitwire ladder uses, so a squeezed budget keeps the
// read ladder and the serve ladder inside one window per request.
func TestPackMemoCapUnderBudget(t *testing.T) {
	s := &ObjectService{}
	require.Equal(t, int64(32<<20), s.packMemoCap(), "default budget: fixed cap")

	s.FetchMemBudget = 100 << 20 // the validated extreme
	require.Equal(t, int64(25<<20), s.packMemoCap(), "min(32MiB, 100/4)")

	s.FetchMemBudget = 512 << 20 // quarter exceeds the cap: cap holds
	require.Equal(t, int64(32<<20), s.packMemoCap())

	s.FetchMemBudget = -1 // off: fixed cap
	require.Equal(t, int64(packMemoMaxBytes), s.packMemoCap())
}
