// banyand is banyan's server daemon (docs/storage/ha.md §2): one binary, four serving
// roles plus the control-plane subcommands — every server-side job ships in
// this one process.
//
//	banyand [serve]    the storage-plane roles (below); bare flags are the
//	                    historical spelling and stay accepted
//	    all-in-one (default)  git smart HTTP + JSON API + local shards + local meta
//	    gateway               the stateless frontend: gitwire + rpc over the
//	                          refs service (meta.RemoteMeta) and the scattered store
//	    refs                  the serialization point: meta.MetaStore RPC, node health
//	                          table, replica reconciler
//	    storage               a pack warehouse: PUT/GET/DELETE by (repo, packOID),
//	                          heartbeating its address to refs
//	banyand console    the product-side control plane (formerly the banyans
//	                    binary, docs/web/console.md): users, tokens, the logical repository
//	                    tree, the git front door
//	banyand bench      the performance harness (docs/operations/performance.md §3, formerly banyan-bench)
//	banyand web        the web tier (internal/webapp): sessions, OAuth login,
//	                    repo import, the SPA — the standalone control plane
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
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/config"
	"github.com/banyan-vcs/banyan/internal/debug"
	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/metrics"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/refs"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/storenode"
)

func main() {
	// Subcommand dispatch: a known leading word picks a control-plane
	// subcommand; anything else (a flag, nothing) is the serving path —
	// `banyand -config x` keeps working exactly as before.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		case "console":
			runConsole(os.Args[2:])
			return
		case "bench":
			runBench(os.Args[2:])
			return
		case "web":
			runWeb(os.Args[2:])
			return
		}
	}
	runServe()
}

func runServe() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		log.Fatalf("%v\nusage: banyand [-config banyan.yaml] [-role all-in-one|gateway|refs|storage] [-listen addr] [-root dir] [-meta mem|sqlite] [-db path] [-refs-addr host:port] [-node-id id] [-init-repo name] [-dev]", err)
	}
	fmt.Fprintf(os.Stderr, "banyand config: %s\n", cfg.Describe())
	// Ops plane (docs/gateway/service-api.md §6): pprof family + runtime snapshot + expvar
	// counters on an auxiliary listener, off unless debug.pprof_addr is set.
	// Started here so every role gets it.
	debug.Start(debug.Options{
		Addr:                 cfg.Debug.PprofAddr,
		MutexProfileFraction: cfg.Debug.MutexProfileFraction,
		BlockProfileRateNs:   cfg.Debug.BlockProfileRateNs,
	})

	// One termination context for the whole process: SIGINT/SIGTERM cancels
	// it, every role drains (in-flight pack streams finish) instead of
	// dying mid-transfer — the posture a rolling restart needs.
	ctx, stop := signalContext()
	defer stop()

	switch cfg.Role {
	case "storage":
		runStorage(ctx, cfg)
	case "refs":
		runRefs(ctx, cfg)
	case "gateway":
		runGateway(ctx, cfg)
	default:
		runAllInOne(ctx, cfg)
	}
}

// openMeta builds the process-local metadata backend (all-in-one / refs).
func openMeta(cfg *config.Config) meta.MetaStore {
	switch cfg.MetaBackend {
	case "sqlite":
		ms, err := meta.NewSQLiteMetaStore(cfg.DBPath)
		if err != nil {
			log.Fatalf("open sqlite meta store %s: %v", cfg.DBPath, err)
		}
		return ms
	case "mysql", "postgres":
		ms, err := meta.NewSQLMetaStore(cfg.MetaBackend, cfg.DBPath)
		if err != nil {
			log.Fatalf("open %s meta store: %v", cfg.MetaBackend, err)
		}
		return ms
	default:
		return meta.NewMemoryMetaStore()
	}
}

func runAllInOne(ctx context.Context, cfg *config.Config) {
	ms := openMeta(cfg)
	local := storage.NewLocalShardStore(cfg.Root)
	nodes := []storage.Node{{ID: "local-1", Local: true}}
	objects := storage.NewObjectService(ms, local, nodes, model.SHA1)

	if cfg.InitRepo != "" {
		if err := ms.CreateRepo(context.Background(), cfg.InitRepo, model.SHA1); err != nil {
			log.Fatalf("create repo: %v", err)
		}
		fmt.Fprintf(os.Stderr, "created repo %q (object-format=sha1)\n", cfg.InitRepo)
	}

	mux := http.NewServeMux()
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	api := rpc.NewServer(ms, objects, model.SHA1)
	objects.Addressing = meta.NewAddressing(cfg.Storage.Mode, ms)
	gw.Convergence = cfg.Workflow.Convergence == "auto"
	gw.FullCloneMaxBytes = cfg.FullCloneMaxBytes
	switch {
	case cfg.Auth.Upstream != "":
		// Console-fronted mode (docs/web/console.md §5): the console (banyand console) control plane
		// authenticates and vouches for callers; rules sync as snapshot v2.
		up := auth.NewUpstreamPolicy(cfg.Auth.Upstream, []byte(cfg.Auth.UpstreamSecret))
		gw.Auth, api.Auth = up, up
		log.Printf("auth: upstream console %s (snapshot v2 + signed identity)", cfg.Auth.Upstream)
	case cfg.Auth.GitLabSSO != "" || len(cfg.Auth.OAuth) > 0:
		// Token-fronted mode (docs/integration/gitlab-integration.md, docs/gateway/authorization.md §8d): tokens minted by the
		// configured identity provider(s) — GitLab instances (OAuth2/PAT),
		// github.com or a GitHub Enterprise host — authenticate the HTTP
		// face. A single GitLab instance rides the original GitLabSSO
		// policy; anything more uses the multi-provider policy.
		providers := make([]auth.SSOProvider, 0, len(cfg.Auth.OAuth)+1)
		if cfg.Auth.GitLabSSO != "" {
			providers = append(providers, auth.SSOProvider{ID: "gitlab", Kind: auth.SSOKindGitLab, URL: cfg.Auth.GitLabSSO})
		}
		providers = append(providers, toSSOProviders(cfg)...)
		var svc auth.Service
		if len(providers) == 1 && providers[0].Kind == auth.SSOKindGitLab {
			svc = auth.NewGitLabSSO(providers[0].URL)
			log.Printf("auth: gitlab sso %s (token → /api/v4/user)", providers[0].URL)
		} else {
			svc = &auth.MultiSSO{Providers: providers, Admins: cfg.Auth.Admins}
			ids := make([]string, len(providers))
			for i, p := range providers {
				ids[i] = p.ID + "(" + p.Kind + ")"
			}
			log.Printf("auth: oauth providers %s (token → each provider's /user, in order)", strings.Join(ids, ", "))
		}
		if rs, ok := ms.(meta.SubtreeRuleStore); ok {
			svc = &auth.SubtreeRulePolicy{Inner: svc, Rules: rs}
			log.Printf("auth: subtree rules on (runtime directory access rules, docs/gateway/authorization.md §8)")
		}
		gw.Auth = svc
		api.Auth = svc
	case staticPolicy(cfg) != nil:
		policy := staticPolicy(cfg)
		var svc auth.Service = policy
		// Metadata-plane PATs ride on top of the static policy (docs/gateway/authorization.md
		// §6): passwords and SSH keys keep working, tokens add scoped
		// credentials (read/write/admin) — the decorator composes at the
		// same Service seam the wire layers already enforce through.
		if tr, ok := ms.(meta.TokenRegistry); ok {
			svc = &auth.TokenPolicy{Inner: policy, Tokens: tr}
			log.Printf("auth: token layer on (personal access tokens, scopes read/write/admin)")
		}
		// Runtime directory access rules narrow the whole stack (docs/gateway/authorization.md
		// §8): the YAML subtree_access seed stays boot-time only.
		if rs, ok := ms.(meta.SubtreeRuleStore); ok {
			svc = &auth.SubtreeRulePolicy{Inner: svc, Rules: rs}
			log.Printf("auth: subtree rules on (runtime directory access rules, docs/gateway/authorization.md §8)")
		}
		gw.Auth = svc
		api.Auth = svc
		log.Printf("auth: enabled (%d users, %d admins, protected %v)",
			len(policy.Users), len(policy.Admins), policy.ProtectedBranches)
	}
	gw.Register(mux)
	api.Register(mux)
	root(mux)
	gs := startGitaly(cfg, ms, objects)
	stopSSH, err := startSSH(cfg.SSH.Listen, sshHostKeyPath(cfg.SSH.HostKey, cfg.Root), gw)
	if err != nil {
		log.Fatalf("ssh: %v", err)
	}
	if err := serveHTTP(ctx, cfg.Listen, mux); err != nil {
		log.Fatalf("http: %v", err)
	}
	stopSSH()
	if gs != nil {
		gs.GracefulStop() // in-flight gRPC streams drain after the HTTP face
	}
}

// toSSOProviders maps the config's auth.oauth list onto auth providers.
func toSSOProviders(cfg *config.Config) []auth.SSOProvider {
	out := make([]auth.SSOProvider, 0, len(cfg.Auth.OAuth))
	for _, p := range cfg.Auth.OAuth {
		out = append(out, auth.SSOProvider{ID: p.ID, Kind: p.Kind, URL: p.URL, Admins: p.Admins})
	}
	return out
}

// startGitaly serves the gitaly-protocol gRPC frontend (docs/integration/gitaly-compat.md) when
// gitaly.listen is configured: a GitLab Rails/workhorse stack can replace
// its stock gitaly node with this process. Returns the server (nil when the
// frontend is off) so the caller can drain it at shutdown.
func startGitaly(cfg *config.Config, ms meta.MetaStore, objects *storage.ObjectService) *gitaly.Server {
	if cfg.Gitaly.Listen == "" {
		return nil
	}
	if cfg.Gitaly.Token == "" {
		log.Printf("gitaly: WARNING no token set — the frontend accepts unauthenticated gRPC (dev mode)")
	}
	gs := gitaly.NewServer(ms, objects, model.SHA1, gitaly.Options{
		Token:            cfg.Gitaly.Token,
		StorageName:      cfg.Gitaly.StorageName,
		ReportedVersion:  cfg.Gitaly.ReportedVersion,
		Convergence:      cfg.Workflow.Convergence == "auto",
		FullCloneMaxByte: cfg.FullCloneMaxBytes,
		GitLab:           gitLabAdapter(cfg),
	})
	go func() {
		log.Printf("gitaly: serving %s (storage=%s, protocol %s)", cfg.Gitaly.Listen, cfg.Gitaly.StorageName, gitaly.ProtoVersion)
		if err := gs.ListenAndServe(cfg.Gitaly.Listen); err != nil {
			log.Fatalf("gitaly: %v", err)
		}
	}()
	return gs
}

// gitLabAdapter loads the post-receive → internal API adapter config when
// both a GitLab URL and the shared secret are present (docs/integration/gitlab-integration.md §2).
func gitLabAdapter(cfg *config.Config) *gitwire.GitLabConfig {
	if cfg.Gitaly.GitLab.URL == "" || cfg.Gitaly.GitLab.SecretPath == "" {
		return nil
	}
	secret, err := os.ReadFile(cfg.Gitaly.GitLab.SecretPath)
	if err != nil {
		// A configured-but-unreadable secret is a fatal misconfiguration —
		// except in dev, where demo stacks boot before the secret volume
		// is populated (provision arms the adapter with a restart).
		if !cfg.Dev {
			log.Fatalf("gitaly.gitlab: read secret %s: %v", cfg.Gitaly.GitLab.SecretPath, err)
		}
		log.Printf("gitaly.gitlab: secret not readable yet (%v) — adapter off until restart", err)
		return nil
	}
	log.Printf("gitaly.gitlab: post-receive adapter → %s", cfg.Gitaly.GitLab.URL)
	return &gitwire.GitLabConfig{URL: cfg.Gitaly.GitLab.URL, Secret: string(secret)}
}

// staticPolicy builds the demo authorization policy from config (docs/gateway/authorization.md
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
// the role boundary (docs/storage/ha.md §2), so any gateway instance is expendable.
// An optional on-disk pack cache (cache.pack_dir, docs/operations/performance.md P2-1) carries the
// fetched packs across restarts — the one piece of expendable local state
// that meaningfully cuts cold-start traffic.
func runGateway(ctx context.Context, cfg *config.Config) {
	refsClient := meta.NewRefsClient(cfg.RefsAddr)
	ms := &meta.RemoteMeta{Refs: refsClient}
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
	objects := storage.NewObjectServiceWithBackend(ms, backend, model.SHA1)

	if cfg.InitRepo != "" {
		if err := ms.CreateRepo(context.Background(), cfg.InitRepo, model.SHA1); err != nil {
			log.Fatalf("create repo: %v", err)
		}
	}

	mux := http.NewServeMux()
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	api := rpc.NewServer(ms, objects, model.SHA1)
	objects.Addressing = meta.NewAddressing(cfg.Storage.Mode, ms)
	gw.Convergence = cfg.Workflow.Convergence == "auto"
	gw.FullCloneMaxBytes = cfg.FullCloneMaxBytes
	switch {
	case cfg.Auth.Upstream != "":
		// Console-fronted mode on the gateway role (docs/web/console.md §5): same
		// upstream policy as all-in-one — the console fronts both git and
		// management traffic; this process only accepts signed identities.
		up := auth.NewUpstreamPolicy(cfg.Auth.Upstream, []byte(cfg.Auth.UpstreamSecret))
		gw.Auth, api.Auth = up, up
		log.Printf("auth: upstream console %s (snapshot v2 + signed identity)", cfg.Auth.Upstream)
	case cfg.Auth.Remote:
		// The gateway decides against a policy snapshot synced from the
		// refs service (docs/gateway/authorization.md §4) — same abstraction, synced state; the
		// concrete rules live on refs, never duplicated here.
		policy := auth.NewRemotePolicy(cfg.RefsAddr)
		gw.Auth = policy
		api.Auth = policy
		log.Printf("auth: remote policy from %s", cfg.RefsAddr)
	}
	gw.Register(mux)
	api.Register(mux)
	root(mux)
	gs := startGitaly(cfg, ms, objects)
	// Same transport wiring as all-in-one: the gateway decides access
	// against the synced policy snapshot (keys ride along, docs/gateway/authorization.md §4/§7).
	stopSSH, err := startSSH(cfg.SSH.Listen, sshHostKeyPath(cfg.SSH.HostKey, cfg.Root), gw)
	if err != nil {
		log.Fatalf("ssh: %v", err)
	}
	if err := serveHTTP(ctx, cfg.Listen, mux); err != nil {
		log.Fatalf("http: %v", err)
	}
	stopSSH()
	if gs != nil {
		gs.GracefulStop()
	}
}

// runRefs serves the serialization point: meta RPC, the health table and
// the reconciler (single writer; HA story in docs/storage/ha.md §4).
func runRefs(ctx context.Context, cfg *config.Config) {
	ms := openMeta(cfg)
	if cfg.MetaBackend == "mem" {
		log.Printf("refs role with meta=%s: metadata is NOT durable across restarts (use -meta sqlite/mysql/postgres)", cfg.MetaBackend)
	}
	srv := refs.NewServer(ms, refs.Options{
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
	srv.RunBackground(ctx) // health ticker + reconciler stop with the process
	log.Printf("refs: serving %s (default_replicas=%d suspect=%s dead=%s recon=%s×%d)",
		cfg.Listen, cfg.DefaultR, time.Duration(cfg.SuspectSec)*time.Second,
		time.Duration(cfg.DeadSec)*time.Second, time.Duration(cfg.ReconSec)*time.Second, cfg.ReconCurrency)
	if err := serveHTTP(ctx, cfg.Listen, mux); err != nil {
		log.Fatalf("http: %v", err)
	}
}

// runStorage serves the pack warehouse and heartbeats to refs.
func runStorage(ctx context.Context, cfg *config.Config) {
	node := storenode.New(cfg.NodeID, cfg.Root, cfg.RefsAddr, cfg.Advertise)
	mux := http.NewServeMux()
	node.Register(mux)
	root(mux)
	go node.HeartbeatLoop(ctx, time.Duration(cfg.HeartbeatSec)*time.Second)
	log.Printf("storage %s: serving %s (advertise=%s → refs %s)", cfg.NodeID, cfg.Listen, cfg.Advertise, cfg.RefsAddr)
	if err := serveHTTP(ctx, cfg.Listen, mux); err != nil {
		log.Fatalf("http: %v", err)
	}
}

func root(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "banyan — endpoints: /r/<repo>[/info/refs|/git-upload-pack|/git-receive-pack], /api/v1/*, /metrics")
	})
	// Process telemetry (Prometheus text format): scrape target for every
	// role. Internal by convention — front it with the same network
	// boundary as the process's other internal endpoints.
	mux.Handle("/metrics", metrics.Handler())
}
