package cli

// user.go — account administration over the JSON API (docs/gateway/
// authorization.md §2a): the administrator's password reset. The fresh
// random password prints exactly once; the account then owes a
// self-chosen password at its next web login (the console's must-change
// fence — the admin hands over a secret neither of you keeps).
import (
	"fmt"
	"io"
	"os"
)

func runUser(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "user: want reset-password <name>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "reset-password":
		if len(rest) != 1 {
			return usageErr(stderr, "user reset-password: want <name>")
		}
		return runUserReset(rest[0], stdout, stderr)
	default:
		return usageErr(stderr, "user: unknown subcommand %q (want reset-password)", sub)
	}
}

// runUserReset is a server-side admin act: no workspace involved, the
// server comes from BANYAN_SERVER or the logged-in default host, and the
// caller's own session credential must carry admin rank.
func runUserReset(name string, stdout, stderr io.Writer) int {
	var server string
	if s := os.Getenv("BANYAN_SERVER"); s != "" {
		server = s
	} else if cfg := loadConfig(); cfg.DefaultHost != "" {
		server = cfg.DefaultHost
	} else {
		return usageErr(stderr, "user reset-password: set BANYAN_SERVER or log in and pick a default host (server-side admin command, no workspace)")
	}
	var out struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := newClient(server).post("/api/v1/auth/reset-password", map[string]string{"user": name}, &out); err != nil {
		return cmdErr(stderr, err)
	}
	fmt.Fprintf(stdout, "%s's new password (shown once): %s\n", out.User, out.Password)
	fmt.Fprintln(stdout, "the account must choose its own password at the next login")
	return 0
}
