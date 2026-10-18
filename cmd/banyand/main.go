// banyand is banyan's server daemon:
// mounts the git smart HTTP v1 endpoints (gitwire) and the banyan RPC (rpc);
// single-node mode: local-disk shards + swappable metadata backend
// (in-memory or SQLite, docs/02 §2).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	root := flag.String("root", "./data", "local shard storage root")
	repo := flag.String("init-repo", "", "repo name to create at startup (optional)")
	metaBackend := flag.String("meta", "mem", `metadata backend: "mem" (in-memory) or "sqlite" (persistent)`)
	dbPath := flag.String("db", "", "sqlite database path for -meta sqlite (default <root>/banyan.db)")
	flag.Parse()

	var meta storage.MetaStore
	switch *metaBackend {
	case "", "mem":
		meta = storage.NewMemoryMetaStore()
	case "sqlite":
		path := *dbPath
		if path == "" {
			path = filepath.Join(*root, "banyan.db")
		}
		ms, err := storage.NewSQLiteMetaStore(path)
		if err != nil {
			log.Fatalf("open sqlite meta store %s: %v", path, err)
		}
		defer ms.Close()
		meta = ms
		fmt.Fprintf(os.Stderr, "meta store: sqlite (%s)\n", path)
	default:
		log.Fatalf("unknown -meta %q (want mem|sqlite)", *metaBackend)
	}

	local := storage.NewLocalShardStore(*root)
	nodes := []storage.Node{{ID: "local-1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)

	if *repo != "" {
		if err := meta.CreateRepo(context.Background(), *repo, model.SHA1); err != nil {
			log.Fatalf("create repo: %v", err)
		}
		fmt.Fprintf(os.Stderr, "created repo %q (object-format=sha1)\n", *repo)
	}

	mux := http.NewServeMux()
	gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "banyan — endpoints: /r/<repo>[/info/refs|/git-upload-pack|/git-receive-pack], /api/v1/*")
	})

	fmt.Fprintf(os.Stderr, "banyand listening on %s (root=%s, meta=%s)\n", *listen, *root, *metaBackend)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
