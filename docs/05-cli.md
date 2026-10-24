# 05 · banyan CLI 命令设计

> 参考 git 常用命令面梳理。本文只做命令设计，不含实现。核心立场（用户决策）：**不默认 clone 全仓，最小化下载；
> 读操作（log/blame 等）经常性走 API，由服务端完成**。

## 1. 设计原则

| 原则 | banyan 映射 | 现状 |
|---|---|---|
| 客户端永远在线，按需取对象 | CLI 无离线承诺；默认只下载工作区所需 | 待建 |
| 客户端-服务端不对称：历史/合并服务端算，客户端只有工作区 + 有界缓存 | log/blame/diff 全走 RPC；commit 服务端组装 | log/objects RPC 已有 |
| 版本控制即 API：CI/工具可完全没有本地仓 | 所有读操作均有 RPC 形态，检出可选 | 部分 |
| 提交廉价、增量、无锁；ref 移动与变更创建解耦 | 目录时间线追加即发布，head 单点 CAS；根视图读时装配 | **v5 写路径已同构** |
| 工作区按读付费（sparse/virtualized） | 子树即仓库（`/tree/<path>`）；仓内再稀疏化 | 子树已有 |

关键取舍：banyan 对象模型仍是 git（blob/tree/commit）。thin CLI 走
API 直取内容寻址对象；git 客户端走 wire pack——**同一服务端双协议面**，不另造对象格式。

## 2. 客户端形态：薄工作区（默认）

```
.banyan/
  config        # server URL / repo / scope(子树路径) / token 引用
  manifest      # 检出基准 {commit OID, tree OID, 文件→OID 清单}
  cache/objects # 内容寻址 blob 缓存（LRU 上界，可随时清）
工作区 = 普通目录，无 .git，无本地历史
```

- 薄工作区天然无历史：log/blame/diff/show 永远问服务端，从不下载提交闭包；
- `--git` 姿态退回完整本地 git 仓（promisor 指向 banyan，见 §6），给离线/IDE 深度场景留后门；
- `--depth` / `--filter` 等减量选项只属于 git 姿态——薄工作区本身就不取历史。

## 3. 命令面（对照 git 常用命令）

### 3.1 取码与同步

| git | banyan | 网络/执行 |
|---|---|---|
| `git clone <url>` | `banyan clone <url>[/tree/<dir>]` | 默认最小化：HEAD 根树 + 检出文件 blob；`--sparse <dirs>` 只物化指定目录；`--git` 全量本地仓 |
| `git fetch` | `banyan fetch` | 服务端 tree-diff → 只取变更 blob，更新 manifest，不动工作区 |
| `git pull` | `banyan pull` | fetch + 快进物化；基线落后提示 `--rebase`（服务端重放，§4.3） |
| ——（partial 补取） | `banyan fetch-objects <oid>...` | 批量对象 RPC（懒回填内部命令；restore/cat 兜底） |

clone 协商差异（对照 git 的 have/want pack 协商）：native CLI 不做协商，按树递归
请求不可变对象——可断点续传、任意 HTTP 缓存可服务。
pack 协商留给 git 协议面（含 have 减除，docs/09 §3.1）。

### 3.2 本地与提交

| git | banyan | 说明 |
|---|---|---|
| `git status` | `banyan status` | manifest vs 工作区，离线可算（树嵌在 manifest） |
| `git add` | ——（取消） | 快照式：commit 时全量扫描工作区，无暂存区 |
| `git commit` | `banyan commit -m` | 网络提交，服务端组装（§4.2）；目录级时间线 ⇒ 零跨目录锁 |
| `git push` | `banyan push` | 仅 `--git` 姿态（wire receive-pack）；薄工作区 commit 即发布 |
| `git restore` | `banyan restore [<path>]` | 回 manifest 状态；缓存被逐则回填 |
| `git checkout/switch` | `banyan switch <branch\|tag\|oid>` | 换 manifest 基准 + 增量物化（detached OID 允许） |

### 3.3 历史与阅读（全部远端，零下载）

| git | banyan | 服务端 |
|---|---|---|
| `git log [<path>]` | `banyan log [path] [-n N]` | `/api/v1/log`（**已有**） |
| `git diff` | `banyan diff [rev] [--base <rev>] [path]` | `/api/v1/diff`（M2） |
| `git show` | `banyan show <rev>[:<path>]` | 复用 diff + commit 元数据 |
| `git blame` | `banyan blame <path> [-r rev]` | `/api/v1/blame`（M3） |
| `git log --follow` | `banyan file-history <path>` | `/api/v1/file-history`（M2） |
| `git show <rev>:<file>` | `banyan cat <rev>:<path>` | 单对象定位+取（objects RPC） |

### 3.4 refs 与仓库

| git | banyan | 服务端 |
|---|---|---|
| `git branch` | `banyan branch ls/new/del` | `/api/v1/refs`（辅助 ref wire 侧已落地，dcc5f34） |
| `git tag` | `banyan tag ls/new/del` | 同上（tag 对象管线已就绪） |
| 远端建仓 | `banyan repo create/ls` | `/api/v1/repos`（配合 M5 建仓收紧） |

### 3.5 工作流与运维（后续里程碑）

| 命令 | 对标 | 里程碑 |
|---|---|---|
| `banyan snapshot` | `git stash`（快照式本地状态） | M4 |
| `banyan stack` | 栈式变更（stacked changes） | M4 |
| `banyan sync` / `merge` / `rebase` | 服务端合并/重放（子树对根对齐） | M3/M4 |
| `banyan auth login` | token（PAT / CI job token） | M5 |
| `banyan admin repack / mirror ...` | 运维面（拓扑 A/B 建立/巡检/对账） | M5 |

不设 grep/search——搜索属派生索引范畴，随派生数据机制远期评估。

## 4. 关键协议

### 4.1 clone / pull：树递归 + tree-diff 增量

```
POST /api/v1/fetch-trees {repo, sub?, rev|tree, have_tree?, paths?}
→ {trees: {oid: raw}, blobs: [oid|内联内容]}   # 小 blob 内联，大 blob 走 objects 分批
```

- clone：无 `have_tree`，自根树递归；
- pull：`have_tree` = 本地 manifest 树，服务端只返回 diff 路径上的新树与新 blob——
  **下载量 = 两次检出的差异**，与历史深度无关；
- 全部内容寻址不可变 ⇒ 断点续传 + CDN/代理可缓存。

### 4.2 commit：服务端组装（changeset 语义，git 对象落地）

```
banyan commit:
1. 扫描工作区 vs manifest → 变更清单 [{path, blob|删除}]
2. POST /api/v1/objects（带 payload）上传新 blob     # 只传新内容
3. POST /api/v1/commit {repo, sub?, base, message, changes[], author?}
   服务端：载 base 树 → 仅沿变更路径重建子树 → 新 commit 锚入目录时间线
→ {head, root_view}    # root_view = 织入后根视图 OID（CI/webhook 定位用，docs/08 §3.2）
```

changeset 语义（"相对首父的变更清单 + 内容寻址指针"）直接落地为标准 git
树/提交——git 客户端与镜像拓扑完全无感。同目录并发冲突：base ≠ 服务端 head →
拒绝，提示 `banyan pull --rebase`（服务端按变更清单重放到新 head）；真冲突则
返回双方内容，CLI 物化冲突标记，解决后重新 commit。

### 4.3 RPC 契约汇总（HTTPS + JSON）

| 端点 | 状态 |
|---|---|
| `/api/v1/objects` `/api/v1/timelines` `/api/v1/log` `/api/v1/repack` | **已有** |
| `/api/v1/fetch-trees` `/api/v1/commit` `/api/v1/objects/content`（上传）/ `GET /api/v1/objects`（单取）/ `/api/v1/refs`（GET ls + POST CAS）/ `/api/v1/resolve` / `GET /api/v1/repos` | **已落地**（2026-10-23，T22 钉住） |
| `/api/v1/diff` `/api/v1/file-history` | M2 |
| `/api/v1/blame` | M3 |

选 JSON 而非 gRPC：联调成本低；M3 blame 大文件若需流式再升级。所有读响应带
`ETag: <视图 head OID>`——不可变 ⇒ 浏览器/CDN/Agent 工具链可缓存。
端点 schema 与服务层归属（httpd / Gitaly 双前端共核心）详见
[10-service-api.md](10-service-api.md)。

## 5. 输出约定（Agent 友好）

- 全命令 `--json` 机读输出，默认人读；字段名稳定；
- 非交互默认（无提示式向导），只读命令幂等；
- 退出码：`0` 成功 / `1` 冲突 / `2` 服务端错误 / `3` 网络错误——脚本可分支。

## 6. 与 git 客户端的关系（`--git` 姿态，保留原案）

```
banyan clone --git http://srv/r/demo/tree/src/lib
  = git clone --filter=blob:none http://srv/r/demo/tree/src/lib
  + 配置 promisor remote + 对象缺失时调 banyan fetch-objects 回填
```

banyan 不隐藏 git：`--git` 仓内 `git status` / IDE 全部可用；薄工作区则是给
"只要改代码 + 问服务端"的日常/Agent 场景的默认形态。

## 7. 分期

| 阶段 | 命令组 | 服务端前置 |
|---|---|---|
| M2 | clone / fetch / pull / status / commit / log / cat / fetch-objects | fetch-trees + commit RPC |
| M3 | diff / blame / file-history / pull --rebase | diff/blame/file-history RPC |
| M4 | snapshot / stack / sync / merge | 服务端合并与重放 |
| M5 | auth / repo / admin（repack、mirror） | token 认证 + 建仓收紧 |

**进度（2026-10-23）**：M2 组除 `fetch`（独立命令，pull 已覆盖网络半程）外全部
落地——`clone/status/commit/pull/log/cat/fetch-objects/branch/tag/repo ls`，
`internal/cli` + `cmd/banyan`，T22/T22b 以真 git 客户端交叉验证（fresh clone +
fsck + 逐字节内容断言）。薄工作区语义即 §2/§4 设计原样：无 .git、无历史下载、
commit 即发布（changeset 服务端组装 + 目录时间线 CAS）、冲突退出码 1。
