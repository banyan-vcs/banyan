// banyand is banyan's server daemon:
// mounts the git smart HTTP v1 endpoints (gitwire) and the banyan RPC (rpc);
// single-node mode: local-disk shards + swappable metadata backend
// (in-memory or SQLite, docs/02 §2). Configuration resolves as
// defaults < YAML file (-config) < explicit flags (internal/config).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/banyan-vcs/banyan/internal/config"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		log.Fatalf("%v\nusage: banyand [-config banyan.yaml] [-listen addr] [-root dir] [-meta mem|sqlite] [-db path] [-init-repo name]", err)
	}

	var meta storage.MetaStore
	switch cfg.MetaBackend {
	case "mem":
		meta = storage.NewMemoryMetaStore()
	case "sqlite":
		ms, err := storage.NewSQLiteMetaStore(cfg.DBPath)
		if err != nil {
			log.Fatalf("open sqlite meta store %s: %v", cfg.DBPath, err)
		}
		defer ms.Close()
		meta = ms
	default:
		log.Fatalf("unknown meta backend %q", cfg.MetaBackend)
	}

	local := storage.NewLocalShardStore(cfg.Root)
	nodes := []storage.Node{{ID: "local-1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)

	if cfg.InitRepo != "" {
		if err := meta.CreateRepo(context.Background(), cfg.InitRepo, model.SHA1); err != nil {
			log.Fatalf("create repo: %v", err)
		}
		fmt.Fprintf(os.Stderr, "created repo %q (object-format=sha1)\n", cfg.InitRepo)
	}

	mux := http.NewServeMux()
	gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "banyan — endpoints: /r/<repo>[/info/refs|/git-upload-pack|/git-receive-pack], /api/v1/*")
	})

	fmt.Fprintf(os.Stderr, "banyand config: %s\n", cfg.Describe())
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}
