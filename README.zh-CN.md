# Banyan（榕树）

[English](README.md) | **简体中文**

一个 **git 兼容**的版本控制系统服务端：目录锚定时间线（anchored timeline）数据模型
+ "分片 pack CAS + 中心元数据"的分布式存储架构。

> 名字取意**独木成林**：一棵榕树即一片森林——主干是根时间线，气生根垂地自成支撑
> 却同属一树，正如目录时间线独立成仓库、锚定同一根系统。

## 为什么做 banyan

**git 是绕不开的行业标准。** 诞生二十余年，git 从个人项目到超大规模企业无处不在，git 工作流早已是整个行业的肌肉记忆，围绕它的工具链、CI 与平台生态无可替代。任何新方案的第一约束由此确定：**不要求任何人离开 git**。

**但 git 并不擅长大型仓库的托管。** Android、Chromium 这类巨型项目早已触到 git 的能力边界：仓库体积以数十 GB 计、对象数以百万计、一次 clone 以小时计。社区用 shallow / sparse / partial clone 缓解症状，但"仓库即单机目录、可达即全量"的底层假设从未改变。

**Monorepo 是企业级代码维护的方向。** 在现代化构建系统（Bazel / Buck 等）之上，**源码依赖是最高效的持续维护方式**——跨包原子变更、无发版延迟、一次提交协同多方。越来越多企业把代码资产收敛进单仓。

**而 Monorepo 必然要求目录级权限。** 单仓共享意味着部门边界、合规隔离、外包可见性都要在**目录粒度**上控制。原生 git 的权限模型止步于仓库粒度；"一个目录拆一个仓库"的模拟又摧毁了 monorepo 的完整性——这是一个原生 Git 无法很好解决的结构性矛盾。

**AI Agent 让矛盾激化。** Agent 对软件开发的渗透持续加深，仓库协作频率从"人天级"跃升到"Agent 分钟级并发"。git 集中式 ref 协作模型（push 锁、全量协商、force push / rebase 级联）的性能瓶颈与协作风险被同步放大。

banyan 的答案：不推翻 git，而是**重新设计 git 兼容的代码存储服务端**——

- **分布式设计为默认形态**：分片 pack CAS + 中心元数据，存储可横向扩展、不绑定单机，从架构上解决代码这一企业核心资产的存储可靠性问题；
- **目录锚定时间线数据模型**：每个目录拥有自己的提交历史，目录级协作（子目录即仓库、O(子目录) 传输、零跨目录锁）与目录级管理（权限边界即时间线边界）成为**原生能力**，而非外围补丁。

**远景**：让代码存储成为像数据库一样**独立的基础设施层**——任何平台（GitLab / Gitea / 自建）、任何客户端（git CLI / IDE / CI / AI Agent）、任何规模（个人项目到企业 monorepo），都以标准协议访问同一个存储层。banyan 管"代码住在哪里、怎么高效取用"；协作流程归平台，组织与权限归企业——目录时间线把两者的边界对齐在同一粒度上。

## 设计目标

1. **完全兼容 git 命令行**：标准 `git clone / fetch / push` 直接可用（Smart HTTP
   v1；v2 已规划——fetch 侧命令面（docs/11），当前 v2 客户端自动回退）。
2. **任意子目录可 clone 为独立仓库**：不是 partial clone 的"稀疏检出"，而是每个
   目录拥有自己的 commit timeline，`/r/{repo}/tree/{subdir}` 即一个语义完整、
   带完整投影历史的仓库。
3. **时间线优先写路径（v5）**：目录 push 只 CAS 本目录时间线——零跨目录锁；根视图
   读时线性织入，无收编 merge 噪音。
4. **分布式 pack 存储**：packfile 为不可变内容寻址分片 + 中心元数据索引；服务端
   repack 合并碎片并做 delta 编码。
5. **薄客户端（`banyan`）**：默认最小化下载——工作区只有检出清单 + 内容寻址缓存，
   从不下载提交闭包；历史查询（log/blame/diff）由服务端经 JSON API 集中回答。

## 当前能力

| 能力 | 状态 |
|---|---|
| 根/子树 `git clone/fetch/push`（Smart HTTP v1 + v2） | ✅ 真实 git 客户端实测 |
| git-over-SSH 传输（`ssh://` + 公钥认证，v0 有状态协商 + v2） | ✅ 同一服务核心与授权（docs/04 §7b） |
| partial clone（`--filter=blob:none`/`tree:0` + 惰性取回） | ✅ |
| serve 路径 delta 包（过 64KiB 阈值 OFS/REF-delta） | ✅ |
| 增量 fetch 真 thin pack（对客户端持有基对象出 REF_DELTA） | ✅（字节 0.87× git，docs/11 §3） |
| 授权：仓库/目录/分支粒度的下载与提交控制（docs/07） | ✅ demo 策略，opt-in |
| 存储寻址双模式：named 可读目录 / hashed 不透明键 + rename（docs/03 §6） | ✅ hashed 模式 rename 零存储动作，T40 |
| M4 目录级工作流：同目录队列 + 服务端收敛（纯 Go merge-ort 子集，docs/15） | ✅ 并发突发零拒绝；冲突票隔离，T41/T42 |
| 个人分支命名空间（refs/heads/<user>/*）仅本人可见 | ✅ monorepo 分支过多解法 |
| Web 界面：仓库管理 + 文件浏览（TypeScript 零运行时依赖） | ✅ web/ |
| 子树虚拟仓库（`/tree/<path>`，完整历史投影） | ✅ |
| 目录时间线 + 线性织入根视图（v5） | ✅ |
| 浅克隆（depth / deepen / unshallow） | ✅ |
| have 减除增量 fetch | ✅ |
| repack：碎片合并 + delta | ✅ |
| tags 与任意 `refs/<ns>` 命名空间 | ✅ |
| SQLite 元数据持久化 + YAML 配置 | ✅ |
| 服务层 + JSON API（`/api/v1/*`，统一错误信封） | ✅ |
| 薄工作区 CLI（`clone/status/commit/pull/log/cat/...`） | ✅ |
| HA 集群：网关 + ref 服务 + 散布多副本 pack 存储、节点故障恢复、仓库级动态副本 | ✅ 核心（本地盘集群，T23-T26；MySQL/raft refs 后端 → HA-2） |
| token 认证 + 建仓收紧、diff/blame RPC | 🚧 下一步（M5 / M2–M3） |
| Gitaly gRPC 前端（作为 GitLab 底层存储节点） | 🚧 规划中（docs/05 §4） |

23 个集成用例以**真实 git CLI** 作客户端钉住行为（[docs/13](docs/13-testing.md)）；
薄 CLI 的提交链路经 fresh git clone + `fsck` 交叉验证。

## 代码结构

```
cmd/banyand/        服务端守护进程（git smart HTTP + JSON API）
cmd/banyan/         薄工作区 CLI
internal/model/     核心数据模型（anchored commit / timeline / assembly / projection）
internal/storage/   分布式 pack 存储（shard / metastore / packfile / index / repack）
internal/view/      读时视图引擎（v5 织入、子树视图）
internal/gitwire/   git wire 协议（smart HTTP v1 / upload-pack / receive-pack）
internal/service/   协议无关服务层（三前端共核心，docs/05）
internal/rpc/       httpd JSON 前端（/api/v1）
internal/cli/       薄工作区客户端实现
internal/config/    三层配置（默认 < YAML < 显式标志）
```

## 快速开始

### Docker 一键启动

```bash
docker compose up -d          # 构建并启动；数据持久化在 banyan-data 卷
                              # demo 默认开启授权（docs/07）：
                              #   Web 界面 http://localhost:8101（alice / bob / admin，密码 demo）
                              #   git      http://alice:demo@localhost:8100/r/demo.git
                              #   git ssh  ssh://alice@localhost:8102/r/demo.git（demo/ssh/ 密钥）

# 推任意 git 仓库上来（demo 策略里 main 仅 admin 可推）
git remote add banyan http://admin:demo@localhost:8100/r/demo.git
git push banyan main

# git-over-SSH 同样可用（一次性 demo 密钥在 demo/ssh/，docs/12 §3b）
git clone ssh://alice@localhost:8102/r/demo.git \
  -c core.sshCommand="ssh -i demo/ssh/alice -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

git clone http://alice:demo@localhost:8100/r/demo.git            # 根仓库
git clone http://alice:demo@localhost:8100/r/demo.git/tree/plumbing  # 任意子目录即仓库

# 薄工作区：最小化下载、无 .git、历史在服务端
go run ./cmd/banyan clone http://localhost:8100/r/demo
# ...或主机无 Go 时：
docker compose exec banyan banyan clone http://127.0.0.1:8080/r/demo /tmp/demo
```

受限网络可走镜像构建：
`docker compose build --build-arg GOPROXY=https://mirrors.aliyun.com/goproxy/`。

### HA 集群 demo

一条命令启动拆分部署（docs/10）：refs 服务 + 4 个存储节点 + 无状态网关——
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
# 未知配置键直接报错。示例见 banyan.example.yaml。
go run ./cmd/banyand -listen 127.0.0.1:8097 -root ./banyand-data -meta sqlite

# 任意 git 仓库推上来（首次 push 自动建仓）
git remote add banyan http://127.0.0.1:8097/r/myrepo.git
git push banyan main

# 根仓库 clone（HEAD 逐字节一致，已实测 4k+ 提交仓库）
git clone http://127.0.0.1:8097/r/myrepo.git

# ★ 任意子目录 clone 为独立仓库——完整投影历史，
#   git log / git show / git fsck 全部可用
git clone http://127.0.0.1:8097/r/myrepo.git/tree/plumbing/format/packfile

# 薄工作区：最小化下载、无 .git、历史在服务端
go run ./cmd/banyan clone http://127.0.0.1:8097/r/myrepo.git
cd myrepo
go run ../cmd/banyan status        # 离线：manifest 对比工作区
echo change >> README && go run ../cmd/banyan commit -m "edit"   # 服务端组装提交
go run ../cmd/banyan pull          # 服务端 tree-diff 增量
go run ../cmd/banyan log           # 远端历史，零下载
```

## 非目标（当前阶段）

- push 恒走 v0（git 本身从未为 receive-pack 定义 v2）；fetch 侧原生 v2（ls-refs + fetch 命令面，docs/04 §9b）。
- 不做 DevOps 平台能力（MR/CI/权限界面）——banyan 是**存储层**，协作留在已有
  平台（docs/01）。
- 不做 praefect 复制协议；多副本走 banyan 自身分片路线（docs/03）。

## 文档索引

阅读顺序即设计展开的顺序：先总览与定位，再按层拆解核心架构，然后是横切
能力，最后是运行与演进。

**总——banyan 是什么、站在哪里**：

| 文档 | 内容 |
|---|---|
| [docs/00-overview.md](docs/00-overview.md) | 愿景、设计原则、总体架构图 |
| [docs/01-big-picture.md](docs/01-big-picture.md) | 定位：通用代码存储层 + 平台集成拓扑 |

**核心设计——按层自底向上（模型 → 存储 → 协议 → 服务 → 客户端）**：

| 文档 | 内容 |
|---|---|
| [docs/02-data-model.md](docs/02-data-model.md) | **核心**：目录锚定时间线模型、组装算法、git 投影 |
| [docs/03-storage.md](docs/03-storage.md) | 分布式 pack 存储：分片 CAS、双 idx 体系、repack |
| [docs/04-git-compat.md](docs/04-git-compat.md) | git 兼容层：Smart HTTP、SSH 传输、虚拟子树仓库 |
| [docs/05-service-api.md](docs/05-service-api.md) | 服务层抽象 + httpd/Gitaly 前端与 API 契约 |
| [docs/06-cli.md](docs/06-cli.md) | 薄客户端 CLI 命令设计 |

**横切能力**：

| 文档 | 内容 |
|---|---|
| [docs/07-authorization.md](docs/07-authorization.md) | 授权模型、个人分支可见性、Web 演示 |
| [docs/08-derived-data.md](docs/08-derived-data.md) | 派生数据：commit-graph、路径历史、blame |
| [docs/09-indexing-packing.md](docs/09-indexing-packing.md) | 索引（代数）与打包（have 减除/delta/repack）设计 |
| [docs/15-workflow.md](docs/15-workflow.md) | M4 设计：同目录提交队列 + 分歧收敛（merge train） |
| [docs/16-benchmark-report.md](docs/16-benchmark-report.md) | 性能实测报告：多轮验证与劣化归因（对 git http-backend） |

**运行与演进**：

| 文档 | 内容 |
|---|---|
| [docs/10-ha.md](docs/10-ha.md) | 高可用：一体化 vs 网关 + 集群、仓库级副本 |
| [docs/11-performance.md](docs/11-performance.md) | 性能方案：瓶颈清单、优化项、SLO 基准门 |
| [docs/12-docker-demo.md](docs/12-docker-demo.md) | Docker demo 走查：授权、SSH、Web、集群栈指引 |
| [docs/13-testing.md](docs/13-testing.md) | 集成测试矩阵 |
| [docs/14-roadmap.md](docs/14-roadmap.md) | 里程碑与演进路径 |

## 开发与提交约定

- **按层拆分提交**（自底向上）：`docs → model → storage → gitwire → service →
  cli`，每个代码提交**独立可构建、测试通过**；
- **新能力必配集成用例**并登记 [docs/13](docs/13-testing.md)；断言用 testify
  （require=场景门，assert=独立检查组）；
- **git 是行为基准**（git 源码为唯一 oracle）；go-git 只是编解码库，边界收敛在
  `internal/storage/packfile.go`。

## 技术基座

- Go + [go-git](https://github.com/go-git/go-git)（对象模型、packfile/idx 编解码——
  只作库，不作为行为边界）
- 自研：时间线模型、读时织入、分片存储、虚拟仓库、Smart HTTP 层、服务层、薄 CLI

## 许可

核心为 **MIT**（[LICENSE](LICENSE)）。商业化能力维护在
[enterprise/](enterprise/)——独立 Go module，按
[enterprise/LICENSE.enterprise](enterprise/LICENSE.enterprise) 授权：
开发与测试免费，生产使用需 banyan 企业订阅。核心永不 import
enterprise，只有 enterprise 引用核心。
