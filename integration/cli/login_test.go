package cli

// T98 (docs/operations/testing.md): `banyan login` end to end — NO
// permanent tokens. An SSH public key (the banyan-auth exec) mints a 12h
// write-scope session; the CLI carries it on every request; revoking it
// trips a 401 that re-mints SILENTLY over SSH. The whole flow runs
// against a real stack: static policy + TokenPolicy, the git HTTP face
// for seeding, the SSH listener for the mint.
import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestCLILoginSSHSessions(t *testing.T) {
	support.NeedGit(t)
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}

	// alice's keypair: public half in the policy, private half where
	// BANYAN_SSH_KEY points (the same override CI/agents use).
	keyDir := t.TempDir()
	aliceKey := filepath.Join(keyDir, "alice")
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", aliceKey).CombinedOutput()
	require.NoError(t, err, "ssh-keygen: %s", out)
	pub, err := os.ReadFile(aliceKey + ".pub")
	require.NoError(t, err)

	ms := meta.NewMemoryMetaStore()
	policy := &auth.TokenPolicy{
		Inner: &auth.StaticPolicy{
			Users: map[string]string{"alice": "pw"},
			Keys:  map[string][]string{"alice": {strings.TrimSpace(string(pub))}},
		},
		Tokens: ms,
	}
	objects := storage.NewObjectService(ms, storage.NewLocalShardStore(support.TempDirQuiet(t)), []storage.Node{{ID: "t1", Local: true}}, model.SHA1)

	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.Auth = policy
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(hostPriv)
	require.NoError(t, err)
	gw.SSHHostKey = signer
	sshLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sshLn.Close() })
	go func() { _ = gw.ServeSSH(sshLn) }()

	api := rpc.NewServer(ms, objects, model.SHA1)
	api.Auth = policy
	api.SSHAddr = sshLn.Addr().String()
	mux := http.NewServeMux()
	gw.Register(mux)
	api.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)

	// The CLI's home: credentials + TOFU known-hosts land here.
	home := t.TempDir()
	t.Setenv("BANYAN_HOME", home)
	t.Setenv("BANYAN_SSH_KEY", aliceKey)

	// Seed over the authenticated git face (Basic against the inner table;
	// receive-pack auto-creates the repo).
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "t98 seed\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q",
		strings.Replace(hs.URL, "http://", "http://alice:pw@", 1)+"/r/t98.git", "main")

	// ① login: SSH key → 12h session, recorded in the multi-host config.
	stdout, stderr, code := runCLI(t, "login", hs.URL, "-u", "alice")
	require.Zero(t, code, "login failed: %s %s", stdout, stderr)
	assert.Contains(t, stdout, "via SSH")
	cfgRaw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	require.NoError(t, err)
	var cfg struct {
		DefaultHost string `yaml:"default_host"`
		Hosts       map[string]struct {
			User   string `yaml:"user"`
			Token  string `yaml:"token"`
			Method string `yaml:"method"`
		} `yaml:"hosts"`
		SSHHostKeys map[string]string `yaml:"ssh_host_keys"`
	}
	require.NoError(t, yaml.Unmarshal(cfgRaw, &cfg))
	stored := cfg.Hosts[hs.URL]
	require.Equal(t, "alice", stored.User)
	require.Equal(t, "ssh", stored.Method)
	require.True(t, strings.HasPrefix(stored.Token, "banyan_"))
	require.Equal(t, hs.URL, cfg.DefaultHost, "the first login becomes the default host")
	require.NotEmpty(t, cfg.SSHHostKeys, "the host key was TOFU-pinned")

	// ② the session rides every request: clone + commit against the
	// authenticated API face.
	ws := filepath.Join(t.TempDir(), "ws")
	stdout, stderr, code = runCLI(t, "clone", hs.URL+"/r/t98.git", ws)
	require.Zero(t, code, "clone with session failed: %s %s", stdout, stderr)
	support.Write(t, filepath.Join(ws, "new.txt"), "from the CLI\n")
	stdout, stderr, code = runCLI(t, "-C", ws, "commit", "-m", "t98 cli commit")
	require.Zero(t, code, "commit with session failed: %s %s", stdout, stderr)

	// ③ revoke the session server-side: the next pull 401s and re-mints
	// SILENTLY over SSH — the operation completes.
	require.NoError(t, ms.RevokeToken(context.Background(), meta.HashToken(stored.Token)))
	stdout, stderr, code = runCLI(t, "-C", ws, "pull")
	require.Zero(t, code, "pull after revocation should silently re-mint: %s %s", stdout, stderr)
	assert.Contains(t, stdout, "Already up to date")

	// ④ status reports the refreshed session (network-verified), and the
	// host subcommand manages the multi-host view: ls marks the default.
	stdout, stderr, code = runCLI(t, "login", hs.URL, "--status")
	require.Zero(t, code, "%s %s", stdout, stderr)
	assert.Contains(t, stdout, "via ssh")
	assert.Contains(t, stdout, "(valid)")
	stdout, stderr, code = runCLI(t, "host", "ls")
	require.Zero(t, code, "%s %s", stdout, stderr)
	assert.Contains(t, stdout, "* "+hs.URL)
	stdout, stderr, code = runCLI(t, "host", "use", hs.URL)
	require.Zero(t, code, "%s %s", stdout, stderr)
	assert.Contains(t, stdout, "default server: "+hs.URL)

	// ⑤ logout drops the stored credential.
	stdout, stderr, code = runCLI(t, "logout", hs.URL)
	require.Zero(t, code, "%s %s", stdout, stderr)
	stdout, _, code = runCLI(t, "login", hs.URL, "--status")
	assert.Equal(t, 1, code)
	assert.Contains(t, stdout, "not logged in")
}
