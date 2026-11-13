// console.go — `banyand console`: the product-side control plane (docs/17),
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
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/banyan-vcs/banyan/internal/console"
)

func runConsole(args []string) {
	fs := flag.NewFlagSet("banyand console", flag.ExitOnError)
	listen := fs.String("listen", ":8081", "HTTP listen address")
	root := fs.String("root", "./data", "data root (console.db lives here unless -db)")
	dbPath := fs.String("db", "", "console sqlite path (default <root>/console.db)")
	gateway := fs.String("gateway", "http://127.0.0.1:8080", "gateway (banyand) base URL")
	secret := fs.String("secret", "", "identity-signing secret shared with the gateway (required)")
	bootstrap := fs.String("bootstrap-admin", "", "create this admin user on first boot and print a one-time PAT")
	fs.Parse(args)

	if *secret == "" {
		log.Fatalf("banyand console: -secret is required (it must equal the gateway's auth.upstream_secret)")
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(*root, "console.db")
	}

	db, err := console.Open(*dbPath)
	if err != nil {
		log.Fatalf("banyand console: %v", err)
	}
	defer db.Close()

	if *bootstrap != "" {
		bootstrapAdmin(db, *bootstrap)
	}

	srv := console.NewServer(db, *gateway, []byte(*secret))
	mux := http.NewServeMux()
	srv.Register(mux)
	fmt.Fprintf(os.Stderr, "banyand console: serving %s (db=%s gateway=%s)\n", *listen, *dbPath, *gateway)
	log.Fatal(http.ListenAndServe(*listen, mux))
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
