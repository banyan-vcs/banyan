<p align="center"><img src="docs/assets/avatar.png" width="120" alt="banyan" /></p>

# Banyan（榕树）

[English](README.md) | **简体中文**

一个 **git 兼容**的版本控制系统服务端：目录锚定时间线（anchored timeline）数据模型
+ "分片 pack CAS + 中心元数据"的分布式存储架构。

> 名字取意**独木成林**：一棵榕树即一片森林——主干是根时间线，气生根垂地自成支撑
> 却同属一树，正如目录时间线独立成仓库、锚定同一根系统。

## 为什么做 banyan

git 是行业标准，banyan 不要求任何人离开 git；替换的是协议背后的存储服务端：

- 大仓库在 git 上不好过：Android 级 monorepo 体积数十 GB、对象数百万、
  clone 以小时计；shallow / sparse / partial clone 只缓解症状，
  "可达即全量"的底层假设未变。
- monorepo 需要目录级权限：部门边界、合规隔离、外包可见性都要落到
  目录粒度；git 权限止步于仓库粒度，"一个目录拆一个仓"又摧毁了
  monorepo 的完整性。
- AI Agent 把协作节奏从"人天级"推到"Agent 分钟级"，git 集中式 ref
  模型的瓶颈被放大：push 锁、全量协商、force push / rebase 级联。

答案是两个结构性动作：**分片 pack CAS + 中心元数据**（存储横向扩展、
不绑定单机）与**目录锚定时间线**（每个目录拥有自己的提交历史——任意
子目录即仓库、传输 O(子目录)、零跨目录锁、权限边界即时间线边界）。

远景：代码存储成为像数据库一样独立的基础设施层——任何平台、客户端、
规模，以标准协议访问同一存储层（docs/design/big-picture.md）。

## 实测性能

同机对真 `git http-backend`，直接在 **git.git 上实测**（8.5 万提交、
2,031 refs、324 MiB packs）——方法与全表见
[docs/operations/performance.md](docs/operations/performance.md) §3b：

- **全量克隆：字节 1.13×，warm 耗时持平（0.99×）**——合笼单包 verbatim
  直传，零枚举、零重编码。
- **浅克隆：耗时 0.61× git**（字节 1.08×）。
- **广告：2,031 refs 字节 0.99×**。
- **子树克隆：真实仓上仅全量字节的 35%**——git 没有的形态。
- **有界内存服务**：旗舰克隆峰值堆 2.4GB（6GB 软限下）；包服务窗口本身
  恒定内存（72MiB 三包克隆峰值 12.3MiB，64MiB 软限集成测试钉住）。

## 架构

<p align="center">
  <img src="docs/assets/architecture-simple.zh.svg" width="800" alt="banyan 简化架构：浏览器接入无状态 web 层（banyand web——登录、导入、能力档位）；git、GitLab 与薄 CLI 客户端直达无状态网关；refs 服务为唯一串行点，旁边是散布 pack 存储；目录时间线读时织入根仓库" />
</p>

支撑整个设计的四个关键机制：

- **目录锚定时间线** —— 每个目录拥有自己的提交历史：任何子目录都是语义完整的仓库，
  权限边界就是时间线边界。
- **读时织入** —— push 只 CAS 一条目录时间线（跨目录零锁）；根仓库在读时线性织出，
  写路径永不合并。
- **分片 pack CAS + 中心元数据** —— 不可变内容寻址分片散布在 N 个存储节点、R 副本；
  refs 服务是唯一串行点，字节面零共识。
- **薄前端，同一核心** —— git Smart HTTP v1/v2 + SSH、Gitaly gRPC（163 个 RPC，GitLab
  直替）与 JSON API 全部委托给同一个协议无关的服务核心。

## 设计目标

1. **完全兼容 git 命令行**：标准 `git clone / fetch / push` 直接可用
   （Smart HTTP v1 + v2 fetch 命令面，docs/gateway/git-compat.md §9b）。
2. **任意子目录即仓库**：`/r/{repo}/tree/{subdir}` 是一个带完整投影历史的
   仓库；不是稀疏检出。
3. **时间线优先写路径（v5）**：一次 push 只 CAS 一条时间线；根视图读时织入，
   写路径永不合并。
4. **分布式 pack 存储**：不可变内容寻址分片 + 中心元数据索引，服务端
   repack 合并碎片并做 delta 编码。
5. **薄客户端（`banyan`）**：工作区只有检出清单 + 内容寻址缓存，从不下载
   提交闭包；log/blame/diff 由服务端经 JSON API 回答。

## 当前能力

以下全部已落地，由 **113 个顶层集成测试函数**以真实 git CLI 钉住
（[docs/operations/testing.md](docs/operations/testing.md)）；GitLab 模式各条另经真 GitLab-CE 活体验证
（[docs/integration/gitaly-compat.md](docs/integration/gitaly-compat.md) §8、[docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md)）。

**git 协议面**——普通 git 客户端能做什么：

| 能力 | 说明 |
|---|---|
| 根/子树 `clone / fetch / push` | Smart HTTP v1 + v2（[docs/gateway/git-compat.md](docs/gateway/git-compat.md)） |
| git-over-SSH | `ssh://` + 公钥认证，v0 有状态协商 + v2；同一服务核心、同一授权 |
| partial clone | `--filter=blob:none` / `tree:0`，按 OID 惰性取回 |
| 浅克隆 | depth / deepen / unshallow |
| tags 与任意 `refs/<ns>` 命名空间 | 原样存储与广告 |
| push options | `-o` 选项段协商与解析 |
| 高效传输 | 合笼单包 verbatim 直传；增量 fetch delta + 真 thin pack——git.git 实测：全量克隆字节 1.13×、浅克隆耗时 0.61×（[docs/operations/performance.md](docs/operations/performance.md) §3b）；have 减除 |

**目录锚定存储**——数据模型本身：

| 能力 | 说明 |
|---|---|
| 任意子目录即仓库 | `/r/{repo}/tree/{subdir}`，完整投影历史——`log / show / fsck` 全可用 |
| 目录时间线 | 一次 push 只 CAS 一条时间线——零跨目录锁；根视图读时织入（v5） |
| 目录级工作流（M4） | 同目录队列 + 服务端收敛：突发零拒绝、冲突按票隔离；`banyan snapshot` / `stack` / `sync`（[docs/design/workflow.md](docs/design/workflow.md)、[docs/client/cli.md](docs/client/cli.md) §3.7-3.9） |
| 个人分支命名空间 | `refs/heads/<user>/*` 仅本人可见——monorepo 分支过多解法 |
| repack | 碎片合并 + delta 编码 |
| 有界内存服务 | 精确命中 verbatim 直传、类型化枚举 + 逐对象窗口编码，任意仓规模（[docs/storage/storage.md](docs/storage/storage.md) §3） |

**平台集成**——GitLab 直接替换：

| 能力 | 说明 |
|---|---|
| gitaly gRPC 前端 | 协议 v19.5.2，**163/163 RPC**——Operation/Ref/Commit/Diff/Blob 全服务、praefect（cluster-of-one）、备份、bundle、历史重写、sidechannel 传输（[docs/integration/gitaly-compat.md](docs/integration/gitaly-compat.md)） |
| GitLab-CE 跑在 banyan 存储上 | 建项目、HTTP **与** SSH 推拉、Web 浏览、MR 合并、wiki、归档——全部经真 GitLab-CE 19.5 活体验证（[docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md)） |
| 推送策略门 | 保护分支等 ref 级策略经 `/internal/allowed` 强制，Rails 拒绝原话透传到 git 客户端 |
| 平台回调 | post-receive 适配器：活动事件、pipeline、缓存过期、MR-from-push（`-o merge_request.create` 活体实证） |
| 子树投影 ref | `refs/banyan/tree/<dir>`——一个 GitLab URL、一套凭证取推任意子树；推送向上装配回根视图 |
| 子树管理控制面 | GitLab 域名下 `/-/banyan/`（`banyand web` gitlab 档位，只透出 banyan 子树功能）：子树清单/活动/克隆配方、运行时访问规则；GitLab SSO 登录（[docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md) §7b） |
| 推送大小上限 | GitLab Maximum push size 设置经 `receive.maxInputSize` 强制 |

**生产与运维**：

| 能力 | 说明 |
|---|---|
| HA 集群 | 网关 + refs 服务 + 散布多副本 pack：节点故障恢复、仓库级动态副本（[docs/storage/ha.md](docs/storage/ha.md)；当前本地盘集群，MySQL/raft refs → HA-2） |
| 授权 | 仓库/目录/分支粒度的下载与提交控制（[docs/gateway/authorization.md](docs/gateway/authorization.md)；demo 策略，opt-in） |
| PAT | sha256 落库、明文只现一次、过期/吊销读取时判定；scope 是上限不是旁路——TokenPolicy 装饰任意策略，RPC 授权同缝 |
| 生产姿态 | 安全默认启动、元数据默认进数据库、优雅 drain、每角色 `/metrics`（P0，[docs/project/roadmap.md](docs/project/roadmap.md)） |
| 服务端 diff 与 blame | 路径 diff、blame RPC 与 `git diff` / `git blame` 逐字节一致 |
| 存储寻址 | named 可读 / hashed 不透明，rename 零存储动作（[docs/storage/storage.md](docs/storage/storage.md) §6） |
| 服务层 + JSON API | `/api/v1/*` 统一错误信封；SQLite / MySQL / Postgres 元数据 + YAML 配置 |
| web 层（`banyand web`） | 三层独立站部署（web → gateway → 存储）：自己的数据库（sqlite / mysql / postgres，可与 gateway 共用——demo 即如此）；本地登录 + OAuth（GitHub 与多个 GitLab 实例并存；gateway 侧 `auth.oauth`）；SPA 托管（[docs/web/web-layer.md](docs/web/web-layer.md)） |
| 仓库导入 | 按 URL 从 github.com / gitlab.com / 私有 GitLab 导入，会话 token 或粘贴 PAT；服务端 fetch 走与 push 相同的入库路径，pack 流式落盘（[docs/web/web-layer.md](docs/web/web-layer.md) §4） |
| 客户端与界面 | 薄工作区 CLI（`banyan`）、React Web 控制台（代码浏览、历史/diff/blame、分支、token、子树、导入）、产品控制台（`banyand console`，子树作用域角色——[docs/web/console.md](docs/web/console.md)） |
| 运维面 | pprof 全家、运行时快照、服务路径计数器 |

薄 CLI 的提交链路经 fresh git clone + `fsck` 交叉验证；gitaly 前端经
git 自身答案对拍（T60）、workhorse 形态传输模拟器（T61）、真 clone 所乘的
多路复用 sidechannel 传输（T62）交叉验证，diff 面与 git diff 逐字节对比（T63）。

## 代码结构

```
cmd/banyand/        服务端守护进程（角色 + 子命令：serve / web / console / bench）
cmd/banyan/         薄工作区 CLI
internal/model/     核心数据模型（anchored commit / timeline / assembly / projection）
internal/meta/       元数据面（MetaStore：memory / SQL / sqlite、refs 客户端、
                    PAT、子树规则、寻址映射）
internal/storage/   对象面（分片 pack 存储：shard / packfile / index / repack）
internal/view/      读时视图引擎（v5 织入、子树视图）
internal/gitwire/   git wire 协议（smart HTTP v1 / upload-pack / receive-pack）
internal/gitaly/   gitaly gRPC 前端壳（鉴权/生命周期）+ 按协议服务家族的子包
                    （repository/commit/ref/object/operations/transport/backup/
                    node + kit 共享层），GitLab 直接替换，docs/integration/gitaly-compat.md
internal/gitalypb/ vendored gitaly 协议（v19.5.2，生成代码）
internal/service/   协议无关服务层（三前端共核心，docs/gateway/service-api.md）
internal/rpc/       httpd JSON 前端（/api/v1）
internal/importer/  服务端远程 fetch（仓库导入 + gitaly FetchRemote）
internal/webapp/    web 层（`banyand web`：会话库、OAuth、导入任务、SPA 托管）
web/                React 控制台（Vite SPA，由 web 层提供服务）
internal/cli/       薄工作区客户端实现
internal/config/    三层配置（默认 < YAML < 显式标志）
```

## 快速开始

### Docker 一键启动

```bash
docker compose up -d          # 构建并启动；数据持久化在 banyan-data 卷
                              # demo 默认开启授权（docs/gateway/authorization.md）：
                              #   Web 界面 http://localhost:8101（alice / bob / admin，密码 demo）
                              #   git      http://alice:demo@localhost:8100/r/demo.git
                              #   git ssh  ssh://alice@localhost:8102/r/demo.git（demo/ssh/ 密钥）

# 推任意 git 仓库上来（demo 策略里 main 仅 admin 可推）——
# 想象一个 Android 布局的大仓：frameworks/base、system/core、external/skia
git remote add banyan http://admin:demo@localhost:8100/r/demo.git
git push banyan main

# git-over-SSH 同样可用（一次性 demo 密钥在 demo/ssh/，docs/operations/docker-demo.md §3b）
git clone ssh://alice@localhost:8102/r/demo.git \
  -c core.sshCommand="ssh -i demo/ssh/alice -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

git clone http://alice:demo@localhost:8100/r/demo.git            # 整个大仓
git clone http://alice:demo@localhost:8100/r/demo.git/tree/frameworks/base  # 只取其中一个项目

# 薄工作区：最小化下载、无 .git、历史在服务端——
# CLI 的 token 登录是路线图项（M5，docs/client/cli.md），目前需指向
# 免认证服务端：下方"源码构建"的 -dev 形态即是可用路径
```

受限网络可走镜像构建：
`docker compose build --build-arg GOPROXY=https://mirrors.aliyun.com/goproxy/`。

### GitLab-CE 跑在 banyan 存储上

一台真 GitLab，唯一的 gitaly 存储节点换成 banyan——建项目、HTTP push、
Web 界面浏览全部由 banyan 的 gitaly gRPC 前端服务（docs/integration/gitaly-compat.md，兼容层的
活体验收）：

```bash
docker compose -f demo/gitlab/docker-compose.yml up -d   # GitLab-CE 19.5 + banyan（约 4 GiB）
demo/gitlab/provision.sh                                  # 种子、推送、读面走查
# GitLab 界面 http://localhost:8180 —— 每个 git 字节都落在 banyan
# 子树管理 http://localhost:8180/-/banyan/ —— GitLab SSO 登录
```

接入已有 GitLab-CE（配置、验证清单、故障排查）见
[docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md)。

### HA 集群 demo

一条命令启动拆分部署（docs/storage/ha.md）：refs 服务 + 4 个存储节点 + 无状态网关——
pack 散布在存储节点上各 3 副本；杀掉一个存储节点，读写继续，reconciler
自动恢复副本目标。

```bash
bash demo/cluster-demo.sh   # 完整走查：push → 杀节点 → 恢复 → 动态调 R
```

也可手动：`docker compose -f docker-compose.cluster.yml up -d`，推送到
`http://localhost:8100/r/demo.git`，观察 `GET /api/v1/cluster/nodes`，
`docker stop banyan-s4` 后继续 push——后台 reconciler 会自动把副本补回
3（已在 Docker 29.x 实测；相同链路由集成测试 T23–T26 进程内钉住）。

### 源码构建

```bash
go build ./... && go vet ./... && go test ./...   # ~65s（集成测试需 PATH 中有 git）

# 启动服务端。配置三层解析：内置默认 < YAML（-config）< 显式标志；
# 未知配置键直接报错。-dev = 免认证本地模式——生产姿态下无认证
# 拒绝启动（见 banyan.example.yaml）。
go run ./cmd/banyand -dev -listen 127.0.0.1:8097 -root ./banyand-data -meta sqlite

# 任意 git 仓库推上来（首次 push 自动建仓）
git remote add banyan http://127.0.0.1:8097/r/myrepo.git
git push banyan main

# 根仓库 clone（HEAD 逐字节一致，已实测 4k+ 提交仓库）
git clone http://127.0.0.1:8097/r/myrepo.git

# ★ 任意子目录 clone 为独立仓库——完整投影历史，
#   git log / git show / git fsck 全部可用。Android 布局的大仓里，
#   取一个项目的历史，不用拉走其余几十 GB。
git clone http://127.0.0.1:8097/r/myrepo.git/tree/frameworks/base

# 薄工作区：最小化下载、无 .git、历史在服务端
go run ./cmd/banyan clone http://127.0.0.1:8097/r/myrepo.git
cd myrepo
go run ../cmd/banyan status        # 离线：manifest 对比工作区
echo change >> README && go run ../cmd/banyan commit -m "edit"   # 服务端组装提交
go run ../cmd/banyan pull          # 服务端 tree-diff 增量
go run ../cmd/banyan log           # 远端历史，零下载
```

## 非目标（当前阶段）

- push 恒走 v0（git 本身从未为 receive-pack 定义 v2）；fetch 侧原生 v2（ls-refs + fetch 命令面，docs/gateway/git-compat.md §9b）。
- 不做 DevOps 平台能力（MR/CI/权限界面）——banyan 是**存储层**，协作留在已有
  平台（docs/design/big-picture.md）。
- 不做 praefect 复制协议；多副本走 banyan 自身分片路线（docs/storage/storage.md）。

## 文档索引

阅读顺序：先总览与定位，再按层拆解核心架构，然后横切能力，最后运行与演进。

**总——banyan 是什么、站在哪里**：

| 文档 | 内容 |
|---|---|
| [docs/design/overview.md](docs/design/overview.md) | 愿景、设计原则、总体架构图 |
| [docs/design/big-picture.md](docs/design/big-picture.md) | 定位：通用代码存储层 + 平台集成拓扑 |

**核心设计——按层自底向上（模型 → 存储 → 协议 → 服务 → 客户端）**：

| 文档 | 内容 |
|---|---|
| [docs/design/data-model.md](docs/design/data-model.md) | **核心**：目录锚定时间线模型、组装算法、git 投影 |
| [docs/storage/storage.md](docs/storage/storage.md) | 分布式 pack 存储：分片 CAS、双 idx 体系、repack |
| [docs/gateway/git-compat.md](docs/gateway/git-compat.md) | git 兼容层：Smart HTTP、SSH 传输、虚拟子树仓库 |
| [docs/gateway/service-api.md](docs/gateway/service-api.md) | 服务层抽象 + httpd/Gitaly 前端与 API 契约 |
| [docs/client/cli.md](docs/client/cli.md) | 薄客户端 CLI 命令设计 |

**横切能力**：

| 文档 | 内容 |
|---|---|
| [docs/gateway/authorization.md](docs/gateway/authorization.md) | 授权模型（含安全默认启动）、个人分支可见性、Web 演示 |
| [docs/design/derived-data.md](docs/design/derived-data.md) | 派生数据：commit-graph、路径历史、blame |
| [docs/storage/indexing-packing.md](docs/storage/indexing-packing.md) | 索引（代数）与打包（have 减除/delta/repack）设计 |
| [docs/design/workflow.md](docs/design/workflow.md) | M4 设计：同目录提交队列 + 分歧收敛（merge train） |
| [docs/web/console.md](docs/web/console.md) | 产品侧控制台（`banyand console` 子命令）：组/项目路由、PAT 认证、成员角色 |

**运行与演进**：

| 文档 | 内容 |
|---|---|
| [docs/storage/ha.md](docs/storage/ha.md) | 高可用：一体化 vs 网关 + 集群、仓库级副本 |
| [docs/operations/performance.md](docs/operations/performance.md) | 性能方案：瓶颈清单、优化项、SLO 基准门、实测报告（§3a 合成种子；§3b 真实仓含 git.git） |
| [docs/operations/docker-demo.md](docs/operations/docker-demo.md) | Docker demo 走查：授权、SSH、Web、集群栈指引 |
| [docs/operations/testing.md](docs/operations/testing.md) | 集成测试矩阵 |
| [docs/project/roadmap.md](docs/project/roadmap.md) | 里程碑与生产主轴 |
| [docs/integration/gitaly-compat.md](docs/integration/gitaly-compat.md) | Gitaly gRPC 兼容层：覆盖矩阵（163 RPC）与设计账本 |
| [docs/integration/gitlab-integration.md](docs/integration/gitlab-integration.md) | GitLab 集成指南：接入步骤、验证清单、故障排查 |

## 开发与提交约定

- **按层拆分提交**（自底向上）：`docs → model → storage → gitwire → service →
  cli`，每个代码提交**独立可构建、测试通过**；
- **新能力必配集成用例**并登记 [docs/operations/testing.md](docs/operations/testing.md)；断言用 testify
  （require=场景门，assert=独立检查组）；
- **git 是行为基准**（git 源码为唯一 oracle）；go-git 只是编解码库，边界收敛在
  `internal/storage/packfile.go`。

## 技术基座

- Go + [go-git](https://github.com/go-git/go-git)（对象模型、packfile/idx 编解码——
  只作库，不作为行为边界）
- 自研：时间线模型、读时织入、分片存储、虚拟仓库、Smart HTTP 层、服务层、薄 CLI

## 许可

核心为 **MIT**（[LICENSE](LICENSE)）——覆盖 enterprise/ 之外的一切。
企业能力是独立 Go module，按
[enterprise/LICENSE.enterprise](enterprise/LICENSE.enterprise) 授权：
开发与测试免费，生产使用需订阅。核心永不 import enterprise，只有
enterprise 引用核心。
