package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/cliproto"
)

// The multi-version ladder on the uplink (docs/operations/performance.md
// §9 A2): a binary-frame upload against an OLD server (400 on the unknown
// Content-Type — what a pre-proto-2 banyand answers when its JSON decode
// fails) retries the same batch as JSON exactly once and succeeds.
func TestPutObjectsFallsBackToJSON(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			// The old server: only knows the JSON body.
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "INVALID_BODY", "message": "bad body"}})
			return
		}
		if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		var req struct {
			Repo    string `json:"repo"`
			Objects []struct {
				OID  string `json:"oid"`
				Type string `json:"type"`
				Data []byte `json:"data"`
			} `json:"objects"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Repo != "r1" || len(req.Objects) != 1 || req.Objects[0].Type != "blob" || string(req.Objects[0].Data) != "fallback payload" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL)
	err := c.putObjects("r1", []cliproto.ObjectRecord{{
		OID: strings.Repeat("0", 40), Type: "blob", Data: []byte("fallback payload"),
	}})
	require.NoError(t, err)
	require.Equal(t, int64(2), calls.Load(), "binary attempt + JSON retry")
}
