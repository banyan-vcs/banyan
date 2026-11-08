// sync.go — banyan sync: explicit subtree→root alignment for thin subtree
// workspaces (docs/06 §3.9, docs/02 §12). True divergence — both the subtree
// timeline and the root projection advanced — keeps the local head on read
// and waits for exactly this command: the server replays the local-only
// commits onto the root's current projection (the thin client holds no
// history to rebase itself), then sync materializes the result through the
// ordinary pull path. A conflict is true content divergence on the named
// paths — v1 has no half-way resume; resolve by hand and sync again.
package cli

import (
	"fmt"
	"io"
)

func runSync(wsRoot string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return usageErr(stderr, "sync: unexpected argument %q", args[0])
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	if ws.cfg.Sub == "" {
		fmt.Fprintln(stderr, "sync aligns a SUBTREE timeline onto the root view — this is a root workspace (nothing to sync)")
		return 2
	}
	st, err := ws.scan()
	if err != nil {
		return cmdErr(stderr, err)
	}
	if st.dirty() {
		fmt.Fprintln(stderr, "conflict: working tree has local changes (commit or revert first)")
		return 1
	}
	c := newClient(ws.cfg.Server)
	res, err := c.sync(ws.cfg.Repo, ws.cfg.Sub, ws.cfg.Branch)
	if err != nil {
		if isConflictErr(err) {
			fmt.Fprintln(stderr, "conflict: the root view changed this subtree incompatibly — resolve by hand, then sync again")
			fmt.Fprintln(stderr, "hint: clone the root repo with git, push a reconciling commit for the paths below (making the local diff apply cleanly), or rewrite the local side")
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return cmdErr(stderr, err)
	}
	switch res.Status {
	case "rebased":
		fmt.Fprintf(stdout, "rebased %d commit(s) onto the root view → %s\n", res.Rebased, short(res.Head))
	case "uptodate":
		fmt.Fprintln(stdout, "nothing to align (the local timeline is already consistent with the root projection)")
	}
	return pullCore(ws, st, c, stdout, stderr)
}
