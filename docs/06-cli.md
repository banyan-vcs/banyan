# 06 · banyan CLI 命令设计

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

2026-10-25 review 复核（T22c）：状态模型确认**无全局仓库配置**——一切工作区状态在 `.banyan/`（config.json + manifest.json + 可清缓存；磁盘足迹 KB 级，比 .git 轻一个量级），无 ~/.banyan 全局注册表（`BANYAN_SERVER` 环境变量仅为无工作区时 `repo ls` 的服务器地址回退）。补齐 .git 同款语义：**向上工作区发现**——任意子目录调用命令都会沿目录树向上找到 `.banyan/config.json` 锚点，子目录 status/commit 作用于整个工作区（与 git 子目录行为一致）；工作区之外报明确错误（exit 2）。
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
| `banyan snapshot` | `git stash`（快照式本地状态） | M4 ·**已落地**（§3.7，T91） |
| `banyan stack` | 栈式变更（stacked changes） | M4 ·**已落地**（§3.8，T92） |
| `banyan sync` / `merge` / `rebase` | 服务端合并/重放（子树对根对齐） | sync **已落地**（§3.9，T93）；merge/rebase 待建 |
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
→ {head, root_view}    # root_view = 织入后根视图 OID（CI/webhook 定位用，docs/01 §3.2）
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
[05-service-api.md](05-service-api.md)。

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

### 3.6 批量导入（2026-10-27 落地，T59）

```
banyan import <local-repo> <url> [--branch main] [--segment-bytes 3G] [--dry-run]
```

存量大仓迁移工具（docs/03 §5 分批导入的编排层）：把本地 git 仓库按拓扑序
切成字节预算内的段、逐段 `git push` 到 banyan——每段一次快进，服务端流式
摄入、内存 ∝ 段大小而非仓大小；碎片在多 pack 模型下是一等公民（不 repack）。
断点续传天然成立：重跑从远端已有 tip 起算（分叉则明确拒绝）；远端不存在
即全新导入（首推自动建仓）。段规划用 `rev-list --objects | cat-file
--batch-check` 度量排他对象字节、指数探测扩张（实现细节：batch-check 不带
`%(rest)` 时整行即对象名，树/blob 行须裁到首字段）。源仓只读不改。

### 3.7 快照 checkpoint/restore（2026-11-08 落地，T91）

```
banyan snapshot [save] [-m msg] [-n name]     # 存档当前工作态（默认名 snap-<N>）
banyan snapshot ls | apply <name> [--force] | drop <name>
```

语义是**时间机**而非 stash-apply：save 把脏集（M/D/??）字节原样拷进
`.banyan/snapshots/<name>/files/`（`snapshot.json` 同时记录当时的 manifest 全量
拷贝），工作树不动——不隐含回退；apply 把工作区**逐字节还原到快照时刻**：脏
文件取镜像字节、快照时干净的文件按 OID 从内容寻址缓存取（缓存被清则向服务端
回填），快照之后才 tracked 的文件本地消失（服务端仍持有），apply 从不销毁快
照外的 untracked 字节。脏树上 apply 拒绝（exit 1，`--force` 覆盖）。基漂移
（快照后发生过 commit/pull）时 apply 会把 manifest 指针拨回旧视图并提示：还原
态必须先 commit 才能 pull 保住——要把旧改动**叠加**到新头，用 stack（§3.8）。

与 git stash 的偏差（有意）：不自动回退工作树、apply 不做三方合并、untracked
默认捕获。纯本地离线（仅"缓存被清后的干净文件回填"一条路径需网络）。

### 3.8 栈式变更 stack（2026-11-08 落地，T92）

```
banyan stack push -m <msg> [path...]     # 捕获选中脏路径为一个变更集条目并本地回退
banyan stack ls | pop | drop <id>        # 检视 / 顶条写回工作树 / 弃条
banyan stack commit [<id>]               # 逐条落台（默认全部，oldest-first）
```

本地变更队列 + 按路径分片：push 把选中的脏路径（精确匹配或目录前缀；无参数
= 全部，含 untracked——banyan 无 ignore 清单，与 status/snapshot 同口径）存为
一条 changeset（字节进 `.banyan/stack-blobs/` 内容寻址库，独立于可清缓存），
**并把这些路径本地回退**（tracked 从 manifest OID 物化，untracked 删除——字节
已在栈里）。commit 经普通 `/api/v1/commit` 逐条落台并**把内容写回工作树**
（checkout 与 manifest 一起前进，runCommit 同款契约）；changeset 是路径级重
放，服务端在当前头上重组——基被动过时首条即 `CONFLICT_BASE_MOVED`（exit 1，
未落地条目原样保留），`pull` 后重试即在新基上全部落地，无需 rebase。

顺带补齐了"部分提交"：`stack push -m x -- path` 即按路径分片暂存，落台时机自
选。边界：pop 在受影响路径当前有改动时拒绝（exit 1）；无 ignore 机制（见
上）；条目不跨工作区（`.banyan/` 局部状态，docs/06 §2 立场）。

### 3.9 子树→根显式对齐 sync（2026-11-08 落地，T93）

```
banyan sync        # 仅子树工作区；服务端重放 + 自动 pull
```

docs/02 §12 的"真分叉 → root 侧等待显式 sync"落地：薄客户端没有历史对象，
rebase 只能在服务端做——`POST /api/v1/sync`（与 commit 同推送授权门）把子树
时间线的**本地段**（自头回溯至投影祖先=合流点；仅线性链）逐提交三方重放
（internal/merge，与收敛列车同引擎）到根当前投影尖，先推进 Base（织入本地带
收窄到重放段）再 CAS 时间线头，并重置 /.woven 标记族——换头=回退形，分歧期
已折入 /.woven 的旧折痕若不清，重放 subject 会被重复折入。重放后的链正是
push 守卫第三形态（docs/02 §13"rebase 恢复后持投影跨视图重写"）。CLI 随后走
普通 pull 物化。

判定（后裔规则同源）：本地含投影（ahead）或投影含本地（落后——pull 即 FF）
→ `uptodate`；变更已经由 peer 的 pull 吸收进根本地链（吸收孪生）→ treeKnown
复用 → `uptodate`。

**语义边界**：真分歧冲突 = 409 `CONFLICT_SYNC_CONFLICT`（具名提交+路径，
exit 1），v1 无半程续传（`--continue` 形态登记 v2）；人工路径 = git 克隆根仓
库、为冲突路径推一个使本地 diff 可净应用的和解读提交（或改写本地侧），再
sync。本地段含 merge 提交 → 400 `SYNC_UNSUPPORTED`（exit 2）。根工作区 →
exit 2。**何时遇到真分歧**：所有顺序入口都有守卫（thin commit 的
base-moved、子树 push 的投影守卫、root push 的织入尖守卫）——该状态只产生
于守卫过检后、CAS 落地前的并发窗口；sync 即该窗口的收口工具。
