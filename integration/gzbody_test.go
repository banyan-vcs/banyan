// gzbody_test.go — T82: gzip rpc POST bodies (the flake's server half,
// docs/13): git compresses protocol v2 negotiation rounds — round 2
// re-sends the whole have list and arrives with Content-Encoding: gzip
// even under 1 KiB. Before the fix the body was read raw, the pkt-line
// parse hit binary noise, and the response came back empty ("expected
// 'acknowledgments'"). Deterministic raw-HTTP repro both ways: gzipped
// body must negotiate identically to the plain one.
package integration

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pktV2 builds one pkt-line string.
func pktV2(s string) string { return fmt.Sprintf("%04x", len(s)+4) + s }

func TestGzipFetchBody(t *testing.T) {
	needGit(t)
	s := newServer(t)
	seedRepo(t, s, "gz")
	head := strings.TrimSpace(git(t, "", "ls-remote", s.repoURL("gz"), "refs/heads/main"))
	tip := strings.Fields(head)[0]
	require.NotEmpty(t, tip)

	// A v2 fetch request: command, capabilities, delim, want + done —
	// the terminal round (no acknowledgments section, packfile directly).
	var req bytes.Buffer
	req.WriteString(pktV2("command=fetch"))
	req.WriteString(pktV2("agent=t82"))
	req.WriteString("0001") // delim
	req.WriteString(pktV2("want " + tip))
	req.WriteString(pktV2("done"))
	req.WriteString("0000") // flush ends the request

	fetch := func(gzipped bool) (int, string) {
		t.Helper()
		body := io.Reader(bytes.NewReader(req.Bytes()))
		if gzipped {
			var z bytes.Buffer
			zw := gzip.NewWriter(&z)
			_, _ = zw.Write(req.Bytes())
			zw.Close()
			body = &z
		}
		hreq, _ := http.NewRequest("POST", s.repoURL("gz")+"/git-upload-pack", body)
		hreq.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		hreq.Header.Set("Git-Protocol", "version=2")
		if gzipped {
			hreq.Header.Set("Content-Encoding", "gzip")
		}
		resp, err := http.DefaultClient.Do(hreq)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, string(raw)
	}

	codePlain, plain := fetch(false)
	require.Equal(t, http.StatusOK, codePlain)
	assert.Contains(t, plain, "packfile", "plain body must serve the packfile section")

	codeGz, gz := fetch(true)
	require.Equal(t, http.StatusOK, codeGz)
	assert.Contains(t, gz, "packfile", "gzip body must negotiate identically (the flake: empty response)")
	assert.Contains(t, gz, "PACK", "the pack bytes themselves arrive")
}
