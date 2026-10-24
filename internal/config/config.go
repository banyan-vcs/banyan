// Package config resolves banyand's runtime configuration with one rule:
// defaults < YAML config file < explicitly-set command-line flags.
// Strict YAML decoding (unknown keys are errors) plus cross-field validation
// keep the effective configuration consistent and observable.
//
// Roles (docs/11 §2): all-in-one (default) runs everything in one process;
// cluster deployments split into gateway / refs / storage processes.
package config

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the effective banyand configuration.
type Config struct {
	Listen      string `yaml:"listen"`    // HTTP listen address (external for gateway/all-in-one, internal for refs/storage)
	Root        string `yaml:"root"`      // local shard storage root
	InitRepo    string `yaml:"init_repo"` // repo created at startup (optional)
	MetaBackend string `yaml:"meta"`      // "mem" | "sqlite"
	DBPath      string `yaml:"db"`        // sqlite path; defaults to <root>/banyan.db

	Role          string `yaml:"role"`             // all-in-one (default) | gateway | refs | storage
	NodeID        string `yaml:"node_id"`          // storage role: cluster identity
	Advertise     string `yaml:"advertise"`        // storage role: address other roles dial (host:port)
	RefsAddr      string `yaml:"refs_addr"`        // gateway/storage: refs service address
	DefaultR      int    `yaml:"default_replicas"` // cluster default replica target
	WriteAcks     string `yaml:"write_acks"`       // quorum (default) | all | one
	SuspectSec    int    `yaml:"health_suspect_sec"`
	DeadSec       int    `yaml:"health_dead_sec"`
	ReconSec      int    `yaml:"recon_interval_sec"`
	ReconCurrency int    `yaml:"recon_concurrency"`
	HeartbeatSec  int    `yaml:"heartbeat_sec"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Listen:        ":8080",
		Root:          "./data",
		MetaBackend:   "mem",
		Role:          "all-in-one",
		DefaultR:      3,
		WriteAcks:     "quorum",
		SuspectSec:    30,
		DeadSec:       600,
		ReconSec:      15,
		ReconCurrency: 4,
		HeartbeatSec:  3,
	}
}

// Load parses args, applies the -config YAML file when given, and lets
// explicitly-set flags win over the file (unset flags keep the file value,
// which itself overrides the defaults). The result is validated and
// normalized (meta default, db path resolution).
func Load(args []string) (*Config, error) {
	defaults := Default()
	fs := flag.NewFlagSet("banyand", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	configPath := fs.String("config", "", "YAML config file (flag values override file values)")
	listen := fs.String("listen", defaults.Listen, "HTTP listen address")
	root := fs.String("root", defaults.Root, "local shard storage root")
	initRepo := fs.String("init-repo", defaults.InitRepo, "repo name to create at startup (optional)")
	metaBackend := fs.String("meta", defaults.MetaBackend, `metadata backend: "mem" (in-memory) or "sqlite" (persistent)`)
	dbPath := fs.String("db", defaults.DBPath, "sqlite database path for -meta sqlite (default <root>/banyan.db)")
	role := fs.String("role", defaults.Role, `deployment role: all-in-one (default) | gateway | refs | storage (docs/11)`)
	nodeID := fs.String("node-id", defaults.NodeID, "storage role: cluster node identity")
	advertise := fs.String("advertise", defaults.Advertise, "storage role: address other roles dial (host:port; default 127.0.0.1:<listen port>)")
	refsAddr := fs.String("refs-addr", defaults.RefsAddr, "refs service address (gateway/storage roles)")
	defaultR := fs.Int("default-replicas", defaults.DefaultR, "cluster default replica target per repo")
	writeAcks := fs.String("write-acks", defaults.WriteAcks, `write acknowledgements: quorum (default) | all | one`)
	suspect := fs.Int("health-suspect-sec", defaults.SuspectSec, "node suspect threshold in seconds (docs/11 §8.1)")
	dead := fs.Int("health-dead-sec", defaults.DeadSec, "node dead threshold in seconds")
	reconSec := fs.Int("recon-interval-sec", defaults.ReconSec, "reconciler interval in seconds")
	reconCur := fs.Int("recon-concurrency", defaults.ReconCurrency, "reconciler parallel pack copies")
	heartbeat := fs.Int("heartbeat-sec", defaults.HeartbeatSec, "storage node heartbeat interval in seconds")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	cfg := defaults
	if *configPath != "" {
		raw, err := os.ReadFile(*configPath)
		if err != nil {
			return nil, fmt.Errorf("config file: %w", err)
		}
		// Strict decoding: a typo'd or stale key must fail loudly, not be
		// silently ignored — that is the consistency this package promises.
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("config file %s: %w", *configPath, err)
		}
	}

	// Explicitly-set flags override the file.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	apply := func(name string, dst any, val any) {
		if set[name] {
			switch p := dst.(type) {
			case *string:
				*p = val.(string)
			case *int:
				*p = val.(int)
			}
		}
	}
	apply("listen", &cfg.Listen, *listen)
	apply("root", &cfg.Root, *root)
	apply("init-repo", &cfg.InitRepo, *initRepo)
	apply("meta", &cfg.MetaBackend, *metaBackend)
	apply("db", &cfg.DBPath, *dbPath)
	apply("role", &cfg.Role, *role)
	apply("node-id", &cfg.NodeID, *nodeID)
	apply("advertise", &cfg.Advertise, *advertise)
	apply("refs-addr", &cfg.RefsAddr, *refsAddr)
	apply("default-replicas", &cfg.DefaultR, *defaultR)
	apply("write-acks", &cfg.WriteAcks, *writeAcks)
	apply("health-suspect-sec", &cfg.SuspectSec, *suspect)
	apply("health-dead-sec", &cfg.DeadSec, *dead)
	apply("recon-interval-sec", &cfg.ReconSec, *reconSec)
	apply("recon-concurrency", &cfg.ReconCurrency, *reconCur)
	apply("heartbeat-sec", &cfg.HeartbeatSec, *heartbeat)
	return &cfg, cfg.normalize()
}

// normalize validates cross-field consistency and fills derived defaults.
func (c *Config) normalize() error {
	if c.MetaBackend == "" {
		c.MetaBackend = "mem"
	}
	switch c.MetaBackend {
	case "mem", "sqlite":
	default:
		return fmt.Errorf("config: meta %q: want mem|sqlite", c.MetaBackend)
	}
	if c.Role == "" {
		c.Role = "all-in-one"
	}
	switch c.Role {
	case "all-in-one", "gateway", "refs", "storage":
	default:
		return fmt.Errorf("config: role %q: want all-in-one|gateway|refs|storage", c.Role)
	}
	if c.Listen == "" {
		return fmt.Errorf("config: listen must not be empty")
	}
	if c.Root == "" {
		return fmt.Errorf("config: root must not be empty")
	}
	if c.MetaBackend == "sqlite" && c.DBPath == "" {
		c.DBPath = filepath.Join(c.Root, "banyan.db")
	}
	switch c.WriteAcks {
	case "quorum", "all", "one":
	default:
		return fmt.Errorf("config: write_acks %q: want quorum|all|one", c.WriteAcks)
	}
	if c.DefaultR < 1 {
		c.DefaultR = 1
	}
	switch c.Role {
	case "gateway":
		if c.RefsAddr == "" {
			return fmt.Errorf("config: role gateway requires refs_addr")
		}
	case "storage":
		if c.RefsAddr == "" {
			return fmt.Errorf("config: role storage requires refs_addr")
		}
		if c.NodeID == "" {
			return fmt.Errorf("config: role storage requires node_id")
		}
		if c.Advertise == "" {
			c.Advertise = "127.0.0.1:" + portOf(c.Listen)
		}
	}
	c.RefsAddr = withScheme(c.RefsAddr)
	return nil
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i+1:]
	}
	return "8080"
}

func withScheme(addr string) string {
	if addr == "" || strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

// Describe renders the effective configuration for startup logging.
func (c *Config) Describe() string {
	return fmt.Sprintf("role=%s listen=%s root=%s meta=%s db=%q init_repo=%q refs_addr=%q node_id=%q advertise=%q default_replicas=%d write_acks=%s",
		c.Role, c.Listen, c.Root, c.MetaBackend, c.DBPath, c.InitRepo, c.RefsAddr, c.NodeID, c.Advertise, c.DefaultR, c.WriteAcks)
}
