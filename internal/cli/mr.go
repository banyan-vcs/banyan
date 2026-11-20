// mr.go — the merge-request command family (docs/design/merge-requests.md,
// docs/client/cli.md §6): ls / open / show / approve / merge / close /
// reopen, all against the workspace's repo. The server owns the merge
// (3-way over the raw timeline heads, approvals counted, protection rules
// enforced) — the CLI only drives the lifecycle.
package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

func runMR(wsRoot string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "mr: want ls|open|show|approve|merge|close|reopen")
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	c := wsClient(ws)
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls":
		state := ""
		if len(rest) == 1 {
			state = rest[0]
		} else if len(rest) > 1 {
			return usageErr(stderr, "mr ls: want [state]")
		}
		mrs, err := c.mrList(ws.cfg.Repo, state)
		if err != nil {
			return cmdErr(stderr, err)
		}
		for _, m := range mrs {
			fmt.Fprintf(stdout, "!%-3d %-7s %-24.24s %s → %s (%s)\n",
				m.IID, m.State, m.Title, m.Source, m.Target, m.Author)
		}
		return 0
	case "open":
		if len(rest) < 3 {
			return usageErr(stderr, "mr open: want <source> <target> <title...>")
		}
		m, err := c.mrOpen(ws.cfg.Repo, rest[0], rest[1], strings.Join(rest[2:], " "))
		if err != nil {
			return cmdErr(stderr, err)
		}
		fmt.Fprintf(stdout, "!%d %s\n", m.IID, m.URL)
		return 0
	case "show":
		if len(rest) != 1 {
			return usageErr(stderr, "mr show: want <iid>")
		}
		iid, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil || iid <= 0 {
			return usageErr(stderr, "mr show: iid must be a positive integer")
		}
		v, err := c.mrGet(ws.cfg.Repo, iid)
		if err != nil {
			return cmdErr(stderr, err)
		}
		fmt.Fprintf(stdout, "!%d %s [%s]\n%s → %s · opened by %s\n", v.IID, v.Title, v.State, v.Source, v.Target, v.Author)
		if v.State == "open" {
			if v.Mergeable {
				fmt.Fprintln(stdout, "mergeable: no conflicts")
			} else {
				fmt.Fprintf(stdout, "conflicts: %s\n", strings.Join(v.Conflicts, ", "))
			}
			if v.RuleLevel != "" {
				fmt.Fprintf(stdout, "target rule: %s (approvals %d/%d)\n", v.RuleLevel, len(v.Approvals), v.RequiredApprovals)
			}
		}
		if len(v.Approvals) > 0 {
			fmt.Fprintf(stdout, "approved by: %s\n", strings.Join(v.Approvals, ", "))
		}
		if v.MergeCommit != "" {
			fmt.Fprintf(stdout, "merged as %s by %s\n", v.MergeCommit, v.MergedBy)
		}
		return 0
	case "approve":
		if len(rest) != 1 {
			return usageErr(stderr, "mr approve: want <iid>")
		}
		iid, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil || iid <= 0 {
			return usageErr(stderr, "mr approve: iid must be a positive integer")
		}
		if err := c.mrPostVoid(ws.cfg.Repo, iid, "approve"); err != nil {
			return cmdErr(stderr, err)
		}
		return 0
	case "merge":
		if len(rest) != 1 {
			return usageErr(stderr, "mr merge: want <iid> (the default message lands server-side)")
		}
		iid, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil || iid <= 0 {
			return usageErr(stderr, "mr merge: iid must be a positive integer")
		}
		m, err := c.mrMerge(ws.cfg.Repo, iid, "")
		if err != nil {
			return cmdErr(stderr, err)
		}
		fmt.Fprintf(stdout, "!%d merged as %s\n", m.IID, m.MergeCommit)
		return 0
	case "close", "reopen":
		if len(rest) != 1 {
			return usageErr(stderr, "mr %s: want <iid>", sub)
		}
		iid, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil || iid <= 0 {
			return usageErr(stderr, "mr %s: iid must be a positive integer", sub)
		}
		if err := c.mrPostVoid(ws.cfg.Repo, iid, sub); err != nil {
			return cmdErr(stderr, err)
		}
		return 0
	default:
		return usageErr(stderr, "mr: unknown subcommand %q", sub)
	}
}
