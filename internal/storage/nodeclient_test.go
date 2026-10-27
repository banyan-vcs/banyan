package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

// The node client's timeout layering (docs/10 §3): fixed budgets for the
// phases without progress signal (dial, response headers), ctx alone for
// body streaming. A global client Timeout used to kill multi-GB pack
// transfers at the 5-minute mark mid-body; these tests pin the new contract.

func TestNodeClientHeaderBudgetIsBounded(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // headers never arrive in time
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	restore := nodeResponseHeaderWait
	nodeResponseHeaderWait = 80 * time.Millisecond
	t.Cleanup(func() { nodeResponseHeaderWait = restore })

	n := NewNodeClient(slow.URL)
	start := time.Now()
	_, err := n.GetPack(t.Context(), "r", "0123456789012345678901234567890123456789")
	require.Error(t, err, "stalled headers must fail at the transport budget")
	assert.Less(t, time.Since(start), 400*time.Millisecond,
		"the failure must arrive at the header budget, not the old 5-minute Timeout")
}

func TestNodeClientBodyFollowsContextOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // headers fine, body stalls forever
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	n := NewNodeClient(srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := n.GetPack(ctx, "r", "0123456789012345678901234567890123456789")
	require.Error(t, err, "a stalled body must fail when the caller's ctx gives up")
	assert.Less(t, time.Since(start), 2*time.Second,
		"body reads answer to the ctx, not to any whole-client timeout")
}

func TestNodeClientStillTransfersLongBodies(t *testing.T) {
	// A body longer than the OLD 5-minute budget would take too long to
	// prove here; the meaningful pin is that a slow-but-flowing body far
	// past the header budget streams to completion.
	payload := make([]byte, 4<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4194304")
		for i := 0; i < len(payload); i += 64 << 10 {
			end := i + 64<<10
			if end > len(payload) {
				end = len(payload)
			}
			w.Write(payload[i:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	n := NewNodeClient(srv.URL)
	got, err := n.GetPack(t.Context(), "r", model.OID("0123456789012345678901234567890123456789"))
	require.NoError(t, err)
	require.Len(t, got, len(payload),
		"a flowing body must stream to completion — progress, not elapsed time, is what counts")
}
