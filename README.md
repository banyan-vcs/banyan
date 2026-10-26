# Banyan

**English** | [简体中文](README.zh-CN.md)

A **git-compatible** version control server with a directory-anchored timeline
data model and a sharded pack-CAS + central-metadata storage architecture.

> The name comes from the banyan tree (榕树， "one tree becomes a forest"):
> the trunk is the root timeline; the aerial roots are directory timelines —
> each an independent repo, all anchored to the same tree.

## Why banyan

**Git is the inescapable industry standard.** Two decades deep, from personal
projects to the largest enterprises — any new approach must not ask anyone
to leave git.

**But git does not host huge repositories well.** Android-scale monorepos
hit its limits: tens of GB, millions of objects, hour-long clones.
Shallow / sparse / partial clone treat the symptoms, not the "reachable
means cloned" assumption underneath.

**Monorepo is where enterprise code maintenance is heading.** On modern
build systems (Bazel / Buck), source dependency is the most efficient way
to keep code alive: atomic cross-package changes, no release lag, one
commit coordinating many parties.

**And monorepo inevitably needs directory-level permissions** — departmental
boundaries, compliance isolation, vendor visibility — which native git
cannot express; splitting directories into separate repos destroys the
integrity a monorepo exists to provide.

**AI agents multiply collaboration frequency** from human-days to
agent-minutes, amplifying git's centralized-ref bottlenecks (push locks,
full negotiation, force-push/rebase cascades) and the collaboration risks
that come with them.

Banyan answers by **redesigning the git-compatible storage server**:

- **Distributed by construction** — sharded pack CAS + central metadata:
  storage scales horizontally and never binds to a single machine, solving
  the reliability problem of storing code — the core enterprise asset.
- **A directory-anchored timeline data model** — every directory owns its
  commit history, making directory-level collaboration (any subtree is a
  repo, O(subdirectory) transfers, zero cross-directory locks) and
  directory-level governance (permission boundaries are timeline
  boundaries) **native capabilities** rather than add-ons.

**Vision**: code storage becomes an independent infrastructure layer, like
a database — any platform (GitLab / Gitea / self-hosted), any client
(git CLI / IDE / CI / AI agents), any scale (personal project to enterprise
monorepo), one storage layer over standard protocols. Banyan owns "where
code lives and how it is served efficiently"; collaboration flows stay on
the platforms, organization and permissions stay with the enterprise —
directory timelines align both boundaries at the same granularity.

## Design goals

1. **Drop-in git compatibility** — plain `git clone / fetch / push` just works
   (Smart HTTP v1 + v2 fetch-side command set — ls-refs/fetch, docs/04 9b).
2. **Any subdirectory is a repo** — not sparse checkout: every directory owns
   its commit timeline, and `/r/{repo}/tree/{subdir}` serves a semantically
   complete repository with full projected history.
3. **Timeline-first write path (v5)** — pushing to a directory touches only
   that directory's timeline (a single CAS): zero cross-directory locks, and
   the root view is woven linearly on read — no adoption-merge noise.
4. **Sharded pack storage** — packfiles are immutable, content-addressed
   shards with a central metadata index; server-side repack merges fragments
   with delta encoding.
5. **A thin client (`banyan`)** — minimal download by default: the workspace
   holds the checkout manifest and a content-addressed cache, never the
   commit closure; history queries (log/blame/diff) are answered by the
   server over the JSON API.

## What works today

| Capability | Status |
|---|---|
| Root + subtree `git clone/fetch/push` (Smart HTTP v1 + v2) | ✅ verified against real git clients |
| Git over SSH (`ssh://` + public-key auth, v0 stateful negotiation + v2) | ✅ same serving core, same authz (docs/04 §7b) |
| Partial clone (`--filter=blob:none`/`tree:0` + lazy fetch) | ✅ |
| Serve-path delta packs (OFS/REF-delta past 64KiB) | ✅ |
| Thin packs on incremental fetch (REF_DELTA vs client-held bases) | ✅ (0.87x git's bytes, docs/11 sec.3) |
| Authorization: repo / directory / branch granularity, fetch and push (docs/07) | ✅ demo policy, opt-in |
| Storage addressing: named (readable dirs) vs hashed (opaque keys, gateway-mapped) + rename (docs/03 §6) | ✅ hashed renames never touch storage, T40 |
| M4 directory workflow: per-timeline queue + server-side convergence (pure-Go merge-ort subset, docs/15) | ✅ bursts land with zero rejections; conflicts isolated per ticket, T41/T42 |
| Personal branch namespaces (`refs/heads/<user>/*`) visible to their owner alone | ✅ the monorepo too-many-branches answer |
| Web UI: repository management + file browsing (TypeScript, zero runtime deps) | ✅ `web/` |
| Subtree virtual repos (`/tree/<path>`), full-history projection | ✅ |
| Per-directory timelines, linear woven root view (v5) | ✅ |
| Shallow clone (depth / deepen / unshallow) | ✅ |
| Incremental fetch with have subtraction | ✅ |
| Repack: fragment merge + delta encoding | ✅ |
| Tags and arbitrary `refs/<ns>` namespaces | ✅ |
| SQLite metadata persistence + YAML config | ✅ |
| Service layer + JSON API (`/api/v1/*`, error envelope) | ✅ |
| Thin-workspace CLI (`clone/status/commit/pull/log/cat/...`) | ✅ |
| HA cluster: gateway + refs service + scattered multi-replica pack storage, node failure recovery, dynamic per-repo replicas | ✅ core (local-disk cluster, T23-T26; MySQL/raft refs backend → HA-2) |
| Token auth + repo-create tightening, diff/blame RPCs | 🚧 next (M5 / M2–M3) |
| Gitaly gRPC frontend (act as GitLab's storage node) | 🚧 planned (docs/05 §4) |

43 integration cases pin the behavior with the **real git CLI** as the client
([docs/13](docs/13-testing.md)) — the thin-CLI commit path is cross-verified
by fresh git clones plus `fsck`.

## Repository layout

```
cmd/banyand/        server daemon (git smart HTTP + JSON API)
cmd/banyan/         thin-workspace CLI
internal/model/     core data model (anchored commit / timeline / assembly / projection)
internal/storage/   sharded pack storage (shard / metastore / packfile / index / repack)
internal/view/      read-time view engine (v5 weave, subtree views)
internal/gitwire/   git wire protocol (smart HTTP v1 / upload-pack / receive-pack)
internal/service/   protocol-agnostic service layer (shared by all frontends, docs/05)
internal/rpc/       httpd JSON frontend (/api/v1)
internal/cli/       thin-workspace client implementation
internal/config/    layered config (defaults < YAML < flags)
```

## Quick start

### One command with Docker

```bash
docker compose up -d          # build + run; data persists in the banyan-data volume
                              # authorization is ON in the demo (docs/07):
                              #   web UI  http://localhost:8101 (alice / bob / admin, password demo)
                              #   git     http://alice:demo@localhost:8100/r/demo.git

# push any git repo (main is admin-push-only under the demo policy)
git remote add banyan http://admin:demo@localhost:8100/r/demo.git
git push banyan main

# git over SSH too (throwaway demo keys in demo/ssh/, docs/12 §3b)
git clone ssh://alice@localhost:8102/r/demo.git \
  -c core.sshCommand="ssh -i demo/ssh/alice -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

git clone http://alice:demo@localhost:8100/r/demo.git            # root repo
git clone http://alice:demo@localhost:8100/r/demo.git/tree/plumbing  # any subtree is a repo

# thin workspace: minimal download, no .git, server-side history
go run ./cmd/banyan clone http://localhost:8100/r/demo
# ...or without Go on the host:
docker compose exec banyan banyan clone http://127.0.0.1:8080/r/demo /tmp/demo
```

Constrained networks can build through a mirror:
`docker compose build --build-arg GOPROXY=https://mirrors.aliyun.com/goproxy/`.

### HA cluster demo

One command boots the split deployment (docs/10): a refs service, four
storage nodes and a stateless gateway — packs scatter across the storage
nodes with 3 replicas each; killing a storage node keeps reads and writes
alive while the reconciler restores the replica target.

```bash
bash demo/cluster-demo.sh   # full walkthrough: push, kill a node, recover, resize R
```

Or drive it by hand: `docker compose -f docker-compose.cluster.yml up -d`,
push to `http://localhost:8100/r/demo.git`, watch
`GET /api/v1/cluster/nodes`, `docker stop banyan-s4`, push again — the
background reconciler restores the 3-replica target on its own (verified on
Docker 29.x; the same flows are pinned in-process by integration tests T23–T26).

### From source

```bash
go build ./... && go vet ./... && go test ./...   # ~65s (integration needs git on PATH)

# Start the server. Config resolves as defaults < YAML (-config) < explicit flags;
# unknown config keys are rejected. See banyan.example.yaml.
go run ./cmd/banyand -listen 127.0.0.1:8097 -root ./banyand-data -meta sqlite

# Push any git repo (first push auto-creates it)
git remote add banyan http://127.0.0.1:8097/r/myrepo.git
git push banyan main

# Clone the root repo (byte-identical HEAD, verified up to 4k-commit repos)
git clone http://127.0.0.1:8097/r/myrepo.git

# Clone any subdirectory as a standalone repo — full projected history,
# git log / git show / git fsck all work on it
git clone http://127.0.0.1:8097/r/myrepo.git/tree/plumbing/format/packfile

# Thin workspace: minimal download, no .git, server-side history
go run ./cmd/banyan clone http://127.0.0.1:8097/r/myrepo.git
cd myrepo
go run ../cmd/banyan status        # offline: manifest vs worktree
echo change >> README && go run ../cmd/banyan commit -m "edit"   # server-side assembly
go run ../cmd/banyan pull          # server-side tree-diff increment
go run ../cmd/banyan log           # remote history, zero download
```

## Non-goals (current stage)

- Push stays on protocol v0 (git itself never defined v2 for receive-pack); the fetch side speaks v2 natively (ls-refs + fetch, docs/04 9b).
- No DevOps platform features (MR/CI/permissions UI) — banyan is a
  **storage layer**; collaboration stays on existing platforms (docs/01).
- No praefect replication protocol; multi-replica follows banyan's own
  sharding roadmap (docs/03).

## Documentation

Reading order follows the design: overview and positioning first, then the
architecture layer by layer, then cross-cutting concerns, then operations.

**The story** — what banyan is and where it sits:

| Doc | Contents |
|---|---|
| [docs/00-overview.md](docs/00-overview.md) | Vision, design principles, architecture map |
| [docs/01-big-picture.md](docs/01-big-picture.md) | Positioning: universal code storage layer + platform integration |

**Core design** — bottom-up by layer (model → storage → wire → service → client):

| Doc | Contents |
|---|---|
| [docs/02-data-model.md](docs/02-data-model.md) | **Core**: anchored timelines, assembly, git projection |
| [docs/03-storage.md](docs/03-storage.md) | Sharded pack storage: CAS, dual indexes, repack |
| [docs/04-git-compat.md](docs/04-git-compat.md) | Git compatibility: Smart HTTP, SSH transport, virtual subtree repos |
| [docs/05-service-api.md](docs/05-service-api.md) | Service layer + httpd/Gitaly frontends, API contracts |
| [docs/06-cli.md](docs/06-cli.md) | Thin-client CLI command design |

**Cross-cutting**:

| Doc | Contents |
|---|---|
| [docs/07-authorization.md](docs/07-authorization.md) | Authorization model, personal-branch visibility, web demo |
| [docs/08-derived-data.md](docs/08-derived-data.md) | Derived data: commit-graph, path history, blame |
| [docs/09-indexing-packing.md](docs/09-indexing-packing.md) | Indexes (generations) & packing (have subtraction, delta, repack) |
| [docs/15-workflow.md](docs/15-workflow.md) | M4 design: per-directory commit queue + convergence (merge train) |
| [docs/16-benchmark-report.md](docs/16-benchmark-report.md) | Measured benchmark report vs git http-backend (multi-run, drift forensics) |

**Operations & evolution**:

| Doc | Contents |
|---|---|
| [docs/10-ha.md](docs/10-ha.md) | High availability: all-in-one vs gateway + cluster, per-repo replicas |
| [docs/11-performance.md](docs/11-performance.md) | Performance plan: bottlenecks, optimizations, SLO bench gates |
| [docs/12-docker-demo.md](docs/12-docker-demo.md) | Docker demo walkthrough: auth, ssh, web UI, cluster pointers |
| [docs/13-testing.md](docs/13-testing.md) | Integration test matrix |
| [docs/14-roadmap.md](docs/14-roadmap.md) | Milestones |

## Conventions

- Layered commits (bottom-up): `docs → model → storage → gitwire → service →
  cli`; every commit builds and passes its tests independently.
- New capabilities land with an integration case registered in
  [docs/13](docs/13-testing.md); assertions use testify (require = scenario
  gates, assert = independent checks).
- git is the behavioral reference (its source is the oracle); go-git is just
  a codec library, confined to `internal/storage/packfile.go`.

## Tech base

- Go + [go-git](https://github.com/go-git/go-git) (object model, packfile/idx
  codec — library only, never a behavioral boundary)
- Homegrown: timeline model, read-time weaving, sharded storage, virtual
  repos, Smart HTTP layer, service layer, thin CLI

## License

The core is **MIT** ([LICENSE](LICENSE)) — it covers everything outside
[enterprise/](enterprise/). Commercial capabilities live under enterprise/
— a separate Go module licensed under
[enterprise/LICENSE.enterprise](enterprise/LICENSE.enterprise): free to
modify and test, production use requires a banyan enterprise subscription
(each enterprise source carries a `LicenseRef-BANYAN-ENTERPRISE` SPDX
header). The core never imports enterprise; only enterprise imports the
core.
