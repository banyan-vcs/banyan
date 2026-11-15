package cli

// The multi-host config store (config.go): path resolution, key
// canonicalization, the put/drop default semantics, and the one-shot
// migration from the previous generation's credentials.json + known_hosts.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateConfig points the config at a fresh temp file (and the legacy
// store at a fresh temp dir) for one test.
func isolateConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("BANYAN_CONFIG", path)
	t.Setenv("BANYAN_HOME", t.TempDir())
	return path
}

func TestServerKey(t *testing.T) {
	assert.Equal(t, "http://banyan.corp:8080", serverKey("http://banyan.corp:8080/"))
	assert.Equal(t, "https://banyan.io", serverKey(" https://banyan.io "))
	assert.Equal(t, "banyan.corp:8022", serverKey("banyan.corp:8022")) // bare host stays as typed
}

func TestConfigPathResolution(t *testing.T) {
	old := configOverride
	defer func() { configOverride = old }()

	configOverride = `/tmp/overridden.yaml`
	assert.Equal(t, `/tmp/overridden.yaml`, configPath())

	configOverride = ""
	t.Setenv("BANYAN_CONFIG", "/tmp/env.yaml")
	assert.Equal(t, "/tmp/env.yaml", configPath())

	t.Setenv("BANYAN_CONFIG", "")
	t.Setenv("BANYAN_HOME", "/tmp/home")
	assert.Equal(t, filepath.Join("/tmp/home", "config.yaml"), configPath())

	t.Setenv("BANYAN_HOME", "")
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "conf", "banyan.yaml"), configPath())
}

func TestMultiHostRoundtrip(t *testing.T) {
	path := isolateConfig(t)

	putCred("http://a.corp/", credential{User: "alice", Token: "banyan_a", Method: "ssh", SSHAddr: "a.corp:22", Expires: 42})
	putCred("http://b.corp", credential{User: "bob", Token: "banyan_b", Method: "web", Expires: 42})

	// The first login became the default; the second must not steal it.
	cfg := loadConfig()
	require.Equal(t, "http://a.corp", cfg.DefaultHost)
	assert.Len(t, cfg.Hosts, 2)

	// Lookup canonicalizes the query URL to the config key.
	cred := credForHost("http://a.corp/some/path")
	require.NotNil(t, cred)
	assert.Equal(t, "alice", cred.User)
	assert.Equal(t, "a.corp:22", cred.SSHAddr)
	assert.Nil(t, credForHost("http://unknown.io"))

	// Dropping the default clears it; the other island survives.
	dropCred("http://a.corp")
	cfg = loadConfig()
	assert.Empty(t, cfg.DefaultHost)
	assert.Len(t, cfg.Hosts, 1)
	require.NotNil(t, cfg.Hosts["http://b.corp"])

	// Host pins ride the same file.
	pinHostKey("b.corp:22", "blob1")
	assert.Equal(t, "blob1", hostKeyOf("b.corp:22"))
	assert.Equal(t, "blob1", loadConfig().SSHHostKeys["b.corp:22"])
	require.NoFileExists(t, filepath.Join(filepath.Dir(path), "credentials.json"))
}

func TestLegacyMigration(t *testing.T) {
	isolateConfig(t)
	home := homeDir()
	require.NoError(t, os.MkdirAll(home, 0o700))
	raw, err := json.Marshal(map[string]any{
		"servers": map[string]any{
			"legacy.corp:8080": map[string]any{"user": "carol", "token": "banyan_old", "expires": 99, "method": "ssh", "ssh": "legacy.corp:22"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "credentials.json"), raw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "known_hosts.json"), []byte(`{"legacy.corp:22":"pin"}`), 0o600))

	cfg := loadConfig()
	e := cfg.Hosts["http://legacy.corp:8080"] // legacy keys had no scheme — http is the shape those stacks ran
	require.NotNil(t, e)
	assert.Equal(t, "carol", e.User)
	assert.Equal(t, "banyan_old", e.Token)
	assert.Equal(t, "legacy.corp:22", e.SSHAddr)
	assert.Equal(t, "pin", cfg.SSHHostKeys["legacy.corp:22"])
	assert.Equal(t, "http://legacy.corp:8080", cfg.DefaultHost, "the first migrated host becomes the default")
	assert.FileExists(t, configPath(), "migration persisted the config")
}

func TestServerArgFromDefaultFallback(t *testing.T) {
	isolateConfig(t)
	t.Setenv("BANYAN_SERVER", "")

	putCred("http://d.corp", credential{User: "u", Token: "t", Method: "web"})
	srv, err := serverArgFrom("/nonexistent-workspace", "")
	require.NoError(t, err)
	assert.Equal(t, "http://d.corp", srv)

	// The chain still prefers the explicit argument.
	srv, err = serverArgFrom("/nonexistent-workspace", "http://explicit.io/")
	require.NoError(t, err)
	assert.Equal(t, "http://explicit.io", srv)
}
