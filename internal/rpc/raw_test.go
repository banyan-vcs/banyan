package rpc

// The raw face (GET /repos/<repo>/raw/<path>): a blob's bytes with a
// display content-type from the path — the README image contract. nosniff
// rides every answer, SVG additionally carries the script-neutering CSP
// (a directly-navigated hostile SVG must not run in the web origin),
// directories refuse, unknown extensions download, and the authn gate
// holds.
import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

func TestRaw(t *testing.T) {
	s, ms, hs := newAuthStack(t)
	_ = s
	ctx := context.Background()
	require.NoError(t, ms.CreateRepo(ctx, "pics", model.SHA1))

	png, svg, readme := []byte("PNGDATA"), []byte("<svg/>"), []byte("# hi\n")
	put := func(p string, data []byte) service.Change {
		t.Helper()
		oid := model.ObjectOID(model.BlobType, data, model.SHA1)
		require.NoError(t, s.Svc.PutObjects(ctx, "pics", []service.PutObject{{OID: oid, Type: "blob", Data: data}}))
		return service.Change{Path: p, Blob: oid}
	}
	// The demoseed recipe: first commit on the default branch takes base "".
	_, err := s.Svc.CreateCommit(ctx, service.CommitRequest{
		Repo: "pics", Branch: "main", Message: "seed", Author: &service.Identity{Name: "alice", Email: "a@b"},
		Changes: []service.Change{
			put("assets/logo.png", png),
			put("assets/logo.svg", svg),
			put("README.md", readme),
		},
	})
	require.NoError(t, err)

	get := func(path string) *http.Response {
		t.Helper()
		return apiAs(t, hs.URL, http.MethodGet, "/api/v1/repos/pics/raw/"+path, "alice", nil)
	}

	// --- the image contract: bytes + content-type + nosniff ---
	resp := get("assets/logo.png")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, png, body)
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))

	// --- SVG: content-type + the CSP that neuters scripts on navigation ---
	resp = get("assets/logo.svg")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/svg+xml", resp.Header.Get("Content-Type"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'none'")

	// --- unknown extensions download; the ref pin answers the same ---
	resp = get("README.md")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	resp = get("assets/logo.png?rev=main")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// --- refusals: missing file 404, directory 400, non-GET 405 ---
	resp = get("assets/missing.png")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = get("assets")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp = apiAs(t, hs.URL, http.MethodPost, "/api/v1/repos/pics/raw/assets/logo.png", "alice", map[string]string{})
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	// --- the authn gate holds for the raw face too ---
	bare, err := http.Get(hs.URL + "/api/v1/repos/pics/raw/assets/logo.png")
	require.NoError(t, err)
	bare.Body.Close()
	require.Equal(t, http.StatusUnauthorized, bare.StatusCode)
}
