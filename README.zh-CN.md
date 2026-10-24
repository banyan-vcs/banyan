# Banyan（榕树）

[English](README.md) | **简体中文**

一个 **git 兼容**的版本控制系统服务端：目录锚定时间线（anchored timeline）数据模型
+ "分片 pack CAS + 中心元数据"的分布式存储架构。

> 名字取意**独木成林**：一棵榕树即一片森林——主干是根时间线，气生根垂地自成支撑
> 却同属一树，正如目录时间线独立成仓库、锚定同一根系统。

## 设计目标

1. **完全兼容 git 命令行**：标准 `git clone / fetch / push` 直接可用（Smart HTTP
   v1；v2 客户端自动协商回退）。
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
| 根/子树 `git clone/fetch/push`（Smart HTTP v1） | ✅ 真实 git 客户端实测 |
| 子树虚拟仓库（`/tree/<path>`，完整历史投影） | ✅ |
| 目录时间线 + 线性织入根视图（v5） | ✅ |
| 浅克隆（depth / deepen / unshallow） | ✅ |
| have 减除增量 fetch | ✅ |
| repack：碎片合并 + delta | ✅ |
| tags 与任意 `refs/<ns>` 命名空间 | ✅ |
| SQLite 元数据持久化 + YAML 配置 | ✅ |
| 服务层 + JSON API（`/api/v1/*`，统一错误信封） | ✅ |
| 薄工作区 CLI（`clone/status/commit/pull/log/cat/...`） | ✅ |
| token 认证 + 建仓收紧、diff/blame RPC | 🚧 下一步（M5 / M2–M3） |
| Gitaly gRPC 前端（作为 GitLab 底层存储节点） | 🚧 规划中（docs/10 §4） |

23 个集成用例以**真实 git CLI** 作客户端钉住行为（[docs/07](docs/07-testing.md)）；
薄 CLI 的提交链路经 fresh git clone + `fsck` 交叉验证。

## 代码结构

```
cmd/banyand/        服务端守护进程（git smart HTTP + JSON API）
cmd/banyan/         薄工作区 CLI
internal/model/     核心数据模型（anchored commit / timeline / assembly / projection）
internal/storage/   分布式 pack 存储（shard / metastore / packfile / index / repack）
internal/view/      读时视图引擎（v5 织入、子树视图）
internal/gitwire/   git wire 协议（smart HTTP v1 / upload-pack / receive-pack）
internal/service/   协议无关服务层（三前端共核心，docs/10）
internal/rpc/       httpd JSON 前端（/api/v1）
internal/cli/       薄工作区客户端实现
internal/config/    三层配置（默认 < YAML < 显式标志）
```

## 快速开始

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

- 不实现 protocol v2（v1 足以服务标准 git 客户端）。
- 不做 DevOps 平台能力（MR/CI/权限界面）——banyan 是**存储层**，协作留在已有
  平台（docs/08）。
- 不做 praefect 复制协议；多副本走 banyan 自身分片路线（docs/02）。

## 文档索引

| 文档 | 内容 |
|---|---|
| [docs/00-overview.md](docs/00-overview.md) | 愿景、设计原则、总体架构 |
| [docs/01-data-model.md](docs/01-data-model.md) | **核心**：目录锚定时间线模型、组装算法、git 投影 |
| [docs/02-storage.md](docs/02-storage.md) | 分布式 pack 存储：分片 CAS、双 idx 体系、repack |
| [docs/03-git-compat.md](docs/03-git-compat.md) | git 兼容层：Smart HTTP、虚拟子树仓库 |
| [docs/04-derived-data.md](docs/04-derived-data.md) | 派生数据：commit-graph、路径历史、blame |
| [docs/05-cli.md](docs/05-cli.md) | 薄客户端 CLI 命令设计 |
| [docs/06-roadmap.md](docs/06-roadmap.md) | 里程碑与演进路径 |
| [docs/07-testing.md](docs/07-testing.md) | 集成测试矩阵 |
| [docs/08-big-picture.md](docs/08-big-picture.md) | 定位：通用代码存储层 + GitLab 等平台集成拓扑 |
| [docs/09-indexing-packing.md](docs/09-indexing-packing.md) | 索引（代数）与打包（have 减除/delta/repack）设计 |
| [docs/10-service-api.md](docs/10-service-api.md) | 服务层抽象 + httpd/Gitaly 双前端与 API 契约 |

## 开发与提交约定

- **按层拆分提交**（自底向上）：`docs → model → storage → gitwire → service →
  cli`，每个代码提交**独立可构建、测试通过**；
- **新能力必配集成用例**并登记 [docs/07](docs/07-testing.md)；断言用 testify
  （require=场景门，assert=独立检查组）；
- **git 是行为基准**（git 源码为唯一 oracle）；go-git 只是编解码库，边界收敛在
  `internal/storage/packfile.go`。

## 技术基座

- Go + [go-git](https://github.com/go-git/go-git)（对象模型、packfile/idx 编解码——
  只作库，不作为行为边界）
- 自研：时间线模型、读时织入、分片存储、虚拟仓库、Smart HTTP 层、服务层、薄 CLI
