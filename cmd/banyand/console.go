// console.go — `banyand console`: the product-side control plane (docs/web/console.md),
// formerly the standalone banyans binary. It owns users, tokens, the logical
// repository tree ({group}/{project}[/path]) and the member/permission model
// in its own database, fronts all git and management traffic, and hands the
// gateway a signed identity plus a policy snapshot — the gateway keeps the
// deep per-ref enforcement but never sees a credential.
//
// Bootstrap: the first admin comes from -bootstrap-admin <username>, which
// creates the user and prints a one-time PAT to stdout.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/banyan-vcs/banyan/internal/console"
)

type consoleOptions struct {
	Listen    string
	Root      string
	DBPath    string
	Gateway   string
	Secret    string
	Bootstrap string
}

func newConsoleCmd() *cobra.Command {
	var o consoleOptions
	cmd := &cobra.Command{
		Use:   "console",
		Short: "the product-side control plane: users, tokens, the repository tree",
		RunE: func(_ *cobra.Command, _ []string) error {
			runConsole(o)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&o.Listen, "listen", ":8081", "HTTP listen address")
	fl.StringVar(&o.Root, "root", "./data", "data root (console.db lives here unless -db)")
	fl.StringVar(&o.DBPath, "db", "", "console sqlite path (default <root>/console.db)")
	fl.StringVar(&o.Gateway, "gateway", "http://127.0.0.1:8080", "gateway (banyand) base URL")
	fl.StringVar(&o.Secret, "secret", "", "identity-signing secret shared with the gateway (required)")
	fl.StringVar(&o.Bootstrap, "bootstrap-admin", "", "create this admin user on first boot and print a one-time PAT")
	return cmd
}

func runConsole(o consoleOptions) {
	if o.Secret == "" {
		log.Fatalf("banyand console: -secret is required (it must equal the gateway's auth.upstream_secret)")
	}
	if o.DBPath == "" {
		o.DBPath = filepath.Join(o.Root, "console.db")
	}

	db, err := console.Open(o.DBPath)
	if err != nil {
		log.Fatalf("banyand console: %v", err)
	}
	defer db.Close()

	if o.Bootstrap != "" {
		bootstrapAdmin(db, o.Bootstrap)
	}

	srv := console.NewServer(db, o.Gateway, []byte(o.Secret))
	mux := http.NewServeMux()
	srv.Register(mux)
	fmt.Fprintf(os.Stderr, "banyand console: serving %s (db=%s gateway=%s)\n", o.Listen, o.DBPath, o.Gateway)
	log.Fatal(http.ListenAndServe(o.Listen, mux))
}

// bootstrapAdmin creates the named admin when missing and prints a fresh PAT
// exactly once. On an existing user it is a no-op — bootstrap never resets.
func bootstrapAdmin(db *console.DB, username string) {
	if _, err := db.UserByName(context.Background(), username); err == nil {
		return
	}
	u, err := db.CreateUser(context.Background(), username, true)
	if err != nil {
		log.Fatalf("banyand console: bootstrap %q: %v", username, err)
	}
	secret := console.NewTokenSecret()
	if _, err := db.CreateToken(context.Background(), u.ID, "bootstrap", console.HashToken(secret), 0); err != nil {
		log.Fatalf("banyand console: bootstrap token: %v", err)
	}
	fmt.Printf("bootstrap admin %q created.\nPersonal access token (shown once):\n%s\n", username, secret)
}
