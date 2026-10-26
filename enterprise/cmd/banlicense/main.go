// SPDX-License-Identifier: LicenseRef-BANYAN-ENTERPRISE
//
// Enterprise License — see LICENSE.enterprise at the enterprise module
// root: free to modify and test; production use requires a subscription.

// banlicense issues banyan enterprise licenses from a vendor private key
// (Ed25519 PEM). Run from the enterprise module:
//
//	go run ./cmd/banlicense -key vendor.pem -plan enterprise -instance prod-1 -days 365
//
// When -key does not exist a new keypair is generated there (the public
// half lands beside it as <key>.pub) — the first run bootstraps the
// vendor key; keep it offline afterwards.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/banyan-vcs/banyan/enterprise/license"
)

func main() {
	keyPath := flag.String("key", "vendor.pem", "vendor private key (Ed25519 PEM); generated when missing")
	plan := flag.String("plan", "enterprise", "free | team | enterprise")
	instance := flag.String("instance", "*", "licensed deployment instance id (* = any)")
	seats := flag.Int("seats", 0, "licensed seats (0 = unlimited)")
	days := flag.Int("days", 365, "validity in days (0 = non-expiring, dev only)")
	flag.Parse()

	priv, err := loadOrGenerate(*keyPath)
	if err != nil {
		fatal(err)
	}
	p := license.Payload{Plan: license.Plan(*plan), Instance: *instance, Seats: *seats}
	if *days > 0 {
		p.Expires = time.Now().AddDate(0, 0, *days).UTC().Format(time.RFC3339)
	}
	tok, err := license.Issue(priv, p)
	if err != nil {
		fatal(err)
	}
	fmt.Println(tok)
}

func loadOrGenerate(path string) (ed25519.PrivateKey, error) {
	if raw, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(raw)
		if block == nil || block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("%s: not a PEM private key", path)
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an Ed25519 key", path)
		}
		return priv, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(path, privPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path+".pub", pubPEM, 0o644); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "banlicense: generated new vendor keypair %s / %s (keep the private half offline)\n", path, path+".pub")
	return priv, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "banlicense:", err)
	os.Exit(1)
}
