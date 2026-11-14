package gitwire

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The per-request fetch memory budget (fetch_mem_budget,
// docs/operations/performance.md §8.2) shrinks the serve ladder's
// thresholds in lockstep: a quarter for the materializing rungs (the
// closure-ladder cutoff), half for response-cache entries. The default
// keeps the historical constants; a negative budget turns tightening off.
func TestFetchMemBudgetThresholds(t *testing.T) {
	s := &Server{}
	require.Equal(t, int64(200<<20), s.fetchMemBudget(), "default budget")
	require.Equal(t, int64(32<<20), s.streamServeMin(), "default cutoff: min(32MiB, 200/4)")
	require.Equal(t, int64(100<<20), s.respEntryMax(), "default entry cap: min(128MiB, 200/2)")

	s.FetchMemBudget = 100 << 20 // the validated extreme
	require.Equal(t, int64(25<<20), s.streamServeMin(), "squeezed cutoff: min(32MiB, 100/4)")
	require.Equal(t, int64(50<<20), s.respEntryMax(), "squeezed entry cap: min(128MiB, 100/2)")

	s.FetchMemBudget = 512 << 20 // larger than the constants: they hold
	require.Equal(t, int64(32<<20), s.streamServeMin())
	require.Equal(t, int64(128<<20), s.respEntryMax())

	s.FetchMemBudget = -1 // off: fixed thresholds
	require.Equal(t, int64(streamServeMinPackBytes), s.streamServeMin())
	require.Equal(t, int64(128<<20), s.respEntryMax())
}
