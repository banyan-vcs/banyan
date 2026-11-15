// proxy_test.go: the streaming proxy's failure honesty — an upstream body
// that dies mid-stream must reach the client as a hard transport error,
// never as a well-formed short response (the laundering shape behind git's
// "N bytes of body are still expected", pitfall 79).
package console

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProxyAbortsOnUpstreamBreak: the gateway flushes part of a pack and
// then dies (a handler panic makes net/http close the connection without a
// terminator). The proxy must kill the client connection — a cleanly ended
// short body would hand git a truncated pack inside a valid response.
func TestProxyAbortsOnUpstreamBreak(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		// Past the proxy's 64 KiB pump buffer, so the break is mid-body
		// for sure, with bytes already relayed.
		_, _ = w.Write(bytes.Repeat([]byte{0x1f}, 96<<10))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic("simulated gateway death mid-pack")
	}))
	defer gw.Close()

	s := &Server{Gateway: gw.URL, Secret: []byte("t-secret")}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.proxyToGateway(w, r, "/r/p_test01.git/git-upload-pack", "", "alice")
	}))
	defer front.Close()

	resp, err := http.Get(front.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, rerr := io.Copy(io.Discard, resp.Body)
	require.Error(t, rerr, "a truncated upstream must surface as a transport error, not a clean short body")
}

// TestProxyRelaysCompleteBody: the healthy twin — a complete upstream body
// relays whole; the abort path must not fire on ordinary EOF.
func TestProxyRelaysCompleteBody(t *testing.T) {
	body := bytes.Repeat([]byte{0x2e}, 200<<10) // several pump buffers
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer gw.Close()

	s := &Server{Gateway: gw.URL, Secret: []byte("t-secret")}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.proxyToGateway(w, r, "/r/p_test01.git/git-upload-pack", "", "alice")
	}))
	defer front.Close()

	resp, err := http.Get(front.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Len(t, got, len(body))
}
