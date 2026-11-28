// mergerequests.go — the merge-request command family (docs/design/merge-requests.md,
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

	"github.com/spf13/cobra"
)

// newMergeRequestCmd builds the family. The canonical spelling is
// merge-request; the camelCase mergeRequest stays as an alias (it was the
// original dispatch word).
func newMergeRequestCmd(wsRoot *string, stdout, stderr io.Writer) *cobra.Command {
	parent := &cobra.Command{
		Use:     "merge-request",
		Aliases: []string{"mergeRequest"},
		Short:   "merge-request lifecycle (server-side merge, approvals)",
		RunE: familyRun(stderr,
			"merge-request: want ls|open|show|approve|merge|close|reopen",
			"merge-request: unknown subcommand %q"),
	}
	parent.AddCommand(&cobra.Command{
		Use:   "ls [state]",
		Short: "list merge requests (optionally filtered by state)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			state := ""
			if len(args) == 1 {
				state = args[0]
			}
			if code := mrList(*wsRoot, state, stdout, stderr); code != 0 {
				return &exitError{code}
			}
			return nil
		},
	})
	parent.AddCommand(&cobra.Command{
		Use:   "open <source> <target> <title...>",
		Short: "open a merge request",
		Args:  cobra.MinimumNArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			if code := mrOpen(*wsRoot, args[0], args[1], strings.Join(args[2:], " "), stdout, stderr); code != 0 {
				return &exitError{code}
			}
			return nil
		},
	})
	// The iid-taking verbs share one shape: exactly one positive integer.
	for _, verb := range []string{"show", "approve", "merge", "close", "reopen"} {
		parent.AddCommand(&cobra.Command{
			Use:   verb + " <iid>",
			Short: verb + " one merge request",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				iid, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || iid <= 0 {
					return &exitError{usageErr(stderr, "merge-request: iid must be a positive integer")}
				}
				if code := mrVerb(*wsRoot, verb, iid, stdout, stderr); code != 0 {
					return &exitError{code}
				}
				return nil
			},
		})
	}
	return parent
}

// mrWS loads the workspace and its client — every verb needs the same pair.
func mrWS(wsRoot string) (*workspace, *client, error) {
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return nil, nil, err
	}
	return ws, wsClient(ws), nil
}

func mrList(wsRoot, state string, stdout, stderr io.Writer) int {
	ws, c, err := mrWS(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	mergeRequests, err := c.mergeRequestList(ws.cfg.Repo, state)
	if err != nil {
		return cmdErr(stderr, err)
	}
	for _, m := range mergeRequests {
		fmt.Fprintf(stdout, "!%-3d %-7s %-24.24s %s → %s (%s)\n",
			m.IID, m.State, m.Title, m.Source, m.Target, m.Author)
	}
	return 0
}

func mrOpen(wsRoot, source, target, title string, stdout, stderr io.Writer) int {
	ws, c, err := mrWS(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	m, err := c.mergeRequestOpen(ws.cfg.Repo, source, target, title)
	if err != nil {
		return cmdErr(stderr, err)
	}
	fmt.Fprintf(stdout, "!%d %s\n", m.IID, m.URL)
	return 0
}

// mrVerb drives the one-iid lifecycle verbs; merge prints the merge commit,
// show the detail view, the rest are silent successes.
func mrVerb(wsRoot string, verb string, iid int64, stdout, stderr io.Writer) int {
	ws, c, err := mrWS(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	switch verb {
	case "show":
		v, err := c.mergeRequestGet(ws.cfg.Repo, iid)
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
		if err := c.mergeRequestPostVoid(ws.cfg.Repo, iid, "approve"); err != nil {
			return cmdErr(stderr, err)
		}
		return 0
	case "merge":
		m, err := c.mergeRequestMerge(ws.cfg.Repo, iid, "")
		if err != nil {
			return cmdErr(stderr, err)
		}
		fmt.Fprintf(stdout, "!%d merged as %s\n", m.IID, m.MergeCommit)
		return 0
	default: // close | reopen
		if err := c.mergeRequestPostVoid(ws.cfg.Repo, iid, verb); err != nil {
			return cmdErr(stderr, err)
		}
		return 0
	}
}
