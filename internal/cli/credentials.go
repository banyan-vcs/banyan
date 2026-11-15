package cli

// Login credentials (docs/client/cli.md §4.5): SHORT-LIVED session tokens
// only — minted over SSH public-key auth or a web-console approval, never
// operator-created permanent PATs. Store shape: ~/.banyan/credentials.json
// (0600) keyed by server host, plus ~/.banyan/known_hosts for SSH
// trust-on-first-use. BANYAN_HOME overrides the directory (tests).
import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type credential struct {
	User    string `json:"user"`
	Token   string `json:"token"`
	Expires int64  `json:"expires"` // unix seconds; the CLI re-authenticates past it
	Method  string `json:"method"`  // "ssh" | "web" — ssh refreshes silently
	SSHAddr string `json:"ssh,omitempty"`
}

type credStore struct {
	Servers map[string]credential `json:"servers"`
}

func homeDir() string {
	if v := os.Getenv("BANYAN_HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".banyan")
	}
	return ".banyan"
}

func loadCreds() credStore {
	var s credStore
	raw, err := os.ReadFile(filepath.Join(homeDir(), "credentials.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	if s.Servers == nil {
		s.Servers = map[string]credential{}
	}
	return s
}

func saveCreds(s credStore) {
	if err := os.MkdirAll(homeDir(), 0o700); err != nil {
		return
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(homeDir(), "credentials.json"), raw, 0o600)
}

// hostKeyOf returns the pinned SSH host key blob for addr ("" = unseen).
func hostKeyOf(addr string) string {
	raw, err := os.ReadFile(filepath.Join(homeDir(), "known_hosts.json"))
	if err != nil {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		return m[addr]
	}
	return ""
}

func pinHostKey(addr, blobB64 string) {
	m := map[string]string{}
	if raw, err := os.ReadFile(filepath.Join(homeDir(), "known_hosts.json")); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	m[addr] = blobB64
	if out, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.MkdirAll(homeDir(), 0o700)
		_ = os.WriteFile(filepath.Join(homeDir(), "known_hosts.json"), out, 0o600)
	}
}

// credForHost loads the credential for a server URL (matched by host).
func credForHost(server string) *credential {
	u, err := url.Parse(server)
	if err != nil {
		return nil
	}
	s := loadCreds()
	if c, ok := s.Servers[u.Host]; ok {
		return &c
	}
	return nil
}

func putCred(server string, c credential) {
	u, err := url.Parse(server)
	if err != nil {
		return
	}
	s := loadCreds()
	s.Servers[u.Host] = c
	saveCreds(s)
}

func dropCred(server string) {
	u, err := url.Parse(server)
	if err != nil {
		return
	}
	s := loadCreds()
	delete(s.Servers, u.Host)
	saveCreds(s)
}

func basicHeaderValue(user, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
}

// serverArgFrom resolves the server URL for login/logout: explicit arg,
// the workspace's server, or BANYAN_SERVER.
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
	return "", errNoServer
}

var errNoServer = &usageError{"login: pass the server URL (or run inside a workspace, or set BANYAN_SERVER)"}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }
