// Package cli implements the banyan thin client (docs/06): a smart remote
// control over the JSON API — minimal download (view tree + checkout blobs,
// never the commit closure), history questions answered server-side, a
// content-addressed local cache instead of a .git directory.
//
//	Run(argv, stdout, stderr) → exit code (0 ok / 1 conflict / 2 server /
//	3 network) — script- and agent-friendly by design.
package cli

import (
	"fmt"
	"io"
)

const usageText = `banyan — thin client for a banyan server (docs/06)

usage: banyan [-C <dir>] <command> [args]

workspace commands (run inside a clone, or with -C):
  clone <url>[/tree/<dir>] [dir] [--sparse a,b] [-b branch]   minimal clone
  status                                                      manifest vs worktree (offline)
  commit -m <msg> [--name n] [--email e]                      scan → upload → server-side assembly
  pull                                                        server tree-diff increment
  log [path] [-n N]                                           remote history, zero download
  cat [rev:]<path>                                            object content (HEAD resolves locally)
  fetch-objects <oid>...                                      backfill objects into the cache
  branch | tag                                                list remote refs
  repo ls                                                     list server repos (needs a workspace or BANYAN_SERVER)

exit codes: 0 ok · 1 conflict · 2 server/usage error · 3 network`

// Run executes one CLI invocation and returns the process exit code.
func Run(argv []string, stdout, stderr io.Writer) int {
	args := argv
	wsRoot := "."
	if len(args) > 0 && args[0] == "-C" {
		if len(args) < 2 {
			fmt.Fprintln(stderr, "error: -C needs a directory")
			return 2
		}
		wsRoot, args = args[1], args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "clone":
		return runClone(wsRoot, rest, stdout, stderr)
	case "status":
		return runStatus(wsRoot, rest, stdout, stderr)
	case "commit":
		return runCommit(wsRoot, rest, stdout, stderr)
	case "pull":
		return runPull(wsRoot, rest, stdout, stderr)
	case "log":
		return runLog(wsRoot, rest, stdout, stderr)
	case "cat":
		return runCat(wsRoot, rest, stdout, stderr)
	case "fetch-objects":
		return runFetchObjects(wsRoot, rest, stdout, stderr)
	case "branch":
		return runRefsList(wsRoot, rest, stdout, stderr, "refs/heads/")
	case "tag":
		return runRefsList(wsRoot, rest, stdout, stderr, "refs/tags/")
	case "repo":
		return runRepo(wsRoot, rest, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usageText)
		return 0
	default:
		fmt.Fprintf(stderr, "error: unknown command %q\n\n%s\n", cmd, usageText)
		return 2
	}
}
