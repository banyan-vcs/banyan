package cli

// The multi-version compatibility layer (docs/client/cli.md §4.4):
// discovery caches per workspace, an old server (no /api/v1/proto)
// downgrades the fast paths by cache decision, and a stale cache
// self-heals on the first mismatch.
import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServerCapsDiscoverAndCache(t *testing.T) {
	var probes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/proto" {
			probes.Add(1)
			_, _ = w.Write([]byte(`{"proto":2,"features":["fetch-trees-bin2","objects-frame","objects-batch"]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	c := newClient(srv.URL)
	c.attachCaps(root)
	require.NotNil(t, c.caps)
	require.Equal(t, 2, c.caps.Proto)
	require.True(t, c.caps.has("objects-frame"))
	require.Equal(t, int64(1), probes.Load(), "discovery probed once")

	// The cache answers the next command without another probe.
	c2 := newClient(srv.URL)
	c2.attachCaps(root)
	require.NotNil(t, c2.caps)
	require.Equal(t, int64(1), probes.Load(), "the workspace cache answered — no re-probe")

	// A stale cache is ignored: backdate it past the TTL.
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".banyan"), 0o755))
	cp := loadCaps(root)
	cp.Checked -= int64(capsTTL.Seconds()) + 10
	raw := `{"proto":` + strconv.Itoa(cp.Proto) + `,"features":["x"],"checked":` + strconv.Itoa(int(cp.Checked)) + `}`
	require.NoError(t, os.WriteFile(filepath.Join(root, capsFile), []byte(raw), 0o644))
	c3 := newClient(srv.URL)
	c3.attachCaps(root)
	require.True(t, c3.caps.has("objects-frame"), "rediscovered after the TTL")
	require.Equal(t, int64(2), probes.Load())
}

func TestServerCapsOldServerDowngrades(t *testing.T) {
	// No /api/v1/proto at all: the pre-proto-2 server shape.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	c := newClient(srv.URL)
	c.attachCaps(root)
	require.Nil(t, c.caps, "404 = proto 1 = nil caps (try-and-fallback)")

	// has() on nil answers true for everything — the in-protocol
	// fallbacks carry an unknown server.
	require.True(t, c.caps.has("anything"))

	// A cached proto-1 answer pins the downgrade decisions.
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".banyan"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, capsFile),
		[]byte(`{"proto":1,"features":[],"checked":9223372036}`), 0o644))
	c2 := newClient(srv.URL)
	c2.attachCaps(root)
	require.NotNil(t, c2.caps)
	require.False(t, c2.caps.has("fetch-trees-bin2"), "a known proto-1 server never gets v2 hints")
	require.False(t, c2.caps.has("objects-frame"))

	// And the fetchTrees hint follows it.
	require.Equal(t, "bin", c2.fetchFormat())
}

func TestCapsStaleDropsCache(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".banyan"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, capsFile),
		[]byte(`{"proto":2,"features":["objects-frame"],"checked":9223372036}`), 0o644))
	c := newClient("http://unused")
	c.attachCaps(root)
	require.NotNil(t, c.caps)
	c.capsStale()
	require.Nil(t, c.caps)
	_, err := os.Stat(filepath.Join(root, capsFile))
	require.True(t, os.IsNotExist(err), "the cache file is gone")
}
