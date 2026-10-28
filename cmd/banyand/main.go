// banyand is banyan's server daemon (docs/10 §2): one binary, four roles.
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

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/config"
	"github.com/banyan-vcs/banyan/internal/debug"
	"github.com/banyan-vcs/banyan/internal/gitaly"
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
	// Ops plane (docs/05 §6): pprof family + runtime snapshot + expvar
	// counters on an auxiliary listener, off unless debug.pprof_addr is set.
	// Started here so every role gets it.
	debug.Start(debug.Options{
		Addr:                 cfg.Debug.PprofAddr,
		MutexProfileFraction: cfg.Debug.MutexProfileFraction,
		BlockProfileRateNs:   cfg.Debug.BlockProfileRateNs,
	})

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
	case "mysql", "postgres":
		ms, err := storage.NewSQLMetaStore(cfg.MetaBackend, cfg.DBPath)
		if err != nil {
			log.Fatalf("open %s meta store: %v", cfg.MetaBackend, err)
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
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	api := rpc.NewServer(meta, objects, model.SHA1)
	objects.Addressing = storage.NewAddressing(cfg.Storage.Mode, meta)
	gw.Convergence = cfg.Workflow.Convergence == "auto"
	gw.FullCloneMaxBytes = cfg.FullCloneMaxBytes
	switch {
	case cfg.Auth.Upstream != "":
		// Console-fronted mode (docs/17 §5): the banyans control plane
		// authenticates and vouches for callers; rules sync as snapshot v2.
		up := auth.NewUpstreamPolicy(cfg.Auth.Upstream, []byte(cfg.Auth.UpstreamSecret))
		gw.Auth, api.Auth = up, up
		log.Printf("auth: upstream console %s (snapshot v2 + signed identity)", cfg.Auth.Upstream)
	case staticPolicy(cfg) != nil:
		policy := staticPolicy(cfg)
		gw.Auth = policy
		api.Auth = policy
		log.Printf("auth: enabled (%d users, %d admins, protected %v)",
			len(policy.Users), len(policy.Admins), policy.ProtectedBranches)
	}
	gw.Register(mux)
	api.Register(mux)
	root(mux)
	startGitaly(cfg, meta, objects)
	if err := startSSH(cfg.SSH.Listen, sshHostKeyPath(cfg.SSH.HostKey, cfg.Root), gw); err != nil {
		log.Fatalf("ssh: %v", err)
	}
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

// startGitaly serves the gitaly-protocol gRPC frontend (docs/19) when
// gitaly.listen is configured: a GitLab Rails/workhorse stack can replace
// its stock gitaly node with this process.
func startGitaly(cfg *config.Config, meta storage.MetaStore, objects *storage.ObjectService) {
	if cfg.Gitaly.Listen == "" {
		return
	}
	if cfg.Gitaly.Token == "" {
		log.Printf("gitaly: WARNING no token set — the frontend accepts unauthenticated gRPC (development only)")
	}
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{
		Token:           cfg.Gitaly.Token,
		StorageName:     cfg.Gitaly.StorageName,
		ReportedVersion: cfg.Gitaly.ReportedVersion,
	})
	go func() {
		log.Printf("gitaly: serving %s (storage=%s, protocol %s)", cfg.Gitaly.Listen, cfg.Gitaly.StorageName, gitaly.ProtoVersion)
		if err := gs.ListenAndServe(cfg.Gitaly.Listen); err != nil {
			log.Fatalf("gitaly: %v", err)
		}
	}()
}
// staticPolicy builds the demo authorization policy from config (docs/07
// §2): nil when the auth section is absent — authorization off.
func staticPolicy(cfg *config.Config) *auth.StaticPolicy {
	if len(cfg.Auth.Users) == 0 {
		return nil
	}
	return &auth.StaticPolicy{
		Users:             cfg.Auth.Users,
		Admins:            cfg.Auth.Admins,
		ProtectedBranches: cfg.Auth.ProtectedBranches,
		SubtreeAccess:     cfg.Auth.SubtreeAccess,
		Keys:              cfg.Auth.Keys,
	}
}

// runGateway is the stateless frontend: every store/metadata call crosses
// the role boundary (docs/10 §2), so any gateway instance is expendable.
// An optional on-disk pack cache (cache.pack_dir, docs/11 P2-1) carries the
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
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	api := rpc.NewServer(meta, objects, model.SHA1)
	objects.Addressing = storage.NewAddressing(cfg.Storage.Mode, meta)
	gw.Convergence = cfg.Workflow.Convergence == "auto"
	gw.FullCloneMaxBytes = cfg.FullCloneMaxBytes
	switch {
	case cfg.Auth.Upstream != "":
		// Console-fronted mode on the gateway role (docs/17 §5): same
		// upstream policy as all-in-one — the console fronts both git and
		// management traffic; this process only accepts signed identities.
		up := auth.NewUpstreamPolicy(cfg.Auth.Upstream, []byte(cfg.Auth.UpstreamSecret))
		gw.Auth, api.Auth = up, up
		log.Printf("auth: upstream console %s (snapshot v2 + signed identity)", cfg.Auth.Upstream)
	case cfg.Auth.Remote:
		// The gateway decides against a policy snapshot synced from the
		// refs service (docs/07 §4) — same abstraction, synced state; the
		// concrete rules live on refs, never duplicated here.
		policy := auth.NewRemotePolicy(cfg.RefsAddr)
		gw.Auth = policy
		api.Auth = policy
		log.Printf("auth: remote policy from %s", cfg.RefsAddr)
	}
	gw.Register(mux)
	api.Register(mux)
	root(mux)
	startGitaly(cfg, meta, objects)
	// Same transport wiring as all-in-one: the gateway decides access
	// against the synced policy snapshot (keys ride along, docs/07 §4/§7).
	if err := startSSH(cfg.SSH.Listen, sshHostKeyPath(cfg.SSH.HostKey, cfg.Root), gw); err != nil {
		log.Fatalf("ssh: %v", err)
	}
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

// runRefs serves the serialization point: meta RPC, the health table and
// the reconciler (single writer; HA story in docs/10 §4).
func runRefs(cfg *config.Config) {
	meta := openMeta(cfg)
	if cfg.MetaBackend == "mem" {
		log.Printf("refs role with meta=%s: metadata is NOT durable across restarts (use -meta sqlite/mysql/postgres)", cfg.MetaBackend)
	}
	srv := refs.NewServer(meta, refs.Options{
		DefaultReplicas:  cfg.DefaultR,
		WriteAcks:        cfg.WriteAcks,
		SuspectAfter:     time.Duration(cfg.SuspectSec) * time.Second,
		DeadAfter:        time.Duration(cfg.DeadSec) * time.Second,
		ReconInterval:    time.Duration(cfg.ReconSec) * time.Second,
		ReconConcurrency: cfg.ReconCurrency,
		AddressingMode:   cfg.Storage.Mode,
	})
	srv.Policy = staticPolicy(cfg)
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
