// banyans is banyan's product-side server (docs/17): the console control
// plane above the gateway. It owns users, tokens, the logical repository
// tree ({group}/{project}[/path]) and the member/permission model in its own
// database, fronts all git and management traffic, and hands the gateway a
// signed identity plus a policy snapshot — the gateway keeps the deep
// per-ref enforcement but never sees a credential.
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

func main() {
	listen := flag.String("listen", ":8081", "HTTP listen address")
	root := flag.String("root", "./data", "data root (console.db lives here unless -db)")
	dbPath := flag.String("db", "", "console sqlite path (default <root>/console.db)")
	gateway := flag.String("gateway", "http://127.0.0.1:8080", "gateway (banyand) base URL")
	secret := flag.String("secret", "", "identity-signing secret shared with the gateway (required)")
	bootstrap := flag.String("bootstrap-admin", "", "create this admin user on first boot and print a one-time PAT")
	flag.Parse()

	if *secret == "" {
		log.Fatalf("banyans: -secret is required (it must equal the gateway's auth.upstream_secret)")
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(*root, "console.db")
	}

	db, err := console.Open(*dbPath)
	if err != nil {
		log.Fatalf("banyans: %v", err)
	}
	defer db.Close()

	if *bootstrap != "" {
		bootstrapAdmin(db, *bootstrap)
	}

	srv := console.NewServer(db, *gateway, []byte(*secret))
	mux := http.NewServeMux()
	srv.Register(mux)
	fmt.Fprintf(os.Stderr, "banyans: serving %s (db=%s gateway=%s)\n", *listen, *dbPath, *gateway)
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
		log.Fatalf("banyans: bootstrap %q: %v", username, err)
	}
	secret := console.NewTokenSecret()
	if _, err := db.CreateToken(context.Background(), u.ID, "bootstrap", console.HashToken(secret), 0); err != nil {
		log.Fatalf("banyans: bootstrap token: %v", err)
	}
	fmt.Printf("bootstrap admin %q created.\nPersonal access token (shown once):\n%s\n", username, secret)
}
