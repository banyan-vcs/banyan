package cli

// config.go — the CLI's multi-host configuration, gh-style (docs/client/cli.md
// §4.5): ONE yaml file holds every logged-in server plus the default host.
//
//	default_host: http://banyan.corp:8080
//	hosts:
//	  http://banyan.corp:8080:
//	    user: alice
//	    token: banyan_…            # a 12h session — never a permanent PAT
//	    expires: 1763000000
//	    method: ssh                # ssh sessions re-mint silently on 401
//	    ssh: banyan.corp:8022      # the advertised SSH address
//	  http://banyan.example.io:
//	    user: bob
//	    method: web
//	ssh_host_keys:                  # SSH trust-on-first-use pins
//	  banyan.corp:8022: c3NoLWVkMjU1MTk…
//
// Private deployments have no public login — every server is its own island,
// so the store is keyed per host (the docker-login model) and one of them is
// the default. Path resolution: --config flag > BANYAN_CONFIG > $BANYAN_HOME/
// config.yaml (when that override is set) > ~/conf/banyan.yaml. The previous
// generation kept credentials.json + known_hosts.json under ~/.banyan — those
// migrate in on first load (the legacy files stay behind; their tokens are
// 12h sessions that expire on their own).
import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// hostEntry is one server's view in the config.
type hostEntry struct {
	User    string `yaml:"user,omitempty"`
	Token   string `yaml:"token,omitempty"`
	Expires int64  `yaml:"expires,omitempty"`
	Method  string `yaml:"method,omitempty"` // "ssh" | "web" — ssh refreshes silently
	SSHAddr string `yaml:"ssh,omitempty"`    // advertised SSH address (re-mint)
	WebURL  string `yaml:"web,omitempty"`
}

type cliConfig struct {
	DefaultHost string                `yaml:"default_host,omitempty"`
	Hosts       map[string]*hostEntry `yaml:"hosts,omitempty"`
	SSHHostKeys map[string]string     `yaml:"ssh_host_keys,omitempty"`
}

// configOverride is the global --config flag (set once in Run, before dispatch).
var configOverride string

func configPath() string {
	if configOverride != "" {
		return configOverride
	}
	if v := os.Getenv("BANYAN_CONFIG"); v != "" {
		return v
	}
	if v := os.Getenv("BANYAN_HOME"); v != "" {
		return filepath.Join(v, "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "banyan.yaml"
	}
	return filepath.Join(home, "conf", "banyan.yaml")
}

// serverKey canonicalizes a server URL to its config key: scheme://host
// (the port rides along; trailing slashes and whitespace fall away).
func serverKey(server string) string {
	s := strings.TrimSuffix(strings.TrimSpace(server), "/")
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return s
}

func loadConfig() *cliConfig {
	cfg := &cliConfig{}
	if raw, err := os.ReadFile(configPath()); err == nil {
		_ = yaml.Unmarshal(raw, cfg)
	}
	if cfg.Hosts == nil {
		cfg.Hosts = map[string]*hostEntry{}
	}
	if cfg.SSHHostKeys == nil {
		cfg.SSHHostKeys = map[string]string{}
	}
	migrateLegacy(cfg)
	return cfg
}

// migrateLegacy folds the previous-generation store (credentials.json keyed
// by host:port + known_hosts.json, under ~/.banyan or BANYAN_HOME) into the
// config. Legacy keys carry no scheme — the http:// shape is what those
// deployments ran. The first migrated host becomes the default (first login
// semantics, same as putCred).
func migrateLegacy(cfg *cliConfig) {
	changed := false
	if len(cfg.Hosts) == 0 {
		if raw, err := os.ReadFile(filepath.Join(homeDir(), "credentials.json")); err == nil {
			var legacy struct {
				Servers map[string]struct {
					User    string `json:"user"`
					Token   string `json:"token"`
					Expires int64  `json:"expires"`
					Method  string `json:"method"`
					SSHAddr string `json:"ssh"`
				} `json:"servers"`
			}
			if json.Unmarshal(raw, &legacy) == nil {
				for host, c := range legacy.Servers {
					key := "http://" + host
					if _, exists := cfg.Hosts[key]; !exists {
						cfg.Hosts[key] = &hostEntry{User: c.User, Token: c.Token, Expires: c.Expires, Method: c.Method, SSHAddr: c.SSHAddr}
						changed = true
					}
				}
			}
		}
	}
	if len(cfg.SSHHostKeys) == 0 {
		if raw, err := os.ReadFile(filepath.Join(homeDir(), "known_hosts.json")); err == nil {
			var pins map[string]string
			if json.Unmarshal(raw, &pins) == nil {
				for addr, blob := range pins {
					if _, exists := cfg.SSHHostKeys[addr]; !exists {
						cfg.SSHHostKeys[addr] = blob
						changed = true
					}
				}
			}
		}
	}
	if changed {
		if len(cfg.Hosts) > 0 && cfg.DefaultHost == "" {
			cfg.DefaultHost = sortedHostKeys(cfg)[0]
		}
		saveConfig(cfg)
	}
}

// saveConfig persists atomically (temp file + same-dir rename): a killed
// CLI never leaves a half-written config behind. Concurrent CLI processes
// serialize on a best-effort lockfile — a stuck lock older than 5s is
// stolen, so a crashed writer never wedges every future one; the residual
// race (two writers, one lock timeout) degrades to last-writer-wins, which
// the SSH re-mint path self-heals.
func saveConfig(cfg *cliConfig) {
	path := configPath()
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0o700)
	lock := path + ".lock"
	for i := 0; i < 10; i++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			defer func() { _ = f.Close(); _ = os.Remove(lock) }()
			break
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > 5*time.Second {
			_ = os.Remove(lock) // stale: the previous holder is gone
			continue
		}
		time.Sleep(100 * time.Millisecond)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, werr := tmp.Write(raw); werr == nil {
		_ = tmp.Chmod(0o600)
		_ = tmp.Close()
		_ = os.Rename(name, path)
	} else {
		_ = tmp.Close()
	}
	_ = os.Remove(name) // no-op after a successful rename
}

// sortedHostKeys lists the configured servers in byte order (stable output
// for ls, status and the interactive menus).
func sortedHostKeys(cfg *cliConfig) []string {
	keys := make([]string, 0, len(cfg.Hosts))
	for k := range cfg.Hosts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
