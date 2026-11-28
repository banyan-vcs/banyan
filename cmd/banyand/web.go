// web.go — `banyand web`: the web tier (internal/webapp) — sessions in its
// own database (sqlite/mysql/postgres, optionally the SAME database as the
// gateway), local + OAuth login, the SPA, the /api/v1 proxy and the import
// orchestration. Flags cover humans; the env fallbacks are the container
// spellings the compose files and the GitLab demo provisioning already
// write (PORT, BANYAN_URL, BASE_PATH, DIST_DIR, BANYAN_WEB_DB,
// BANYAN_WEB_PROFILE) — they apply AFTER the parse, so the environment
// wins over the flag, exactly as before the cobra port.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/banyan-vcs/banyan/internal/webapp"
)

func envDefault(flagVal, envKey string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return flagVal
}

type webOptions struct {
	Listen   string
	Gateway  string
	DBSpec   string
	Dist     string
	BasePath string
	Profile  string
}

func newWebCmd() *cobra.Command {
	var o webOptions
	cmd := &cobra.Command{
		Use:   "web",
		Short: "the web tier (internal/webapp): sessions, OAuth login, the SPA",
		RunE: func(_ *cobra.Command, _ []string) error {
			// PORT holds a bare port number in container-land.
			if p := os.Getenv("PORT"); p != "" {
				o.Listen = ":" + p
			}
			o.Gateway = envDefault(o.Gateway, "BANYAN_URL")
			o.Dist = envDefault(o.Dist, "DIST_DIR")
			o.BasePath = envDefault(o.BasePath, "BASE_PATH")
			o.DBSpec = envDefault(o.DBSpec, "BANYAN_WEB_DB")
			o.Profile = envDefault(o.Profile, "BANYAN_WEB_PROFILE")
			runWeb(o)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&o.Listen, "listen", ":3000", "HTTP listen address")
	fl.StringVar(&o.Gateway, "gateway", "http://127.0.0.1:8080", "gateway (banyand serve) base URL")
	fl.StringVar(&o.DBSpec, "db", "sqlite:./web/web.db", "web database: sqlite:<path> | mysql:<dsn> | postgres:<dsn> (a bare path is sqlite)")
	fl.StringVar(&o.Dist, "dist", "./web/dist", "the SPA build directory")
	fl.StringVar(&o.BasePath, "base-path", "", "URL prefix without trailing slash (e.g. /-/banyan behind GitLab nginx)")
	fl.StringVar(&o.Profile, "profile", webapp.ProfileStandalone, webapp.ProfileStandalone+" | "+webapp.ProfileGitLab)
	return cmd
}

func runWeb(o webOptions) {
	db, err := webapp.OpenDB(o.DBSpec)
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
		Gateway:  o.Gateway,
		BasePath: o.BasePath,
		DistDir:  filepath.FromSlash(o.Dist),
		Profile:  o.Profile,
		DB:       db,
	})
	mux := http.NewServeMux()
	srv.Register(mux)
	fmt.Fprintf(os.Stderr, "banyand web: serving %s (gateway=%s db=%s profile=%s base=%q)\n",
		o.Listen, o.Gateway, o.DBSpec, o.Profile, o.BasePath)
	log.Fatal(http.ListenAndServe(o.Listen, mux))
}
