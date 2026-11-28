package cli

// The cobra seam: the legacy contract on top of the new dispatch —
// single-dash long flags still parse, exit codes keep their meaning,
// help lands on stdout with 0, families keep their bare/unknown posture,
// and the merge-request alias survives.
import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeLegacyDash(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{"long single-dash", []string{"serve", "-listen", "0.0.0.0:8080"}, []string{"serve", "--listen", "0.0.0.0:8080"}},
		{"mixed length", []string{"-dev", "-root", "/tmp"}, []string{"--dev", "--root", "/tmp"}},
		{"already double", []string{"--config=x"}, []string{"--config=x"}},
		{"true shorthands stay", []string{"-C", "/ws", "commit", "-m", "x"}, []string{"-C", "/ws", "commit", "-m", "x"}},
		{"negative number stays", []string{"log", "-n", "-3"}, []string{"log", "-n", "-3"}},
		{"bare dash is positional", []string{"cat", "-"}, []string{"cat", "-"}},
		{"terminator stops rewriting", []string{"cat", "--", "-weird"}, []string{"cat", "--", "-weird"}},
		{"uppercase not a long flag", []string{"-N"}, []string{"-N"}},
		{"equals form rewritten", []string{"-max-mb=512"}, []string{"--max-mb=512"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeLegacyDash(tc.in))
		})
	}
}

func TestCobraExitContract(t *testing.T) {
	// The framework's own failures (unknown command, bad flag, arity) all
	// land on the historical "error:" shape with exit 2 — on stderr.
	for _, args := range [][]string{
		{"definitely-not-a-command"},
		{"status", "--bogus"},
		{"cat"},            // arity: wants exactly one
		{"stack"},          // bare family parent
		{"stack", "bogus"}, // unknown family subcommand
		{"merge-request"},  // bare family parent (canonical spelling)
		{"mergeRequest"},   // ...and through the alias
		{"user"},
		{"host"},
		{"repo"},
		{"repo", "create"}, // any non-ls spelling: the one historical message
	} {
		var out, errb strings.Builder
		code := Run(args, &out, &errb)
		assert.Equal(t, 2, code, "args %v: stderr=%q", args, errb.String())
		assert.NotEmpty(t, errb.String(), "args %v: the diagnostic lands on stderr", args)
		assert.True(t, strings.HasPrefix(errb.String(), "error:"), "args %v: the error: shape", args)
	}

	// Empty argv: usage on stderr, exit 2.
	var out, errb strings.Builder
	assert.Equal(t, 2, Run(nil, &out, &errb))
	assert.Contains(t, errb.String(), "Usage:")
	assert.Empty(t, out.String())
}

func TestCobraHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		var out, errb strings.Builder
		code := Run(args, &out, &errb)
		assert.Equal(t, 0, code, "args %v", args)
		assert.Contains(t, out.String(), "Available Commands:")
		assert.Contains(t, out.String(), "Exit codes: 0 ok", "the exit-code legend rides the root help")
	}

	// Subcommand help lists its flags (single-dash legacy noted).
	var out, errb strings.Builder
	assert.Equal(t, 0, Run([]string{"gc", "--help"}, &out, &errb))
	assert.Contains(t, out.String(), "--max-mb")
}

func TestMergeRequestAliasAndCanonical(t *testing.T) {
	// Both spellings dispatch to the same family (needs a workspace, which
	// is missing here — the load failure is the proof of dispatch).
	for _, spelling := range []string{"merge-request", "mergeRequest"} {
		var out, errb strings.Builder
		code := Run([]string{"-C", t.TempDir(), spelling, "ls"}, &out, &errb)
		assert.Equal(t, 2, code, "%s dispatches like merge-request", spelling)
		assert.NotContains(t, errb.String(), "unknown command", "%s is recognized", spelling)
	}
}

func TestSnapshotBareIsSave(t *testing.T) {
	// Bare `snapshot` (no subcommand) forwards to save — the quirk the
	// integration tests pin. Outside a workspace the load fails with the
	// usual exit 2, which proves the forwarding happened (a bare parent
	// posture would print a usage error instead).
	var out, errb strings.Builder
	code := Run([]string{"-C", t.TempDir(), "snapshot"}, &out, &errb)
	require.Equal(t, 2, code)
	assert.NotContains(t, errb.String(), "subcommand")
}

func TestLegacySingleDashFlagParses(t *testing.T) {
	// End to end through Run: `gc -max-mb 64` (the legacy spelling) must
	// parse as --max-mb. Outside a workspace it fails at the load with
	// exit 2 — but the flag itself must not be the failure.
	var out, errb strings.Builder
	code := Run([]string{"-C", t.TempDir(), "gc", "-max-mb", "64"}, &out, &errb)
	assert.Equal(t, 2, code)
	assert.NotContains(t, errb.String(), "unknown flag")
	assert.NotContains(t, errb.String(), "unknown shorthand")
}
