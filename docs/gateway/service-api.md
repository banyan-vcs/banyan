# 服务层抽象与 API 服务层（httpd / Gitaly 双前端）

> 背景（用户决策）：banyand 抽象中间层，前端可插拔——**httpd** 为本地 demo 服务
> （git smart HTTP + `/api/v1` JSON，也是 **CLI 的一等契约**）；另规划 **Gitaly gRPC
> 兼容前端**，使 GitLab 可直接把 banyand 当作底层存储节点。本文设计 CLI 所需的
> API 服务层，命令面见 [cli.md](../client/cli.md)。

## 0. 决策变更记录

docs/design/big-picture.md 初版把 Gitaly RPC 列为非目标（私有契约跟随者风险）。本篇改为**可选、
分阶段兼容**：私有契约的差异与版本漂移被隔离在 gitaly 适配层内，核心服务与
CLI 契约不受影响。协议原生（标准 git 远端）仍是第一优先路线，Gitaly 兼容是
加深 GitLab 集成的第二路线（比 08 §3.2 镜像拓扑更深一层：无需镜像，单真相源）。

## 1. 分层架构与依赖方向

```
 banyan CLI           GitLab(Rails/workhorse)          任意 git 客户端
    │ JSON /api/v1         │ Gitaly gRPC                   │ smart HTTP
┌───▼───────────────┐ ┌────▼──────────────┐        ┌───────▼───────┐
│ rpc（httpd api）   │ │ gitaly 适配        │        │ gitwire        │
└───┬───────────────┘ └────┬──────────────┘        └───────┬───────┘
    │     前端薄层：解析 / 鉴权 / 序列化，零业务逻辑          │
    └────────────────┬───┴───────────────────────────────┘
              ┌─────▼──────────────────────────┐
              │ service 层（Go 接口，协议无关）    │
              │ Repo / Ref / Object / Tree /     │
              │ Commit / History / Admin         │
              └─────┬──────────────────────────┘
                    │
       model / view（v5 装配）/ derived / storage（Meta + Shard）
```

规则：

1. 前端（rpc / gitaly / gitwire）只依赖 service 接口；前端之间零 import；
2. service 只依赖 model/view/derived/storage，协议类型（HTTP/gRPC）不出 service 层；
3. **pack 协议引擎**（现 gitwire 内部的 upload/receive 解析与闭包计算）是可复用
   组件：smart HTTP 与 Gitaly `PostUploadPack`/`PostReceivePack` 共用；
4. **鉴权唯一挂点在 service 边界**（M5 token / 仓库级 scope）：三个前端同权收敛，
   不在各前端重复实现。

## 2. 核心服务接口（internal/service，设计稿）

```go
type RepoService interface {
    Create(ctx, repo string) error                 // M5 建仓收紧后走 scope
    List(ctx) ([]RepoInfo, error)
    Delete(ctx, repo string) error
}

type RefService interface {                        // heads / tags / 任意 refs/<ns>
    List(ctx, repo string) ([]Ref, error)          // name + head OID
    CAS(ctx, repo, name string, old, new OID) error // old=0 创建，new=0 删除
}

type ObjectService interface {
    Get(ctx, repo string, oid OID) (Object, error)          // cat / 回填
    BatchMeta(ctx, repo string, oids []OID) (map[OID]Meta, error)
    Put(ctx, repo string, blobs map[OID][]byte) error       // commit 前置上传
}

type TreeService interface {                       // CLI 取码：薄工作区的根
    Fetch(ctx, repo, sub, rev string, haveTree OID, paths []string) (FetchResult, error)
    // FetchResult = 变更路径上的新树 + 新 blob（≤256KiB 内联，其余列 pending）
}

type CommitService interface {
    Create(ctx, req CommitRequest) (CommitResult, error)
    // req = {repo, sub, base, message, author, changes[{path, blob|delete}]}
    // res = {head, rootView}
    // **发布语义唯一化**：锚定 + 目录时间线 CAS（v5 写路径）只在此实现，
    // receive-pack / CLI commit RPC / Gitaly receive 三处复用。
}

type HistoryService interface {                    // 读侧集中计算
    Log(ctx, repo, rev, path string, n int) ([]CommitInfo, error)
    Commit(ctx, repo, rev string) (CommitInfo, error)   // show 元数据
    Diff(ctx, repo string, from, to, path string) ([]FileDiff, error)
    FileHistory(ctx, repo, path string) ([]PathChange, error)
    Blame(ctx, repo, rev, path string) ([]BlameLine, error)  // M3
}

type AdminService interface {
    Timelines(ctx, repo string) ([]TimelineInfo, error)
    Repack(ctx, repo string, threshold int) (RepackResult, error)
}
```

## 3. CLI API 契约（`/api/v1`，httpd 参考实现）

CLI 的稳定契约是这层 JSON API（版本化 `/api/v1`），与前端启停无关——生产形态
可只开 gitaly 前端，CLI 仍指向任一开启 httpd 的节点。

### 3.1 端点总表（命令 → 端点 → service 方法）

| 命令（docs/client/cli.md） | 端点 | service 方法 | 状态 |
|---|---|---|---|
| clone / fetch / pull | `POST /api/v1/fetch-trees` | Tree.Fetch | **已落地** |
| fetch-objects / cat / restore 回填 | `POST /api/v1/objects`（批量定位）/ `GET /api/v1/objects?repo&oid` | Object.BatchMeta / Get | 批量**已有**，GET **已落地** |
| commit（blob 上传） | `POST /api/v1/objects/content` | Object.Put | **已落地** |
| commit | `POST /api/v1/commit` | Commit.Create | **已落地** |
| sync | `POST /api/v1/sync` | Timeline.Sync | **已落地**（2026-11-08，docs/design/data-model.md §12 真分叉显式对齐；body `{repo, sub, branch?}`，与 commit 同推送授权门；409 `CONFLICT_SYNC_CONFLICT` 具名提交+路径，400 `SYNC_UNSUPPORTED`=merge 链） |
| log | `POST /api/v1/log` | History.Log | **已有**（已下沉 service） |
| show | `POST /api/v1/log`（n=1）+ `GET /api/v1/diff` | Log + Diff | **已落地**（diff 引擎与 gitaly 同源） |
| diff | `GET /api/v1/diff?repo&from&to&path` | History.Diff | **已落地**（2026-11-06；字节对 `git diff`，T81；path 支持目录前缀 `lib/`） |
| file-history | `POST /api/v1/file-history` | History.FileHistory | M2 |
| blame | `GET /api/v1/blame?repo&rev&path` | History.Blame | **已落地**（2026-11-06；`--line-porcelain` 字节对齐 `git blame`，T81） |
| cat（非 HEAD rev / 缓存脱靶） | `POST /api/v1/resolve` | Resolve | **已落地**（计划外增补：rev:path → 对象指针） |
| branch / tag | `GET /api/v1/refs` / `POST /api/v1/refs` | Ref.List / Ref.CAS | **已落地**（POST 限根仓库） |
| repo create/ls | `GET /api/v1/repos` | Repo.List | GET **已落地**；create 配合 M5 |
| repo rename | `POST /api/v1/repos/<repo>/rename` | Repo.Rename | **已落地**（docs/storage/storage.md §6） |
| 副本数 | `GET/PUT /api/v1/repos/<repo>/placement` | Admin.Placement | **已落地**（docs/storage/ha.md §6） |
| force-push 开关 | `GET/PUT /api/v1/repos/<repo>/force-push` | Repo.ForcePush | **已落地**（docs/gateway/git-compat.md §6a；PUT body `{"enabled":bool}`，运行时生效无需重启） |
| 仓库规模 | `GET /api/v1/repos/<repo>/size` | Repo.Size | **已落地**（docs/operations/performance.md §6；返回 `{bytes, packs}`，全 clone 预算同源） |
| 钩子注册表 | `GET/PUT/DELETE /api/v1/repos/<repo>/hooks[?id=]` | Admin.Hooks | **已落地**（docs/gateway/git-compat.md §7；PUT body `{id,dir,events[],url,secret?,timeout_ms?}`，元数据面存储——多网关一致） |
| timelines（诊断） | `GET /api/v1/timelines` | Admin.Timelines | **已有**（已接 ListAllTimelines） |
| subtrees（管理清单） | `GET /api/v1/subtrees?repo=` | Repo.Subtrees | **已落地**（docs/gateway/authorization.md §8c：过滤 `/.woven`/aux ref、按锚分组、支线 head 带最近提交、projected=投影 ref 集） |
| 仓库导入 | `POST /api/v1/imports` | Repo.Import | **已落地**（2026-11-14；docs/gateway/authorization.md §8d、docs/web/web-layer.md §4：body `{name?, url, source_authorization?}`，同步执行——建仓→服务端 fetch（internal/importer）→采用远端 HEAD；409 已存在、失败回滚；T95） |
| 子树访问规则 | `GET/PUT/DELETE /api/v1/repos/<repo>/subtree-access` | Admin.SubtreeRules | **已落地**（docs/gateway/authorization.md §8c：PUT body `{path, users[]}` 锚形路径；元数据面存储，rename/delete 随迁） |
| admin repack | `POST /api/v1/repack` | Admin.Repack | **已有** |

2026-10-23 落地批次：internal/service + rpc 薄化 + internal/cli（T22/T22b
真 git 客户端交叉验证）。

### 3.2 统一约定

- 鉴权：`Authorization: Bearer <token>`（M5 落地，服务层统一校验）；
- 缓存：读响应 `ETag: <视图 head OID>`，不可变可缓存；
- 错误信封（前端翻译 HTTP 状态，CLI 翻译退出码）：

```json
{ "error": { "code": "CONFLICT_BASE_MOVED", "message": "…", "data": { "head": "…" } } }
```

| code | HTTP | CLI 退出码 |
|---|---|---|
| OK | 200 | 0 |
| CONFLICT_BASE_MOVED / CONFLICT_REF_CAS | 409 | 1 |
| INVALID_ARG / NOT_FOUND | 400 / 404 | 2 |
| UNAUTHORIZED / FORBIDDEN | 401 / 403 | 2 |
| INTERNAL / UNAVAILABLE | 500 / 503 | 2 |
| （传输层失败） | —— | 3 |

### 3.3 关键 schema（字段级）

```jsonc
// POST /api/v1/fetch-trees —— clone 无 haveTree；pull 有 → 下载量 = 两检出差异
→ { "repo": "demo", "sub": "/src/lib", "rev": "main", "haveTree": "sha…", "paths": ["core"], "format": "bin" }
← { "trees": {"<oid>": "<raw>"}, "blobs": {"<oid>": "<raw，≤256KiB 内联>"},
    "pending": ["<大 blob oid，走 GET /api/v1/objects>"] }
// format:"bin"（薄 CLI 默认）→ Content-Type: application/x-banyan-fetch-trees，
// 二进制帧（docs/operations/performance.md P1-2，JSON base64 的 +33% 消失）：
//   "banyan-fetch-1
" + JSON 头行（branch/head/tree/full/pending/algo）
//   + *记录：kind('t'|'b') + 原始 oid（20/32B）+ uvarint 长度 + 内容；省略 format 走 JSON（兼容）

// POST /api/v1/commit —— changeset 语义，服务端组装（docs/client/cli.md §4.2）
→ { "repo": "demo", "sub": "/src/lib", "base": "sha…", "message": "…",
    "author": {"name": "…", "email": "…"},
    "changes": [{"path": "core/x.go", "blob": "sha…"}, {"path": "old.go", "delete": true}] }
← { "head": "sha…", "rootView": "sha…" }        // rootView 供 CI/webhook 定位
← 409 { "error": { "code": "CONFLICT_BASE_MOVED", "data": { "head": "当前 head" } } }

// POST /api/v1/refs —— CAS：old 零值=创建，new 零值=删除（heads/tags/自定义 ns 同一入口）
→ { "repo": "demo", "name": "refs/tags/v1.1", "old": "sha…|0", "new": "sha…|0" }
```

## 4. Gitaly gRPC 兼容前端

### 4.1 定位与寻址

GitLab Rails/workhorse 经 gRPC 访问存储节点；banyand 实现 Gitaly 服务子集后，
`gitlab.rb` 指向 banyand（`gitaly_token` 对齐），即成"GitLab 底层存储"。
寻址：Gitaly `repository{storage_name, relative_path}` → **默认 verbatim**
（`@hashed/xx/yy/zz.git` 整体作为 banyan repo 名，不透明 id）；可选 repos 表
映射（08 §3.3 命名空间映射）。proto 按**目标 GitLab 大版本锁定 vendored**，
版本漂移在适配层吸收。

### 4.2 分期

| 期 | Gitaly service | RPC 子集（代表） | 映射到核心 | GitLab 解锁 |
|---|---|---|---|---|
| P1 | ServerService | ServerInfo / DiskStatistics | Admin | 存活/容量检查 |
| P1 | RepositoryService | RepositoryExists / Create / Delete / Size / HasLocalBranches | Repo | 项目建删、空仓 |
| P1 | SmartHTTPService | InfoRef* / PostUploadPack / PostReceivePack | pack 引擎 + Ref + Commit.Create | git HTTP clone/fetch/push |
| P1 | RefService | FindDefaultBranchName / ListBranches / ListTags | Ref.List | 分支/tag 列表 |
| P1 | CommitService | FindCommit / CommitLog / CommitsBetween / TreeEntries / ListFiles | History + Tree | 历史页/文件树浏览 |
| P1 | BlobService | GetBlob(s) | Object.Get | 文件查看/raw |
| P1 | DiffService | CommitDiff(s) | History.Diff | 变更视图 |
| P2 | OperationService | UserCommitFiles / User*Branch | Commit.Create + Ref.CAS | Web IDE/网页提交 |
| P2 | TagService | CreateTag / DeleteTag | Ref.CAS + tag 对象管线 | tag 管理 |
| P2 | SSHService | SSHReceivePack / SSHUploadPack | pack 引擎 | git over SSH |
| P3 | RemoteService | FetchRemote | 拉取引擎 | GitLab 项目拉取镜像 |
| P3 | HousekeepingService | OptimizeRepository | **Admin.Repack**（天然映射） | GitLab 触发存储优化 |

### 4.3 receive 链路的 Rails hook 回调（关键风险点）

GitLab 的 push 校验（保护分支、大小限额、push 事件）由存储节点回调 Rails
internal API（PreReceive/PostReceive）完成。banyand 不跑 git hook——gitaly
前端在 receive 提交点直接调 `internal_url`（config 开关）。P1 关闭时 push
可用但保护分支/事件失效（demo 可接受）；P2 完整实现，与 M5 webhook 派发
共用同一 receive 后挂点。

### 4.4 明确不承诺

praefect 复制/事务协议（RefTransaction/Replication，直连单节点模式先行）、
ObjectPool、Wiki、Conflicts（远期评估）。多副本走 banyan 自身路线（docs/storage/storage.md
分布式规划），不借 praefect。

## 5. banyand 装配与配置

单二进制，前端按 config 启停；demo profile = 现状（`listen` 即 httpd 全开）。

```yaml
listen: 127.0.0.1:8100       # httpd：/r/*（git smart HTTP）+ /api/v1（CLI）
root: …
meta: sqlite
gitaly:
  enable: false
  listen: 127.0.0.1:8102
  token: "<与 gitlab.rb gitaly_token 一致>"
  storage: default           # 对 GitLab 暴露的 storage_name
  hooks:
    enabled: true            # P2 起生效：receive 后回调 Rails internal API
    internal_url: "https://gitlab.example/api/v4/internal"
debug:
  pprof_addr: 127.0.0.1:6060 # 运维面（§5a）；缺省整层关闭
```

### 5a. 运维面（debug 节，2026-10-26 落地）

性能跟踪端点跑在**独立辅助监听**上（`debug.pprof_addr`，四个角色通用，
缺省关闭——刻意不搭公开 git/API 端口：profile 与计数器描述仓库内容
与工作模式，暴露是部署决策）。启动失败仅告警不退出（运维面从不是
启动依赖）。

- `/debug/pprof/*`：pprof 全家（heap/allocs/goroutine/block/mutex/
  threadcreate/profile/trace/cmdline/symbol）。
- `/debug/stats`：一次性运行时快照（uptime、Go 版本、GOMAXPROCS、
  goroutine/线程数、**GOMEMLIMIT 读数**（内存链纪律的软上限）、
  MemStats 精要——heap/stack/sys/alloc/GC 次数与停顿）。
- `/debug/vars`（expvar）：**gitwire 服务路径计数器**——
  `banyan_fetch_exact_pack_stream`（clone 形态快路径，闭包跳过）/
  `banyan_fetch_verbatim_pack`（闭包后精确命中直传）/
  `banyan_fetch_closure_cache`（响应缓存）/
  `banyan_fetch_encoded`（在线编码）/
  `banyan_fetch_thin` / `banyan_push_accepted`。任何服务事故的第一个
  性能问题（"实际走的哪级阶梯"）从此读数即答。
- 采样率旋钮：`debug.mutex_profile_fraction` /
  `debug.block_profile_rate_ns`（0=关；block 低于 ~10µs 有实测吞吐
  代价）；设了采样率而没开 pprof_addr 会被严格校验拒绝。

### 5b. 集群内部端点与 `/metrics`（2026-11-05 登记）

`/api/v1` 与 git 面之外，集群内部有两套平面（设计细节归 docs/storage/storage.md/10，
本节是总册登记）：

**refs 服务内部面**（`/internal/*`）：

| 端点 | 用途 |
|---|---|
| `POST /internal/meta/begin-rename` · `abort-rename` | rename 写屏障两段式（docs/storage/storage.md §6b：围栏+墓碑） |
| `POST /internal/sweep {"min_age_sec":n,"dry_run":b}` | fleet GC 两阶段（docs/storage/ha.md §8.7）；周期化见 docs/project/roadmap.md P1 |
| `/internal/heartbeat` · `/internal/health/nodes` · `/internal/reconcile` · `/internal/authz/policy` · `/internal/meta/*` | 既有平面（docs/storage/ha.md） |

**存储节点内部面**（storenode）：`/internal/pack`（GET 整包/流式）、
`/internal/object`（单对象）、`/internal/pack/list`（带 mtime，sweep 用）、
`/internal/keys`（节点存储键清单，sweep 用）、`/internal/pack/clean-tmp`、
`/internal/pack/rename`·`delete-repo`、`/internal/status`。

**`/metrics`**（Prometheus 文本，每角色：all-in-one/gateway 的主监听 +
refs 自有监听；零依赖自研 registry，internal/metrics）——值班第一眼
切的面：

| 指标 | 类型 | 语义 |
|---|---|---|
| `banyan_git_requests_total{transport,op,result}` | counter | git 会话数（http/ssh × upload_pack/receive_pack 等 × ok/rejected/error） |
| `banyan_git_request_seconds{transport,op}` | histogram | 会话耗时 |
| `banyan_git_inflight{transport,op}` | gauge | 在途会话（drain 观测） |
| `banyan_ingest_packs_total` / `banyan_ingest_bytes_total` | counter | 落盘 pack 数与字节 |
| `banyan_cas_conflicts_total` | counter | timeline head CAS 冲突（实现源头计数，双拓扑成立） |
| `banyan_pack_holder_fallbacks_total` | counter | 散布存储 holder 校验失败回退（抽取终验击中即计数——腐坏副本信号） |
| `banyan_reconciler_under_replicated` / `banyan_reconciler_failed` | gauge | 最近的欠副本仓数/失败数 |
| `banyan_reconciler_copied_total` / `banyan_reconciler_trimmed_total` | counter | 副本修复字节数/裁剪数 |
| `banyan_sweep_keys_reclaimed_total` / `banyan_sweep_packs_reclaimed_total` | counter | fleet GC 回收量 |

运维面层级由此定型：`/debug/*`（pprof/stats/expvar）独立辅助监听、
部署决策开关；`/metrics` 在主监听、随进程常开（只读、无仓库内容，
按行抓取）。

## 6. 代码迁移路径（现状 → 目标）

| 现状 | 目标 |
|---|---|
| ~~rpc handlers 直调 storage/view~~ | ✅ 已抽 `internal/service`（2026-10-23）：rpc 为薄适配，log/repack/timelines 亦走服务层 |
| receive-pack 内联锚定 + CAS 逻辑 | 下沉 `CommitService.Create`，gitwire 调用（随 Gitaly 前端：`PostReceivePack` 复用同一发布语义） |
| upload-pack 闭包/减除逻辑 | 留在 pack 引擎（协议专有）；refs 广告改走 `Ref.List` |
| 无 gitaly 前端 | `internal/gitaly`：vendored proto + 适配器（§4） |

## 7. 里程碑对齐

| 里程碑 | 本篇内容 |
|---|---|
| M2 | ✅ 服务层抽取 + fetch-trees / objects 扩展 / commit / refs / resolve / repos RPC + thin CLI（2026-10-23，T22）；✅ diff RPC（2026-11-06，T81）；路径历史=log RPC 既有 path 过滤；file-history 独立端点仍留待 |
| M3 | ✅ blame RPC（2026-11-06，T81）；pull --rebase（服务端重放复用 Commit.Create） |
| M5 | token 认证（service 边界）；Gitaly P1；repo 建仓收紧 |
| M5+ | Gitaly P2（hook 回调 + webhook 共挂点）；P3（housekeeping→repack） |
