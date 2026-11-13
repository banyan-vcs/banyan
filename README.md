<p align="center"><img src="docs/assets/avatar.png" width="120" alt="banyan" /></p>

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

## Architecture

<p align="center">
  <img src="docs/assets/architecture-simple.svg" width="800" alt="banyan simplified architecture: browsers reach a stateless web tier (banyand web — sign-in, import, capability profiles); git, GitLab and thin-CLI clients go straight to stateless gateways; a refs service as the only serialization point beside scattered pack storage; directory timelines woven into the root repository on read" />
</p>

The four moves that make it work:

- **Directory-anchored timelines** — every directory owns its commit
  history, so any subtree is a semantically complete repo and a permission
  boundary is a timeline boundary.
- **Weave-on-read** — a push CASes exactly one directory timeline (zero
  cross-directory locks); the root repository is woven linearly when it is
  read, never merged on write.
- **Sharded pack CAS + central metadata** — immutable content-addressed
  pack shards scattered across N storage nodes with R replicas; a refs
  service is the only serialization point, so bytes never need consensus.
- **Thin fronts, one core** — git Smart HTTP v1/v2 + SSH, Gitaly gRPC
  (163 RPCs — a GitLab drop-in) and the JSON API all delegate to the same
  protocol-agnostic service core.

## Design goals

1. **Drop-in git compatibility** — plain `git clone / fetch / push` just works
   (Smart HTTP v1 + v2 fetch-side command set — ls-refs/fetch, docs/gateway/git-compat.md 9b).
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

Everything below is landed and pinned by **106 top-level integration test
functions** driving the real git CLI ([docs/operations/testing.md](docs/operations/testing.md));
GitLab-mode rows are additionally verified against a real GitLab-CE
([docs/integration/gitaly-compat.md](docs/integration/gitaly-compat.md) §8, [docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md)).

**Git protocol** — what a plain git client can do:

| Capability | Notes |
|---|---|
| Root + subtree `clone / fetch / push` | Smart HTTP v1 + v2 ([docs/gateway/git-compat.md](docs/gateway/git-compat.md)) |
| Git over SSH | `ssh://` + public-key auth, v0 stateful + v2; same serving core, same authz |
| Partial clone | `--filter=blob:none` / `tree:0`, lazy fetch by OID |
| Shallow clone | depth / deepen / unshallow |
| Tags and arbitrary `refs/<ns>` namespaces | stored and advertised verbatim |
| Push options | `-o` negotiated and parsed |
| Efficient transfers | serve-path delta packs; thin packs on incremental fetch — 0.87× git's bytes ([docs/operations/performance.md](docs/operations/performance.md) §3); have subtraction |

**Directory-anchored storage** — the data model in action:

| Capability | Notes |
|---|---|
| Any subdirectory is a repo | `/r/{repo}/tree/{subdir}`, full projected history — `log / show / fsck` all work on it |
| Per-directory timelines | a push CASes exactly one timeline — zero cross-directory locks; the root view is woven on read (v5) |
| Directory workflow (M4) | per-timeline queue + server-side convergence: bursts land with zero rejections, conflicts isolated per ticket; `banyan snapshot` workspace checkpoints, `banyan stack` path-scoped changeset queue, and `banyan sync` explicit subtree→root rebase — server-side replay closing the true-divergence race ([docs/design/workflow.md](docs/design/workflow.md), [docs/client/cli.md](docs/client/cli.md) §3.7-3.9) |
| Personal branch namespaces | `refs/heads/<user>/*` visible to their owner alone — the monorepo too-many-branches answer |
| Repack | fragment merge + delta encoding |
| Bounded-memory serving | exact-pack verbatim streams; typed enumeration + windowed encode at any repo size ([docs/storage/storage.md](docs/storage/storage.md) §3) |

**Platform integration** — GitLab drop-in:

| Capability | Notes |
|---|---|
| Gitaly gRPC frontend | protocol v19.5.2, **163/163 RPCs** — Operation/Ref/Commit/Diff/Blob services, praefect (cluster-of-one), backups, bundles, history rewrite, sidechannel transport ([docs/integration/gitaly-compat.md](docs/integration/gitaly-compat.md)) |
| GitLab-CE on banyan storage | project create, push/clone over HTTP **and** SSH, web browsing, MR merges, wiki, archives — live-verified against GitLab-CE 19.5 ([docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md)) |
| Push policy gate | protected branches and friends enforced via `/internal/allowed`; Rails' rejection message reaches the git client verbatim |
| Platform callbacks | post-receive adapter: activity events, pipelines, cache expiry, MR-from-push (`-o merge_request.create` verified live) |
| Subtree projection refs | `refs/banyan/tree/<dir>` — one GitLab URL and one credential per subtree; pushes assemble upward into the root view |
| Subtree management console | `/-/banyan/` under the GitLab URL (`banyand web` in its gitlab profile — banyan's subtree features only, GitLab keeps project management): subtree listings, activity, clone recipes and **runtime access rules** — GitLab SSO login ([docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md) §7b) |
| GitLab push size limit | the Maximum-push-size setting enforced via `receive.maxInputSize` |

**Production and operations**:

| Capability | Notes |
|---|---|
| HA cluster | gateway + refs service + scattered multi-replica packs; node-failure recovery, dynamic per-repo replicas ([docs/storage/ha.md](docs/storage/ha.md); local-disk cluster today, MySQL/raft refs → HA-2) |
| Authorization | repo / directory / branch granularity, fetch and push ([docs/gateway/authorization.md](docs/gateway/authorization.md); demo policy, opt-in) |
| Personal access tokens | sha256 at rest, plaintext shown once, expiry/revocation on read; scopes are ceilings, never bypasses — TokenPolicy decorates any policy, RPC authz at the same seam |
| Production posture | secure-by-default startup, DB-backed metadata by default, graceful drain, `/metrics` on every role (P0, [docs/project/roadmap.md](docs/project/roadmap.md)) |
| Server-side diff & blame | path-diff and blame RPCs byte-equal to `git diff` / `git blame` |
| Storage addressing | named vs hashed, renames never touch storage ([docs/storage/storage.md](docs/storage/storage.md) §6) |
| Service layer + JSON API | `/api/v1/*` unified error envelope; SQLite / MySQL / Postgres metadata + YAML config |
| Web tier (`banyand web`) | the three-layer standalone deployment (web → gateway → storage): sessions and import history in the web tier's own database — sqlite / mysql / postgres, optionally **shared with the gateway** (the demo does) — local and OAuth login (**GitHub, gitlab.com and private GitLab instances side by side**; gateway side: `auth.oauth`), the SPA, and the single control plane in the standalone profile ([docs/web/web-layer.md](docs/web/web-layer.md)) |
| Repository import | from github.com / gitlab.com / private GitLab by URL, authenticated by the login provider's token or a pasted PAT — a server-side fetch through the same ingest a push rides, pack streamed to disk at any repo size ([docs/web/web-layer.md](docs/web/web-layer.md) §4) |
| Clients & UIs | thin-workspace CLI (`banyan`), React web console (code browsing, history/diff/blame, branches, tokens, subtree management, import), product console (`banyand console`) with subtree-scoped roles ([docs/web/console.md](docs/web/console.md); console SSH/web → M6.3/M6.4) |
| Ops plane | pprof family, runtime snapshots, serve-path counters |

The thin-CLI commit path is cross-verified by fresh git clones plus `fsck`;
the gitaly frontend is cross-verified against git's own answers (T60), a
workhorse-shaped transport emulator (T61), the multiplexed sidechannel
transport real clones ride (T62), and the diff surface byte-compared with
git diff (T63).

## Repository layout

```
cmd/banyand/        server daemon (roles + subcommands: serve / web / console / bench)
cmd/banyan/         thin-workspace CLI
internal/model/     core data model (anchored commit / timeline / assembly / projection)
internal/meta/       metadata plane (MetaStore: memory / SQL / sqlite, refs client,
                    PAT registry, subtree rules, addressing)
internal/storage/   object plane (sharded packs: shard / packfile / index / repack)
internal/view/      read-time view engine (v5 weave, subtree views)
internal/gitwire/   git wire protocol (smart HTTP v1 / upload-pack / receive-pack)
internal/gitaly/   gitaly gRPC frontend shell (auth/lifecycle) over per-service
                    families (repository/commit/ref/object/operations/transport/
                    backup/node + kit shared plane), GitLab drop-in, docs/integration/gitaly-compat.md
internal/gitalypb/ vendored gitaly protocol (v19.5.2, generated)
internal/service/   protocol-agnostic service layer (shared by all frontends, docs/gateway/service-api.md)
internal/rpc/       httpd JSON frontend (/api/v1)
internal/importer/  server-side remote fetch (repo import + gitaly FetchRemote)
internal/webapp/    the web tier behind `banyand web` (sessions DB, OAuth, import jobs, SPA)
web/                the React console (Vite SPA, served by the web tier)
internal/cli/       thin-workspace client implementation
internal/config/    layered config (defaults < YAML < flags)
```

## Quick start

### One command with Docker

```bash
docker compose up -d          # build + run; data persists in the banyan-data volume
                              # authorization is ON in the demo (docs/gateway/authorization.md):
                              #   web UI  http://localhost:8101 (alice / bob / admin, password demo)
                              #   git     http://alice:demo@localhost:8100/r/demo.git

# push any git repo (main is admin-push-only under the demo policy) —
# picture an Android-shaped monorepo: frameworks/base, system/core, external/skia
git remote add banyan http://admin:demo@localhost:8100/r/demo.git
git push banyan main

# git over SSH too (throwaway demo keys in demo/ssh/, docs/operations/docker-demo.md §3b)
git clone ssh://alice@localhost:8102/r/demo.git \
  -c core.sshCommand="ssh -i demo/ssh/alice -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

git clone http://alice:demo@localhost:8100/r/demo.git            # the whole monorepo
git clone http://alice:demo@localhost:8100/r/demo.git/tree/frameworks/base  # one project out of it

# thin workspace: minimal download, no .git, server-side history
go run ./cmd/banyan clone http://localhost:8100/r/demo
# ...or without Go on the host:
docker compose exec banyan banyan clone http://127.0.0.1:8080/r/demo /tmp/demo
```

### GitLab-CE on banyan storage

A real GitLab whose only Gitaly storage node is banyan — project create,
HTTP push, web UI browsing all served by banyan's gitaly gRPC frontend
(docs/integration/gitaly-compat.md; the compatibility layer's live-fire acceptance):

```bash
docker compose -f demo/gitlab/docker-compose.yml up -d   # GitLab-CE 19.5 + banyan (~4 GiB)
demo/gitlab/provision.sh                                  # seed, push, read-back walkthrough
# GitLab UI http://localhost:8180 — every git byte lives in banyan
# Subtree console http://localhost:8180/-/banyan/ — GitLab SSO login
```

Wiring an existing GitLab-CE (config, verification checklist, troubleshooting):
[docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md).


Constrained networks can build through a mirror:
`docker compose build --build-arg GOPROXY=https://mirrors.aliyun.com/goproxy/`.

### HA cluster demo

One command boots the split deployment (docs/storage/ha.md): a refs service, four
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
# unknown config keys are rejected. -dev = unauthenticated local mode — production
# posture refuses to start without auth (see banyan.example.yaml). See banyan.example.yaml.
go run ./cmd/banyand -dev -listen 127.0.0.1:8097 -root ./banyand-data -meta sqlite

# Push any git repo (first push auto-creates it)
git remote add banyan http://127.0.0.1:8097/r/myrepo.git
git push banyan main

# Clone the root repo (byte-identical HEAD, verified up to 4k-commit repos)
git clone http://127.0.0.1:8097/r/myrepo.git

# Clone any subdirectory as a standalone repo — full projected history,
# git log / git show / git fsck all work on it. In an Android-shaped
# monorepo: one project's history without the other gigabytes.
git clone http://127.0.0.1:8097/r/myrepo.git/tree/frameworks/base

# Thin workspace: minimal download, no .git, server-side history
go run ./cmd/banyan clone http://127.0.0.1:8097/r/myrepo.git
cd myrepo
go run ../cmd/banyan status        # offline: manifest vs worktree
echo change >> README && go run ../cmd/banyan commit -m "edit"   # server-side assembly
go run ../cmd/banyan pull          # server-side tree-diff increment
go run ../cmd/banyan log           # remote history, zero download
```

## Non-goals (current stage)

- Push stays on protocol v0 (git itself never defined v2 for receive-pack); the fetch side speaks v2 natively (ls-refs + fetch, docs/gateway/git-compat.md 9b).
- No DevOps platform features (MR/CI/permissions UI) — banyan is a
  **storage layer**; collaboration stays on existing platforms (docs/design/big-picture.md).
- No praefect replication protocol; multi-replica follows banyan's own
  sharding roadmap (docs/storage/storage.md).

## Documentation

Reading order follows the design: overview and positioning first, then the
architecture layer by layer, then cross-cutting concerns, then operations.

**The story** — what banyan is and where it sits:

| Doc | Contents |
|---|---|
| [docs/design/overview.md](docs/design/overview.md) | Vision, design principles, architecture map |
| [docs/design/big-picture.md](docs/design/big-picture.md) | Positioning: universal code storage layer + platform integration |

**Core design** — bottom-up by layer (model → storage → wire → service → client):

| Doc | Contents |
|---|---|
| [docs/design/data-model.md](docs/design/data-model.md) | **Core**: anchored timelines, assembly, git projection |
| [docs/storage/storage.md](docs/storage/storage.md) | Sharded pack storage: CAS, dual indexes, repack |
| [docs/gateway/git-compat.md](docs/gateway/git-compat.md) | Git compatibility: Smart HTTP, SSH transport, virtual subtree repos |
| [docs/gateway/service-api.md](docs/gateway/service-api.md) | Service layer + httpd/Gitaly frontends, API contracts |
| [docs/client/cli.md](docs/client/cli.md) | Thin-client CLI command design |

**Cross-cutting**:

| Doc | Contents |
|---|---|
| [docs/gateway/authorization.md](docs/gateway/authorization.md) | Authorization model (incl. secure-by-default startup), personal-branch visibility, web demo |
| [docs/design/derived-data.md](docs/design/derived-data.md) | Derived data: commit-graph, path history, blame |
| [docs/storage/indexing-packing.md](docs/storage/indexing-packing.md) | Indexes (generations) & packing (have subtraction, delta, repack) |
| [docs/design/workflow.md](docs/design/workflow.md) | M4 design: per-directory commit queue + convergence (merge train) |
| [docs/web/console.md](docs/web/console.md) | Product-side console (`banyand console`): group/project routing, PAT auth, member roles |
**Operations & evolution**:

| Doc | Contents |
|---|---|
| [docs/storage/ha.md](docs/storage/ha.md) | High availability: all-in-one vs gateway + cluster, per-repo replicas |
| [docs/operations/performance.md](docs/operations/performance.md) | Performance plan: bottlenecks, optimizations, SLO bench gates, measured benchmark report (§3a) |
| [docs/operations/docker-demo.md](docs/operations/docker-demo.md) | Docker demo walkthrough: auth, ssh, web UI, cluster pointers |
| [docs/operations/testing.md](docs/operations/testing.md) | Integration test matrix |
| [docs/project/roadmap.md](docs/project/roadmap.md) | Milestones & the production-readiness track |
| [docs/integration/gitaly-compat.md](docs/integration/gitaly-compat.md) | Gitaly gRPC compat layer: coverage matrix (163 RPCs), design notes |
| [docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md) | GitLab integration guide: wiring steps, verification checklist, troubleshooting |

## Conventions

- Layered commits (bottom-up): `docs → model → storage → gitwire → service →
  cli`; every commit builds and passes its tests independently.
- New capabilities land with an integration case registered in
  [docs/operations/testing.md](docs/operations/testing.md); assertions use testify (require = scenario
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
