// Goal: the git-over-SSH transport (docs/03 §8) end to end with real git
// clients — public-key identity at the auth seam, receive-pack push with
// auto-create, v0 clones with the stateful negotiation rounds (the >48-have
// case forces a mid-negotiation ACK the client blocks on), protocol v2
// command sessions, and the same authorization matrix the HTTP surface
// enforces (T39).
package integration

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// needSSH skips unless the ssh client and ssh-keygen are available (the
// transport is exercised through git's native ssh:// support).
func needSSH(t *testing.T) {
	t.Helper()
	needGit(t)
	for _, bin := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

// newSSHServer boots the stack with the demo policy and the SSH transport
// on an ephemeral port; every user gets a fresh ed25519 key whose public
// half lands in the policy's authorized-keys table.
func newSSHServer(t *testing.T) (url func(user, path string) string, keyEnv func(user string) []string) {
	t.Helper()
	keyDir := t.TempDir()
	keys := map[string][]string{}
	for _, user := range []string{"alice", "bob", "admin"} {
		priv := filepath.Join(keyDir, user)
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "it-"+user, "-f", priv).CombinedOutput()
		require.NoError(t, err, "ssh-keygen: %s", out)
		pub, err := os.ReadFile(priv + ".pub")
		require.NoError(t, err)
		keys[user] = []string{strings.TrimSpace(string(pub))}
	}
	policy := &auth.StaticPolicy{
		Users:             map[string]string{"alice": "a", "bob": "b", "admin": "x"},
		Admins:            []string{"admin"},
		ProtectedBranches: []string{"main"},
		SubtreeAccess:     map[string][]string{"/secret": {"admin"}},
		Keys:              keys,
	}

	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	objects := storage.NewObjectService(meta, local, []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	gw.Auth = policy
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(hostPriv)
	require.NoError(t, err)
	gw.SSHHostKey = signer

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = gw.ServeSSH(ln) }()

	addr := ln.Addr().String()
	url = func(user, path string) string { return fmt.Sprintf("ssh://%s@%s%s", user, addr, path) }
	keyEnv = func(user string) []string {
		// Forward slashes: GIT_SSH_COMMAND is split shell-style and Windows
		// backslash paths lose their separators.
		key := filepath.ToSlash(filepath.Join(keyDir, user))
		sshCmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR", key)
		return []string{"GIT_SSH_COMMAND=" + sshCmd, "GIT_SSH_VARIANT=ssh"}
	}
	return url, keyEnv
}

// sshGit runs git with the user's key; failure is Fatal.
func sshGit(t *testing.T, keyEnv func(string) []string, user, dir string, args ...string) string {
	t.Helper()
	out, err := sshGitTry(keyEnv, user, dir, args...)
	require.NoError(t, err, "git %v (user=%s) 失败\n%s", args, user, out)
	return out
}

func sshGitTry(keyEnv func(string) []string, user, dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "user.name=it", "-c", "user.email=it@banyan.dev",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), keyEnv(user)...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestGitSSH(t *testing.T) {
	needSSH(t)
	url, keyEnv := newSSHServer(t)
	admin := func(p string) string { return url("admin", p) }
	alice := func(p string) string { return url("alice", p) }
	bob := func(p string) string { return url("bob", p) }

	// Seed over SSH: receive-pack session + auto-create (admin may push main).
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, filepath.Join(src, "README.md"), "readme\n")
	write(t, filepath.Join(src, "secret", "plan.txt"), "the plan\n")
	for i := 0; i < 50; i++ {
		appendLine(t, filepath.Join(src, "README.md"), fmt.Sprintf("line %d\n", i))
		git(t, src, "add", "README.md")
		git(t, src, "commit", "-qm", fmt.Sprintf("c%d", i))
	}
	git(t, src, "add", "-A")
	sshGit(t, keyEnv, "admin", src, "push", "-q", admin("/r/t39.git"), "main")

	// v0 clone (stateful transport; the 51-commit history keeps the client's
	// have negotiation well past its first 16-have batch).
	home := t.TempDir()
	sshGit(t, keyEnv, "alice", "", "clone", "-q", "-c", "protocol.version=0", alice("/r/t39.git"), home)
	assert.Equal(t, "readme\n", readFile(t, filepath.Join(home, "README.md"))[:7])

	// v0 incremental fetch over SSH — the stateful negotiation rounds: the
	// client blocks reading an ACK after its second have batch (48+ haves in
	// flight); the server must answer each flush (inline terminal ACK model,
	// upload-pack.c get_common_commits).
	appendLine(t, filepath.Join(src, "README.md"), "later\n")
	git(t, src, "commit", "-qam", "later")
	sshGit(t, keyEnv, "admin", src, "push", "-q", admin("/r/t39.git"), "main")
	sshGit(t, keyEnv, "alice", home, "-c", "protocol.version=0", "fetch", "-q", "origin")
	sshGit(t, keyEnv, "alice", home, "merge", "-q", "--ff-only", "origin/main")
	assert.Contains(t, readFile(t, filepath.Join(home, "README.md")), "later\n", "SSH v0 增量 fetch 后内容应推进")

	// Protocol v2 over SSH: env GIT_PROTOCOL=version=2 (SendEnv), one
	// advertisement then a command loop (serve.c).
	lsV2 := sshGit(t, keyEnv, "bob", "", "-c", "protocol.version=2", "ls-remote", "--heads", bob("/r/t39.git"))
	assert.Equal(t, 1, strings.Count(lsV2, "\n"), "v2 ls-refs over ssh 应只见 main:\n%s", lsV2)
	v2home := t.TempDir()
	sshGit(t, keyEnv, "alice", "", "-c", "protocol.version=2", "clone", "-q", alice("/r/t39.git"), v2home)

	// Personal branches over SSH: same visibility decisions (the identity
	// comes from the key, the rules from the same Service).
	git(t, src, "checkout", "-q", "-b", "alice/wip")
	appendLine(t, filepath.Join(src, "README.md"), "wip\n")
	git(t, src, "commit", "-qam", "wip")
	sshGit(t, keyEnv, "alice", src, "push", "-q", alice("/r/t39.git"), "alice/wip")
	lsAlice := sshGit(t, keyEnv, "alice", "", "ls-remote", "--heads", alice("/r/t39.git"))
	assert.Contains(t, lsAlice, "alice/wip", "本人应看到个人分支")
	lsBob := sshGit(t, keyEnv, "bob", "", "ls-remote", "--heads", bob("/r/t39.git"))
	assert.NotContains(t, lsBob, "alice/wip", "他人不应看到个人分支")
	lsAdmin := sshGit(t, keyEnv, "admin", "", "ls-remote", "--heads", admin("/r/t39.git"))
	assert.Contains(t, lsAdmin, "alice/wip", "admin 全可见")

	// Push guards: someone else's namespace and the protected branch.
	git(t, src, "checkout", "-q", "-b", "alice/evil")
	out, err := sshGitTry(keyEnv, "bob", src, "push", "-q", bob("/r/t39.git"), "alice/evil:alice/evil")
	require.Error(t, err, "bob 不能推 alice 命名空间:\n%s", out)
	assert.Contains(t, out, "permission denied")
	git(t, src, "checkout", "-q", "main")
	appendLine(t, filepath.Join(src, "README.md"), "dev\n")
	git(t, src, "commit", "-qam", "dev on main")
	out, err = sshGitTry(keyEnv, "alice", src, "push", "-q", alice("/r/t39.git"), "main")
	require.Error(t, err, "保护分支仅 admin:\n%s", out)

	// Directory granularity: /secret is admin-only over SSH too.
	secAdmin := t.TempDir()
	sshGit(t, keyEnv, "admin", "", "clone", "-q", admin("/r/t39.git/tree/secret"), secAdmin)
	assert.Contains(t, readFile(t, filepath.Join(secAdmin, "plan.txt")), "the plan")
	out, err = sshGitTry(keyEnv, "bob", "", "clone", "-q", bob("/r/t39.git/tree/secret"), t.TempDir())
	require.Error(t, err, "受限目录拒绝:\n%s", out)
	assert.Contains(t, out, "permission denied")

	// An unauthorized key authenticates as nobody: the connection is
	// refused before any git traffic.
	stray := t.TempDir()
	if out2, err2 := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(stray, "id")).CombinedOutput(); err2 != nil {
		t.Fatalf("ssh-keygen: %s", out2)
	}
	strayEnv := func(string) []string {
		key := filepath.ToSlash(filepath.Join(stray, "id"))
		return []string{"GIT_SSH_COMMAND=" + fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR", key), "GIT_SSH_VARIANT=ssh"}
	}
	out, err = sshGitTry(strayEnv, "alice", "", "ls-remote", alice("/r/t39.git"))
	require.Error(t, err, "未授权密钥应被拒:\n%s", out)
	assert.Contains(t, strings.ToLower(out), "permission denied")
}
