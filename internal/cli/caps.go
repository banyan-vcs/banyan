package cli

// The multi-version protocol compatibility layer (docs/client/cli.md §4.4,
// docs/operations/performance.md §9 多版本兼容): the server broadcasts its
// capabilities at /api/v1/proto, the workspace caches them, and every
// proto-2 fast path checks the cache before using itself — a newer CLI
// against an older server never pays version-trial round trips (and never
// degrades a big clone to JSON framing twice).
//
// The in-protocol guards stay as defense in depth: frames carry versioned
// magics, the fetch response's Content-Type tells the negotiated version,
// and the uplink's binary frames fall back to JSON on a 400. The cache
// only decides which door to knock on first.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const capsFile = ".banyan/server.json"

// capsTTL bounds how long a cached capability answer is trusted; servers
// upgrade, and a day is far below anyone's upgrade cadence.
const capsTTL = 24 * time.Hour

type serverCaps struct {
	Proto    int      `json:"proto"`
	Features []string `json:"features"`
	Checked  int64    `json:"checked"` // unix seconds
}

func (cp *serverCaps) has(feature string) bool {
	if cp == nil {
		return true // unknown server: try the fast path, in-protocol fallbacks catch it
	}
	for _, f := range cp.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// loadCaps reads the workspace's cached capabilities; nil when absent or
// stale (the caller rediscovers).
func loadCaps(root string) *serverCaps {
	raw, err := os.ReadFile(filepath.Join(root, capsFile))
	if err != nil {
		return nil
	}
	var cp serverCaps
	if json.Unmarshal(raw, &cp) != nil || time.Since(time.Unix(cp.Checked, 0)) > capsTTL {
		return nil
	}
	return &cp
}

func saveCaps(root string, cp *serverCaps) {
	if cp == nil {
		return
	}
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(capsFile)), 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(root, capsFile), raw, 0o644)
}

func dropCaps(root string) {
	_ = os.Remove(filepath.Join(root, capsFile))
}

// discoverCaps asks the server. A server without the endpoint (proto 1)
// answers 404 → nil, which callers treat as "try and fall back".
func (c *client) discoverCaps() *serverCaps {
	resp, err := c.do(http.MethodGet, "/api/v1/proto", nil, "")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var cp serverCaps
	if json.NewDecoder(resp.Body).Decode(&cp) != nil || cp.Proto <= 0 {
		return nil
	}
	cp.Checked = time.Now().Unix()
	return &cp
}

// attachCaps loads-or-discovers the workspace's capability cache and pins
// it (plus the cache's root) on the client. Called once per command that
// has a workspace; clone calls it against the freshly created directory.
func (c *client) attachCaps(root string) {
	c.capsRoot = root
	if cp := loadCaps(root); cp != nil {
		c.caps = cp
		return
	}
	if cp := c.discoverCaps(); cp != nil {
		c.caps = cp
		saveCaps(root, cp)
	}
}

// capsStale: a fast path the cache vouched for just failed in the
// version-mismatch way — drop the cache so the next command rediscovers.
func (c *client) capsStale() {
	if c.capsRoot != "" {
		dropCaps(c.capsRoot)
	}
	c.caps = nil
}
