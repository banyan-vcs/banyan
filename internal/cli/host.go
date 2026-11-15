package cli

// host.go — `banyan host` (gh-style multi-host management, docs/client/cli.md
// §4.5). Private deployments have no public login: each server is its own
// island, the config records them all, and ONE of them is the default that
// every server-less resolution falls back to (argument > workspace >
// BANYAN_SERVER > default host). `ls` tabulates; `use` switches — a
// numbered menu when the server is omitted.
import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func runHost(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "host: want ls or use")
	}
	switch args[0] {
	case "ls":
		return hostList(args[1:], stdout, stderr)
	case "use":
		return hostUse(args[1:], stdout, stderr)
	default:
		return usageErr(stderr, "host: unknown subcommand %q (ls | use)", args[0])
	}
}

// hostRow is one line of `host ls`/`login --status` output, text or JSON.
type hostRow struct {
	Server  string `json:"server"`
	User    string `json:"user"`
	Method  string `json:"method"`
	Expires int64  `json:"expires"`
	Default bool   `json:"default"`
	State   string `json:"state"`
}

func writeHostRows(stdout io.Writer, rows []hostRow) {
	raw, _ := json.MarshalIndent(rows, "", "  ")
	fmt.Fprintln(stdout, string(raw))
}

func hostList(args []string, stdout, stderr io.Writer) int {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			return usageErr(stderr, "host ls: unknown flag %q", a)
		}
	}
	cfg := loadConfig()
	keys := sortedHostKeys(cfg)
	if jsonOut {
		rows := make([]hostRow, 0, len(keys))
		for _, k := range keys {
			e := cfg.Hosts[k]
			rows = append(rows, hostRow{Server: k, User: e.User, Method: e.Method, Expires: e.Expires, Default: k == cfg.DefaultHost, State: hostClockState(e)})
		}
		writeHostRows(stdout, rows)
		return 0
	}
	if len(keys) == 0 {
		fmt.Fprintln(stdout, "no configured servers (banyan login <url>)")
		return 0
	}
	for _, k := range keys {
		e := cfg.Hosts[k]
		mark := " "
		if k == cfg.DefaultHost {
			mark = "*"
		}
		fmt.Fprintf(stdout, "%s %s  %s via %s, expires %s (%s)\n", mark, k, e.User, e.Method, timeFmt(e.Expires), hostClockState(e))
	}
	fmt.Fprintln(stdout, "default: "+cfg.DefaultHost+" — switch with: banyan host use <server>")
	return 0
}

// hostClockState judges by the local clock only (no network): `login
// --status` is the probing view.
func hostClockState(e *hostEntry) string {
	if e.Token == "" {
		return "logged out"
	}
	if e.Expires != 0 && e.Expires < time.Now().Unix() {
		return "EXPIRED (banyan login)"
	}
	return "valid"
}

func hostUse(args []string, stdout, stderr io.Writer) int {
	cfg := loadConfig()
	keys := sortedHostKeys(cfg)
	if len(keys) == 0 {
		return cmdErr(stderr, fmt.Errorf("host use: no configured servers (banyan login <url> first)"))
	}
	var key string
	if len(args) > 0 {
		if len(args) > 1 || strings.HasPrefix(args[0], "-") {
			return usageErr(stderr, "host use: want one server URL")
		}
		key = serverKey(args[0])
	} else {
		items := make([]string, len(keys))
		for i, k := range keys {
			items[i] = k
			if k == cfg.DefaultHost {
				items[i] += "  (default)"
			}
		}
		i, err := pickFromList(stdout, os.Stdin, "switch the default server:", items)
		if err != nil {
			return cmdErr(stderr, fmt.Errorf("host use: no selection — pass the server: banyan host use <url>"))
		}
		key = keys[i]
	}
	if _, ok := cfg.Hosts[key]; !ok {
		return cmdErr(stderr, fmt.Errorf("host use: %s is not a configured server (banyan login first)", key))
	}
	cfg.DefaultHost = key
	saveConfig(cfg)
	fmt.Fprintf(stdout, "default server: %s\n", key)
	return 0
}
