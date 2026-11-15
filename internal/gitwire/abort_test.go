// abort_test.go: the mid-stream abort plumbing (newAbortBufWriter /
// abortStream). A serve path that breaks mid-copy must kill the HTTP
// connection — never end the response cleanly short — while non-HTTP sinks
// (SSH sessions, gitaly stream frontends) keep their plain end-of-session
// behavior: an ErrAbortHandler panic there would take the process.
package gitwire

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAbortStreamHTTPSink(t *testing.T) {
	// The v1 chain: the closure ladder receives the bufWriter directly.
	out := newAbortBufWriter(httptest.NewRecorder())
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("bufWriter chain: expected http.ErrAbortHandler panic")
		}
	}()
	abortStream(out)
	t.Fatal("bufWriter chain: expected panic")
}

func TestAbortStreamSidebandChain(t *testing.T) {
	// The v2 chain: the packfile section wraps the bufWriter in
	// sidebandWriter — the abort must be discovered through the wrapper.
	out := newAbortBufWriter(httptest.NewRecorder())
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("sideband chain: expected http.ErrAbortHandler panic")
		}
	}()
	abortStream(sidebandWriter{w: out})
	t.Fatal("sideband chain: expected panic")
}

func TestAbortStreamNonHTTPSinkNoop(t *testing.T) {
	// Plain bufWriters (SSH channel, gitaly stream frontends) carry no
	// abort: the walk is a no-op and the caller ends the stream its own way.
	out := newBufWriter(io.Discard)
	abortStream(out)
	abortStream(sidebandWriter{w: out})
}
