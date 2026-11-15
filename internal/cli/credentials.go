package cli

// Login credentials (docs/client/cli.md §4.5): SHORT-LIVED session tokens
// only — minted over SSH public-key auth or a web-console approval, never
// operator-created permanent PATs. The store is the multi-host config
// (config.go): every logged-in server plus the default host in ONE yaml.
import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
)

// credential is one server's session, handed to the transport (client.go).
type credential struct {
	User    string
	Token   string
	Expires int64  // unix seconds; the CLI re-authenticates past it
	Method  string // "ssh" | "web" — ssh refreshes silently
	SSHAddr string // advertised SSH address (the 401 re-mint path)
}

// homeDir is the legacy store directory (pre-config generations: ~/.banyan;
// BANYAN_HOME overrides — also where tests isolate).
func homeDir() string {
	if v := os.Getenv("BANYAN_HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".banyan")
	}
	return ".banyan"
}

// hostKeyOf returns the pinned SSH host key blob for addr ("" = unseen).
func hostKeyOf(addr string) string {
	return loadConfig().SSHHostKeys[addr]
}

func pinHostKey(addr, blobB64 string) {
	cfg := loadConfig()
	cfg.SSHHostKeys[addr] = blobB64
	saveConfig(cfg)
}

// credForHost loads the session for a server URL (matched by config key).
func credForHost(server string) *credential {
	cfg := loadConfig()
	if e, ok := cfg.Hosts[serverKey(server)]; ok && e != nil {
		return &credential{User: e.User, Token: e.Token, Expires: e.Expires, Method: e.Method, SSHAddr: e.SSHAddr}
	}
	return nil
}

// putCred stores a session; the FIRST host in the store becomes the default
// (gh parity — switching is explicit: `banyan host use`).
func putCred(server string, c credential) {
	cfg := loadConfig()
	key := serverKey(server)
	cfg.Hosts[key] = &hostEntry{User: c.User, Token: c.Token, Expires: c.Expires, Method: c.Method, SSHAddr: c.SSHAddr}
	if cfg.DefaultHost == "" {
		cfg.DefaultHost = key
	}
	saveConfig(cfg)
}

// dropCred removes a session; dropping the default clears the default (no
// silent re-pointing — the user picks the next default explicitly).
func dropCred(server string) {
	cfg := loadConfig()
	key := serverKey(server)
	if _, ok := cfg.Hosts[key]; !ok {
		return
	}
	delete(cfg.Hosts, key)
	if cfg.DefaultHost == key {
		cfg.DefaultHost = ""
	}
	saveConfig(cfg)
}

func basicHeaderValue(user, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
}

// serverArgFrom resolves the server URL: explicit arg, the workspace's
// server, BANYAN_SERVER, then the config's default host.
func serverArgFrom(wsRoot string, arg string) (string, error) {
	if arg != "" {
		return strings.TrimSuffix(arg, "/"), nil
	}
	if ws, err := loadWorkspace(wsRoot); err == nil {
		return ws.cfg.Server, nil
	}
	if s := os.Getenv("BANYAN_SERVER"); s != "" {
		return strings.TrimSuffix(s, "/"), nil
	}
	if cfg := loadConfig(); cfg.DefaultHost != "" {
		return cfg.DefaultHost, nil
	}
	return "", errNoServer
}

var errNoServer = &usageError{"login: pass the server URL (or run inside a workspace, set BANYAN_SERVER, or banyan host use a logged-in server)"}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }
