package main

// The cobra seam on the server side: the bare-flag legacy shim, the
// single-dash rewriter, and the stdlib-vs-pflag conformance over the same
// ServeFlagTable (the precedence defaults < YAML < explicit flags must not
// depend on which parser fed LoadFromFlags).
import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/pflag"

	"github.com/banyan-vcs/banyan/internal/config"
)

func TestPrependServe(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty is serve", nil, []string{"serve"}},
		{"leading flag is serve", []string{"-config", "x.yaml"}, []string{"serve", "-config", "x.yaml"}},
		{"double-dash too", []string{"--dev"}, []string{"serve", "--dev"}},
		{"subcommand untouched", []string{"web", "-listen", ":3000"}, []string{"web", "-listen", ":3000"}},
		{"explicit serve untouched", []string{"serve", "-dev"}, []string{"serve", "-dev"}},
		{"help reaches the root", []string{"--help"}, []string{"--help"}},
		{"short help too", []string{"-h"}, []string{"-h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := prependServe(tc.in)
			if tc.in == nil {
				if len(got) != 1 || got[0] != "serve" {
					t.Fatalf("nil argv → %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) && (len(got) != len(tc.want) || got[0] != tc.want[0]) {
				t.Fatalf("prependServe(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestBanyandNormalizeLegacyDash(t *testing.T) {
	in := []string{"serve", "-config", "banyan.yaml", "-listen", "0.0.0.0:8080", "-root", "/var/lib/banyan", "-meta", "sqlite", "--", "-positional"}
	want := []string{"serve", "--config", "banyan.yaml", "--listen", "0.0.0.0:8080", "--root", "/var/lib/banyan", "--meta", "sqlite", "--", "-positional"}
	if got := normalizeLegacyDash(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeLegacyDash(%v)\n got %v\nwant %v", in, got, want)
	}
}

// TestServePflagMatchesStdlibLoad is the conformance pin: for the same
// argv (single-dash legacy included), the pflag-built serve command and
// config.Load's stdlib set must resolve to the identical Config.
func TestServePflagMatchesStdlibLoad(t *testing.T) {
	yamlPath := filepath.Join(t.TempDir(), "banyan.yaml")
	if err := os.WriteFile(yamlPath, []byte("listen: :9999\nroot: ./from-yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()

	samples := [][]string{
		{"-dev"}, // bare argv resolves: the production posture demands -config/-db
		{"-dev", "-listen", "127.0.0.1:8097", "-meta", "sqlite", "-root", tmp},
		{"-config", yamlPath, "-dev"},
		{"-config", yamlPath, "-listen", "127.0.0.1:1", "-dev"}, // explicit flag beats YAML
		{"-role", "storage", "-refs-addr", "127.0.0.1:8102", "-node-id", "s1", "-listen", ":8103", "-dev"},
		{"--advertise", "h:1", "--health-suspect-sec", "7", "--recon-concurrency", "9", "--dev"},
	}
	for _, argv := range samples {
		want, err := config.Load(argv)
		if err != nil {
			t.Fatalf("config.Load(%v): %v", argv, err)
		}
		fl := pflag.NewFlagSet("serve", pflag.ContinueOnError)
		registerServeFlags(fl)
		if err := fl.Parse(normalizeLegacyDash(argv)); err != nil {
			t.Fatalf("pflag parse %v: %v", argv, err)
		}
		got, err := config.LoadFromFlags(pflagSource{fl})
		if err != nil {
			t.Fatalf("LoadFromFlags(%v): %v", argv, err)
		}
		if !reflect.DeepEqual(*got, *want) {
			t.Fatalf("argv %v:\n pflag got %+v\n stdlib want %+v", argv, *got, *want)
		}
	}
}
