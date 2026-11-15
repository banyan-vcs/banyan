package cli

// login.go — `banyan login` (docs/client/cli.md §4.5): NO permanent tokens.
// Preference order, per deployment capability broadcast (/api/v1/proto):
//
//  1. SSH — the server's advertised SSH address + the user's agent/default
//     keys run the `banyan-auth` exec; the server mints a 12h session.
//     Host keys are trust-on-first-use (pinned in ~/.banyan/known_hosts.json);
//     a CHANGED key is an error. SSH sessions refresh silently on expiry.
//  2. web — the server's advertised web console opens /cli-auth#<handshake>:
//     the user logs in with whatever the console fronts (local or
//     GitHub/GitLab OAuth) and approves; the CLI polls the one-shot grant.
//
// Either way what lands in ~/.banyan/credentials.json is a short-lived,
// revocable session credential — never an operator-minted PAT.
import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func runLogin(wsRoot string, args []string, stdout, stderr io.Writer) int {
	var server, user string
	status := false
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--user" || args[i] == "-u") && i+1 < len(args):
			i++
			user = args[i]
		case args[i] == "--status":
			status = true
		case strings.HasPrefix(args[i], "-") && args[i] != "-":
			return usageErr(stderr, "login: unknown flag %q", args[i])
		case server == "":
			server = args[i]
		default:
			return usageErr(stderr, "login: unexpected argument %q", args[i])
		}
	}
	explicit := server != ""
	srv, err := serverArgFrom(wsRoot, server)
	if err != nil {
		// No resolvable server: offer the configured hosts (plus a new-URL
		// entry) before giving up — the gh-style first-run experience.
		srv, err = pickServerInteractive(stdout)
		if err != nil {
			return cmdErr(stderr, err)
		}
	}
	if status {
		return loginStatus(srv, explicit, stdout, stderr)
	}
	if user == "" {
		user = os.Getenv("USER")
		if user == "" {
			user = os.Getenv("USERNAME")
		}
	}
	c := newClient(srv)
	caps := c.discoverCaps()

	if caps != nil && caps.SSH != "" {
		minted, err := sshMint(caps.SSH, user)
		if err == nil {
			putCred(srv, credential{User: minted.User, Token: minted.Token, Expires: minted.Expires, Method: "ssh", SSHAddr: caps.SSH})
			fmt.Fprintf(stdout, "logged in %s as %s via SSH (session expires %s)\n", srv, minted.User, timeFmt(minted.Expires))
			printDefaultIfMarked(srv, stdout)
			return 0
		}
		fmt.Fprintf(stderr, "ssh login unavailable (%v)", err)
		if caps.Web == "" {
			fmt.Fprintln(stderr)
			return cmdErr(stderr, fmt.Errorf("login: server advertises no web fallback"))
		}
		fmt.Fprintln(stderr, " — falling back to the web flow")
	}
	if caps == nil || caps.Web == "" {
		return cmdErr(stderr, fmt.Errorf("login: %s advertises neither SSH nor web login (proto broadcast)", srv))
	}
	granted, err := webLogin(caps.Web, stdout)
	if err != nil {
		return cmdErr(stderr, err)
	}
	putCred(srv, credential{User: granted.User, Token: granted.Token, Expires: granted.Expires, Method: "web"})
	fmt.Fprintf(stdout, "logged in %s as %s via web approval (session expires %s)\n", srv, granted.User, timeFmt(granted.Expires))
	printDefaultIfMarked(srv, stdout)
	return 0
}

// printDefaultIfMarked notes the default host after a login when more than
// one server is configured (the first login already became the default —
// putCred; this tells multi-host users which one is active).
func printDefaultIfMarked(srv string, stdout io.Writer) {
	if cfg := loadConfig(); cfg.DefaultHost == serverKey(srv) && len(cfg.Hosts) > 1 {
		fmt.Fprintf(stdout, "default server: %s (switch: banyan host use)\n", cfg.DefaultHost)
	}
}

// pickServerInteractive offers the configured hosts plus a new-URL entry —
// the no-argument `banyan login` on a multi-host machine. Errors when there
// is nothing configured (the classic pass-the-URL hint).
func pickServerInteractive(stdout io.Writer) (string, error) {
	cfg := loadConfig()
	keys := sortedHostKeys(cfg)
	if len(keys) == 0 {
		return "", errNoServer
	}
	items := make([]string, len(keys), len(keys)+1)
	copy(items, keys)
	items = append(items, "another server…")
	i, err := pickFromList(stdout, os.Stdin, "log in to:", items)
	if err != nil {
		return "", fmt.Errorf("login: no selection — pass the server URL")
	}
	if i < len(keys) {
		return keys[i], nil
	}
	url, err := promptLine(stdout, os.Stdin, "server URL: ")
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	return url, nil
}

// loginStatus reports sessions across hosts (gh auth status parity): one
// line per configured server, network-verified. An explicit server narrows
// to that host; the listing form covers every island the machine knows.
func loginStatus(srv string, explicit bool, stdout, stderr io.Writer) int {
	cfg := loadConfig()
	keys := sortedHostKeys(cfg)
	if explicit {
		key := serverKey(srv)
		e, ok := cfg.Hosts[key]
		if !ok || e.Token == "" {
			fmt.Fprintf(stdout, "not logged in to %s\n", srv)
			return 1
		}
		fmt.Fprintf(stdout, "%s as %s via %s, expires %s (%s)\n", srv, e.User, e.Method, timeFmt(e.Expires), probeSession(key, e))
		return 0
	}
	if len(keys) == 0 {
		fmt.Fprintln(stdout, "not logged in anywhere (banyan login <server>)")
		return 1
	}
	for _, k := range keys {
		e := cfg.Hosts[k]
		mark := " "
		if k == cfg.DefaultHost {
			mark = "*"
		}
		fmt.Fprintf(stdout, "%s %s as %s via %s, expires %s (%s)\n", mark, k, e.User, e.Method, timeFmt(e.Expires), probeSession(k, e))
	}
	if cfg.DefaultHost != "" {
		fmt.Fprintln(stdout, "default: "+cfg.DefaultHost+" — switch with: banyan host use <server>")
	}
	return 0
}

// probeSession verifies a stored session against its server (one auth'd
// GET, 5s budget — the same single source of truth every command rides).
// Deliberately NOT the workspace client: probing must never re-mint.
func probeSession(server string, e *hostEntry) string {
	if e.Token == "" {
		return "logged out"
	}
	if e.Expires != 0 && e.Expires < time.Now().Unix() {
		return "EXPIRED (re-authenticate: banyan login)"
	}
	req, err := http.NewRequest("GET", strings.TrimSuffix(server, "/")+"/api/v1/repos", nil)
	if err != nil {
		return "unreachable"
	}
	req.Header.Set("Authorization", basicHeaderValue(e.User, e.Token))
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "server unreachable"
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "rejected (re-authenticate: banyan login)"
	}
	return "valid"
}

func timeFmt(unix int64) string {
	if unix == 0 {
		return "never"
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}

type mintedSession struct {
	Token   string `json:"token"`
	User    string `json:"user"`
	Expires int64  `json:"expires"`
}

// sshMint runs the banyan-auth exec over SSH: agent keys first, then the
// default key files. The host key is TOFU-pinned; a change is fatal.
func sshMint(addr, user string) (*mintedSession, error) {
	authMethods, err := sshAuthMethods()
	if err != nil || len(authMethods) == 0 {
		return nil, fmt.Errorf("no SSH keys (agent or ~/.ssh/id_ed25519 & friends)")
	}
	pinned := hostKeyOf(addr)
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: tofuCallback(addr, &pinned),
		Timeout:         15 * time.Second,
	}
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	if pinned != "" && hostKeyOf(addr) == "" {
		pinHostKey(addr, pinned) // first use succeeded: remember the key
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.Run("banyan-auth"); err != nil {
		return nil, fmt.Errorf("banyan-auth: %w (%s)", err, strings_oneLine(out.String()))
	}
	var m mintedSession
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || m.Token == "" {
		return nil, fmt.Errorf("banyan-auth: unreadable answer")
	}
	return &m, nil
}

func strings_oneLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// tofuCallback accepts any host key on first sight (pinning it) and only
// the pinned key afterwards.
func tofuCallback(addr string, pinned *string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		blob := base64.StdEncoding.EncodeToString(key.Marshal())
		if *pinned == "" {
			*pinned = blob
			pinHostKey(addr, blob)
			return nil
		}
		if *pinned != blob {
			return fmt.Errorf("host key CHANGED for %s — refusing (remove ~/.banyan/known_hosts.json entry only if you know why)", addr)
		}
		return nil
	}
}

// sshAuthMethods collects agent signers plus the default key files;
// BANYAN_SSH_KEY points at one explicit key first (CI, agents, tests).
func sshAuthMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if c, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(c)
			if signers, err := ag.Signers(); err == nil && len(signers) > 0 {
				methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			}
		}
	}
	candidates := []string{}
	if explicit := os.Getenv("BANYAN_SSH_KEY"); explicit != "" {
		candidates = append(candidates, explicit)
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			candidates = append(candidates, filepath.Join(home, ".ssh", name))
		}
	}
	for _, p := range candidates {
		if raw, rerr := os.ReadFile(p); rerr == nil {
			if signer, perr := ssh.ParsePrivateKey(raw); perr == nil {
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	}
	return methods, nil
}

// webLogin drives the console approval flow: print the /cli-auth URL, poll
// the one-shot grant (5 minutes).
func webLogin(web string, stdout io.Writer) (*mintedSession, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	fmt.Fprintf(stdout, "open in a browser to approve this CLI:\n  %s/cli-auth#id=%s\n", web, id)
	deadline := time.Now().Add(5 * time.Minute)
	hc := &http.Client{Timeout: 10 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := hc.Get(web + "/api/cli/poll?id=" + id)
		if err == nil {
			var out struct {
				Status  string `json:"status"`
				Token   string `json:"token"`
				User    string `json:"user"`
				Expires int64  `json:"expires"`
			}
			dec := json.NewDecoder(resp.Body)
			_ = dec.Decode(&out)
			resp.Body.Close()
			if out.Status == "granted" && out.Token != "" {
				return &mintedSession{Token: out.Token, User: out.User, Expires: out.Expires}, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("login: approval window timed out")
}

func runLogout(wsRoot string, args []string, stdout, stderr io.Writer) int {
	var server string
	if len(args) > 0 {
		if strings.HasPrefix(args[0], "-") {
			return usageErr(stderr, "logout: unexpected flag %q", args[0])
		}
		server = args[0]
	}
	if server == "" && len(sortedHostKeys(loadConfig())) > 1 {
		// Multiple islands and no target: pick one rather than silently
		// dropping the workspace/default host.
		keys := sortedHostKeys(loadConfig())
		i, err := pickFromList(stdout, os.Stdin, "log out of:", keys)
		if err != nil {
			return cmdErr(stderr, fmt.Errorf("logout: no selection — pass the server: banyan logout <url>"))
		}
		server = keys[i]
	}
	srv, err := serverArgFrom(wsRoot, server)
	if err != nil {
		return cmdErr(stderr, err)
	}
	dropCred(srv)
	fmt.Fprintf(stdout, "logged out of %s (the server-side session lives until it expires — revocable by an admin)\n", srv)
	if cfg := loadConfig(); cfg.DefaultHost == "" && len(cfg.Hosts) > 0 {
		fmt.Fprintln(stdout, "no default server — set one with: banyan host use <server>")
	}
	return 0
}
