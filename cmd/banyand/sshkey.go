package main

// Host key management for the git-over-SSH transport (docs/04 §8): the key
// signs the server side of the SSH handshake, so it must persist across
// restarts (a new key every boot would break every client's known_hosts).
// Missing key files are generated on first boot and left on disk.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"

	"github.com/banyan-vcs/banyan/internal/gitwire"
)

// loadOrCreateHostKey returns the host key signer, generating and
// persisting an ed25519 key at path when the file is absent.
func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if raw, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(raw)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "banyand ssh host key")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	log.Printf("ssh: generated host key %s (fingerprint %s)", path, ssh.FingerprintSHA256(signer.PublicKey()))
	return signer, nil
}

// startSSH brings up the git-over-SSH listener on a gitwire server when
// ssh.listen is configured (no-op without it). ssh.host_key defaults to
// <root>/ssh_host_ed25519_key. The transport requires the auth policy —
// an SSH listener without a key table cannot authenticate anyone, so a
// configured listen without auth is refused outright.
func startSSH(listen, hostKey string, gw *gitwire.Server) error {
	if listen == "" {
		return nil
	}
	if gw.Auth == nil {
		return fmt.Errorf("ssh.listen requires an auth policy (no key table without one)")
	}
	signer, err := loadOrCreateHostKey(hostKey)
	if err != nil {
		return fmt.Errorf("ssh host key: %w", err)
	}
	gw.SSHHostKey = signer
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	log.Printf("ssh: git transport on %s (host key %s)", listen, hostKey)
	go func() {
		if err := gw.ServeSSH(ln); err != nil {
			log.Printf("ssh: listener ended: %v", err)
		}
	}()
	return nil
}

// sshHostKeyPath resolves the configured host key, defaulting into the
// data root so the generated key survives restarts alongside it.
func sshHostKeyPath(cfgHostKey, root string) string {
	if cfgHostKey != "" {
		return cfgHostKey
	}
	return filepath.Join(root, "ssh_host_ed25519_key")
}
