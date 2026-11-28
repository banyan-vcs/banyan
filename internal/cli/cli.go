// Package cli implements the banyan thin client (docs/client/cli.md): a smart remote
// control over the JSON API — minimal download (view tree + checkout blobs,
// never the commit closure), history questions answered server-side, a
// content-addressed local cache instead of a .git directory.
//
//	Run(argv, stdout, stderr) → exit code (0 ok / 1 conflict / 2 server /
//	3 network) — script- and agent-friendly by design.
//
// The command tree is spf13/cobra (newRootCmd); this file is the seam that
// keeps the historical contract on top of it: injected writers, diagnostics
// in the "error:" shape, body-chosen exit codes, and the legacy single-dash
// long flags still parsing (normalizeLegacyDash).
package cli

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// Run executes one CLI invocation and returns the process exit code.
func Run(argv []string, stdout, stderr io.Writer) int {
	root := newRootCmd(stdout, stderr)
	if len(argv) == 0 {
		// The historical bare invocation: usage on stderr, exit 2 — not
		// cobra's help-on-stdout-and-zero.
		fmt.Fprintln(stderr, root.UsageString())
		return 2
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(normalizeLegacyDash(argv))
	if err := root.Execute(); err != nil {
		var ec *exitError
		if errors.As(err, &ec) {
			return ec.code // the command body already printed its message
		}
		// A framework error (unknown command, flag or arity): the same
		// "error:" shape and exit 2 the hand-rolled parser used.
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	return 0
}

// exitError carries a body-chosen exit code up through cobra's RunE — the
// legacy command bodies print their own diagnostics and return an int.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("banyan: exit %d", e.code) }

// cmdRun adapts a legacy body (print + return int) to a cobra RunE.
func cmdRun(fn func() int) func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error {
		if code := fn(); code != 0 {
			return &exitError{code}
		}
		return nil
	}
}

// familyRun keeps the command families' bare/unknown-subcommand posture:
// their own usage error on stderr and exit 2 (cobra's default for a parent
// would print help and exit 0).
func familyRun(stderr io.Writer, bare, unknown string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		if len(args) == 0 {
			return &exitError{usageErr(stderr, "%s", bare)}
		}
		return &exitError{usageErr(stderr, unknown, args[0])}
	}
}

// legacyLongDash matches the historical single-dash long flags (-listen,
// -config, -max-mb): a dash, a lowercase letter, then at least one more
// letter/digit/hyphen. True shorthands (-C, -b, -m — exactly two characters)
// and negative numbers (a digit first) do not match.
var legacyLongDash = regexp.MustCompile(`^-[a-z][a-z0-9-]+$`)

// normalizeLegacyDash rewrites single-dash long flags to their --form so
// pflag accepts every documented command line verbatim — README examples,
// the Dockerfile CMD, the compose files and the demo scripts all spell
// `-listen 0.0.0.0:8080`. Tokens after the `--` terminator are positional
// and pass through untouched.
func normalizeLegacyDash(argv []string) []string {
	out := make([]string, 0, len(argv))
	positional := false
	for _, a := range argv {
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 { // -max-mb=512 → name -max-mb
			name = a[:i]
		}
		switch {
		case positional, a == "-", !strings.HasPrefix(a, "-"):
			out = append(out, a)
		case a == "--":
			positional = true
			out = append(out, a)
		case strings.HasPrefix(a, "--") || !legacyLongDash.MatchString(name):
			out = append(out, a)
		default:
			out = append(out, "-"+a)
		}
	}
	return out
}

// newRootCmd builds one invocation's command tree over the injected
// writers. -C/--config are persistent so they parse anywhere (the
// hand-rolled parser only accepted them before the subcommand — a superset).
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	wsRoot := "."
	root := &cobra.Command{
		Use:   "banyan",
		Short: "thin client for a banyan server (docs/client/cli.md)",
		Long: `A smart remote control over the JSON API: minimal download (view tree +
checkout blobs, never the commit closure), history questions answered
server-side, a content-addressed local cache instead of a .git directory.

Workspace commands run inside a clone — any subdirectory — or with -C.
The credential store defaults to ~/conf/banyan.yaml (BANYAN_CONFIG
overrides; --config wins) and holds every logged-in host plus the default.

Exit codes: 0 ok · 1 conflict · 2 server/usage error · 3 network`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&wsRoot, "cd", "C", ".", "run as if banyan was started in <dir>")
	pf.StringVar(&configOverride, "config", "", "credential/config store (default ~/conf/banyan.yaml)")

	root.AddCommand(
		newCloneCmd(&wsRoot, stdout, stderr),
		newStatusCmd(&wsRoot, stdout, stderr),
		newCommitCmd(&wsRoot, stdout, stderr),
		newPullCmd(&wsRoot, stdout, stderr),
		newSyncCmd(&wsRoot, stdout, stderr),
		newSnapshotCmd(&wsRoot, stdout, stderr),
		newStackCmd(&wsRoot, stdout, stderr),
		newLogCmd(&wsRoot, stdout, stderr),
		newCatCmd(&wsRoot, stdout, stderr),
		newFetchObjectsCmd(&wsRoot, stdout, stderr),
		newGCCmd(&wsRoot, stdout, stderr),
		newLoginCmd(&wsRoot, stdout, stderr),
		newLogoutCmd(&wsRoot, stdout, stderr),
		newHostCmd(stdout, stderr),
		newBranchCmd(&wsRoot, stdout, stderr, "refs/heads/"),
		newTagCmd(&wsRoot, stdout, stderr),
		newRepoCmd(&wsRoot, stdout, stderr),
		newMergeRequestCmd(&wsRoot, stdout, stderr),
		newImportCmd(&wsRoot, stdout, stderr),
		newUserCmd(stdout, stderr),
	)
	return root
}
