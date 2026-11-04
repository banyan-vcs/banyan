package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "banyan.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsWhenNoArgs(t *testing.T) {
	cfg, err := Load([]string{"-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.Root != "./data" || cfg.MetaBackend != "mem" {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestYAMLOverridesDefaults(t *testing.T) {
	path := writeYAML(t, "dev: true\nlisten: 127.0.0.1:9000\nroot: /srv/shards\nmeta: sqlite\ndb: /srv/meta.db\ninit_repo: demo\n")
	cfg, err := Load([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9000" || cfg.Root != "/srv/shards" ||
		cfg.MetaBackend != "sqlite" || cfg.DBPath != "/srv/meta.db" || cfg.InitRepo != "demo" {
		t.Fatalf("yaml config = %+v", cfg)
	}
}

func TestExplicitFlagBeatsYAML(t *testing.T) {
	path := writeYAML(t, "dev: true\nlisten: 127.0.0.1:9000\nmeta: sqlite\ndb: /srv/meta.db\n")
	cfg, err := Load([]string{"-config", path, "-listen", ":9999", "-meta", "mem"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9999" || cfg.MetaBackend != "mem" {
		t.Fatalf("flag precedence = %+v", cfg)
	}
	// -meta flipped to mem: the file's db value stays inert but is kept as-is.
	if cfg.DBPath != "/srv/meta.db" {
		t.Fatalf("untouched file values must persist: %+v", cfg)
	}
}

func TestUnsetFlagKeepsYAMLValue(t *testing.T) {
	path := writeYAML(t, "dev: true\nlisten: 127.0.0.1:9000\n")
	cfg, err := Load([]string{"-config", path, "-root", "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9000" || cfg.Root != "/x" {
		t.Fatalf("mixed precedence = %+v", cfg)
	}
}

func TestSQLiteDBDefaultsUnderRoot(t *testing.T) {
	cfg, err := Load([]string{"-dev", "-meta", "sqlite", "-root", "/srv/data"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/srv/data", "banyan.db"); cfg.DBPath != want {
		t.Fatalf("db default = %q, want %q", cfg.DBPath, want)
	}
}

func TestStrictUnknownKeyFails(t *testing.T) {
	path := writeYAML(t, "dev: true\nlisten: :9000\nlisen: :1\n") // typo must not pass silently
	if _, err := Load([]string{"-config", path}); err == nil || !strings.Contains(err.Error(), "lisen") {
		t.Fatalf("unknown key = %v, want a loud error naming it", err)
	}
}

func TestMissingConfigFileFails(t *testing.T) {
	if _, err := Load([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml")}); err == nil {
		t.Fatal("missing config file must fail")
	}
}

func TestInvalidMetaFails(t *testing.T) {
	if _, err := Load([]string{"-meta", "bolt"}); err == nil {
		t.Fatal("invalid meta backend must fail")
	}
	path := writeYAML(t, "dev: true\nmeta: rocks\n")
	if _, err := Load([]string{"-config", path}); err == nil {
		t.Fatal("invalid meta in file must fail")
	}
}

func TestDebugSection(t *testing.T) {
	path := writeYAML(t, "dev: true\ndebug:\n  pprof_addr: 127.0.0.1:6060\n  mutex_profile_fraction: 1\n  block_profile_rate_ns: 10000\n")
	cfg, err := Load([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Debug.PprofAddr != "127.0.0.1:6060" || cfg.Debug.MutexProfileFraction != 1 || cfg.Debug.BlockProfileRateNs != 10000 {
		t.Fatalf("debug section = %+v", cfg.Debug)
	}
	if !strings.Contains(cfg.Describe(), "debug_pprof=\"127.0.0.1:6060\"") {
		t.Fatalf("Describe must surface the ops plane: %s", cfg.Describe())
	}
}

func TestDebugRatesRequireAddr(t *testing.T) {
	path := writeYAML(t, "dev: true\ndebug:\n  mutex_profile_fraction: 1\n")
	if _, err := Load([]string{"-config", path}); err == nil || !strings.Contains(err.Error(), "require debug.pprof_addr") {
		t.Fatalf("rates without pprof_addr = %v, want a loud error", err)
	}
}

// TestProductionRequiresAuth pins the secure-by-default posture: the
// client-facing roles refuse to start unauthenticated, the gitaly frontend
// refuses an empty token, dev mode is the explicit opt-out, and the
// cluster-internal roles (refs/storage) are not client-facing.
func TestProductionRequiresAuth(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"plain-allin-one", "listen: 127.0.0.1:8100\n", "requires authentication"},
		{"gateway-no-auth", "role: gateway\nrefs_addr: 127.0.0.1:8200\n", "requires authentication"},
		{"gitaly-no-token", "gitaly:\n  listen: 127.0.0.1:8300\nauth:\n  users:\n    ops: pw\n", "gitaly.token"},
		{"dev-optout", "dev: true\n", ""},
		{"dev-gitaly-no-token", "dev: true\ngitaly:\n  listen: 127.0.0.1:8300\n", ""},
		{"auth-users-ok", "auth:\n  users:\n    ops: pw\n", ""},
		{"refs-internal-ok", "role: refs\n", ""},
		{"storage-internal-ok", "role: storage\nrefs_addr: 127.0.0.1:8200\nnode_id: s1\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load([]string{"-config", writeYAML(t, tc.yaml), "-root", t.TempDir(), "-db", filepath.Join(t.TempDir(), "b.db")})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got config %+v", tc.wantErr, cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
