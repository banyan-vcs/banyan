// banyand is banyan's server daemon (docs/11 §2): one binary, four roles.
//
//	all-in-one (default)  git smart HTTP + JSON API + local shards + local meta
//	gateway               the stateless frontend: gitwire + rpc over the
//	                      refs service (RemoteMeta) and the scattered store
//	refs                  the serialization point: MetaStore RPC, node health
//	                      table, replica reconciler
//	storage               a pack warehouse: PUT/GET/DELETE by (repo, packOID),
//	                      heartbeating its address to refs
//
// Configuration resolves as defaults < YAML (-config) < explicit flags
// (internal/config).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/banyan-vcs/banyan/internal/config"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/refs"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/storenode"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		log.Fatalf("%v\nusage: banyand [-config banyan.yaml] [-role all-in-one|gateway|refs|storage] [-listen addr] [-root dir] [-meta mem|sqlite] [-db path] [-refs-addr host:port] [-node-id id] [-init-repo name]", err)
	}
	fmt.Fprintf(os.Stderr, "banyand config: %s\n", cfg.Describe())

	switch cfg.Role {
	case "storage":
		runStorage(cfg)
	case "refs":
		runRefs(cfg)
	case "gateway":
		runGateway(cfg)
	default:
		runAllInOne(cfg)
	}
}

// openMeta builds the process-local metadata backend (all-in-one / refs).
func openMeta(cfg *config.Config) storage.MetaStore {
	switch cfg.MetaBackend {
	case "sqlite":
		ms, err := storage.NewSQLiteMetaStore(cfg.DBPath)
		if err != nil {
			log.Fatalf("open sqlite meta store %s: %v", cfg.DBPath, err)
		}
		return ms
	default:
		return storage.NewMemoryMetaStore()
	}
}

func runAllInOne(cfg *config.Config) {
	meta := openMeta(cfg)
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
	root(mux)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

// runGateway is the stateless frontend: every store/metadata call crosses
// the role boundary (docs/11 §2), so any gateway instance is expendable.
// An optional on-disk pack cache (cache.pack_dir, docs/12 P2-1) carries the
// fetched packs across restarts — the one piece of expendable local state
// that meaningfully cuts cold-start traffic.
func runGateway(cfg *config.Config) {
	refsClient := storage.NewRefsClient(cfg.RefsAddr)
	meta := &storage.RemoteMeta{Refs: refsClient}
	backend := storage.NewScatteredStore(refsClient, cfg.DefaultR, cfg.WriteAcks)
	if cfg.Cache.PackDir != "" {
		cache, err := storage.NewDiskPackCache(cfg.Cache.PackDir, int64(cfg.Cache.PackMaxMB)<<20)
		if err != nil {
			log.Fatalf("pack cache: %v", err)
		}
		backend.DiskCache = cache
		entries, bytes := cache.Stats()
		log.Printf("gateway: pack cache %s (%d entries, %.1f MiB)", cfg.Cache.PackDir, entries, float64(bytes)/(1<<20))
	}
	objects := storage.NewObjectServiceWithBackend(meta, backend, model.SHA1)

	if cfg.InitRepo != "" {
		if err := meta.CreateRepo(context.Background(), cfg.InitRepo, model.SHA1); err != nil {
			log.Fatalf("create repo: %v", err)
		}
	}

	mux := http.NewServeMux()
	gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	root(mux)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

// runRefs serves the serialization point: meta RPC, the health table and
// the reconciler (single writer; HA story in docs/11 §4).
func runRefs(cfg *config.Config) {
	meta := openMeta(cfg)
	if cfg.MetaBackend != "sqlite" {
		log.Printf("refs role with meta=%s: metadata is NOT durable across restarts (use -meta sqlite)", cfg.MetaBackend)
	}
	srv := refs.NewServer(meta, refs.Options{
		DefaultReplicas:  cfg.DefaultR,
		WriteAcks:        cfg.WriteAcks,
		SuspectAfter:     time.Duration(cfg.SuspectSec) * time.Second,
		DeadAfter:        time.Duration(cfg.DeadSec) * time.Second,
		ReconInterval:    time.Duration(cfg.ReconSec) * time.Second,
		ReconConcurrency: cfg.ReconCurrency,
	})
	mux := http.NewServeMux()
	srv.Register(mux)
	root(mux)
	srv.RunBackground(context.Background())
	log.Printf("refs: serving %s (default_replicas=%d suspect=%s dead=%s recon=%s×%d)",
		cfg.Listen, cfg.DefaultR, time.Duration(cfg.SuspectSec)*time.Second,
		time.Duration(cfg.DeadSec)*time.Second, time.Duration(cfg.ReconSec)*time.Second, cfg.ReconCurrency)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

// runStorage serves the pack warehouse and heartbeats to refs.
func runStorage(cfg *config.Config) {
	node := storenode.New(cfg.NodeID, cfg.Root, cfg.RefsAddr, cfg.Advertise)
	mux := http.NewServeMux()
	node.Register(mux)
	root(mux)
	go node.HeartbeatLoop(context.Background(), time.Duration(cfg.HeartbeatSec)*time.Second)
	log.Printf("storage %s: serving %s (advertise=%s → refs %s)", cfg.NodeID, cfg.Listen, cfg.Advertise, cfg.RefsAddr)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

func root(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "banyan — endpoints: /r/<repo>[/info/refs|/git-upload-pack|/git-receive-pack], /api/v1/*")
	})
}
