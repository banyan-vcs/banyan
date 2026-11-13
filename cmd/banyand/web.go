// web.go — `banyand web`: the web tier (internal/webapp) — sessions in its
// own database (sqlite/mysql/postgres, optionally the SAME database as the
// gateway), local + OAuth login, the SPA, the /api/v1 proxy and the import
// orchestration. Flags cover humans; the env fallbacks are the container
// spellings the compose files and the GitLab demo provisioning already
// write (PORT, BANYAN_URL, BASE_PATH, DIST_DIR, BANYAN_WEB_DB,
// BANYAN_WEB_PROFILE).
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/banyan-vcs/banyan/internal/webapp"
)

func envDefault(flagVal, envKey string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return flagVal
}

func runWeb(args []string) {
	fs := flag.NewFlagSet("banyand web", flag.ExitOnError)
	listen := fs.String("listen", ":3000", "HTTP listen address")
	gateway := fs.String("gateway", "http://127.0.0.1:8080", "gateway (banyand serve) base URL")
	dbSpec := fs.String("db", "sqlite:./web/web.db", "web database: sqlite:<path> | mysql:<dsn> | postgres:<dsn> (a bare path is sqlite)")
	dist := fs.String("dist", "./web/dist", "the SPA build directory")
	basePath := fs.String("base-path", "", "URL prefix without trailing slash (e.g. /-/banyan behind GitLab nginx)")
	profile := fs.String("profile", webapp.ProfileStandalone, webapp.ProfileStandalone+" | "+webapp.ProfileGitLab)
	fs.Parse(args)

	// PORT holds a bare port number in container-land.
	if p := os.Getenv("PORT"); p != "" {
		*listen = ":" + p
	}
	*gateway = envDefault(*gateway, "BANYAN_URL")
	*dist = envDefault(*dist, "DIST_DIR")
	*basePath = envDefault(*basePath, "BASE_PATH")
	*dbSpec = envDefault(*dbSpec, "BANYAN_WEB_DB")
	*profile = envDefault(*profile, "BANYAN_WEB_PROFILE")

	db, err := webapp.OpenDB(*dbSpec)
	if err != nil {
		log.Fatalf("banyand web: %v", err)
	}
	defer db.Close()
	if err := db.FailInterruptedImports(); err != nil {
		log.Printf("banyand web: import sweep: %v", err)
	}
	if err := db.SweepExpired(); err != nil {
		log.Printf("banyand web: session sweep: %v", err)
	}
	_ = db.SweepOAuthStates()

	srv := webapp.New(webapp.Options{
		Gateway:  *gateway,
		BasePath: *basePath,
		DistDir:  filepath.FromSlash(*dist),
		Profile:  *profile,
		DB:       db,
	})
	mux := http.NewServeMux()
	srv.Register(mux)
	fmt.Fprintf(os.Stderr, "banyand web: serving %s (gateway=%s db=%s profile=%s base=%q)\n",
		*listen, *gateway, *dbSpec, *profile, *basePath)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
