// gzip request bodies (http.go's POST surfaces): git compresses large rpc
// POSTs — protocol v2's second negotiation round re-sends the whole have
// list and routinely crosses the client's gzip threshold even under 1 KiB
// (remote-curl sets Content-Encoding: gzip when the server advertised the
// gzip capability... and also opportunistically on repeats). Reading the
// body raw turns the stream into pkt-line garbage: the first "packet" is
// binary noise, the command parse ends the session, and the client dies on
// an empty response ("expected 'acknowledgments'").
package gitwire

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
)

// decodeBody unwraps a gzip Content-Encoding; every other body passes
// through unchanged. The gzip reader stays un-closed on error paths (the
// underlying body's lifetime belongs to the server).
func decodeBody(r *http.Request) io.Reader {
	if r.Header.Get("Content-Encoding") != "gzip" {
		return r.Body
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		// A broken gzip stream will fail the pkt-line parse anyway; hand
		// back a reader that surfaces the error immediately.
		return errReader{err}
	}
	return zr
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// bodyReader wraps one request body as a buffered pkt-line source.
func bodyReader(r *http.Request, size int) *bufio.Reader {
	return bufio.NewReaderSize(decodeBody(r), size)
}
