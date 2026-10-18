// Package config resolves banyand's runtime configuration with one rule:
// defaults < YAML config file < explicitly-set command-line flags.
// Strict YAML decoding (unknown keys are errors) plus cross-field validation
// keep the effective configuration consistent and observable.
package config

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the effective banyand configuration.
type Config struct {
	Listen      string `yaml:"listen"`    // HTTP listen address
	Root        string `yaml:"root"`      // local shard storage root
	InitRepo    string `yaml:"init_repo"` // repo created at startup (optional)
	MetaBackend string `yaml:"meta"`      // "mem" | "sqlite"
	DBPath      string `yaml:"db"`        // sqlite path; defaults to <root>/banyan.db
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Listen:      ":8080",
		Root:        "./data",
		MetaBackend: "mem",
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
	if set["listen"] {
		cfg.Listen = *listen
	}
	if set["root"] {
		cfg.Root = *root
	}
	if set["init-repo"] {
		cfg.InitRepo = *initRepo
	}
	if set["meta"] {
		cfg.MetaBackend = *metaBackend
	}
	if set["db"] {
		cfg.DBPath = *dbPath
	}
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
	if c.Listen == "" {
		return fmt.Errorf("config: listen must not be empty")
	}
	if c.Root == "" {
		return fmt.Errorf("config: root must not be empty")
	}
	if c.MetaBackend == "sqlite" && c.DBPath == "" {
		c.DBPath = filepath.Join(c.Root, "banyan.db")
	}
	return nil
}

// Describe renders the effective configuration for startup logging.
func (c *Config) Describe() string {
	return fmt.Sprintf("listen=%s root=%s meta=%s db=%q init_repo=%q",
		c.Listen, c.Root, c.MetaBackend, c.DBPath, c.InitRepo)
}
