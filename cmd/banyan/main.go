// banyan is the thin-workspace CLI (docs/05): smart remote control over the
// JSON API — minimal download, server-side history, a content-addressed
// cache instead of a local .git. The dispatch table and exit-code contract
// live in internal/cli (usable in-process by the integration tests).
package main

import (
	"os"

	"github.com/banyan-vcs/banyan/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
