# Banyan（榕树）

一个 **git 兼容**的版本控制系统服务端，采用"目录锚定时间线（anchored timeline）"数据模型与"分片 pack CAS + 中心元数据"的分布式存储架构。

> 名字取意**独木成林**：一棵榕树即一片森林——主干是根时间线，气生根垂地自成支撑却同属一树，正如目录时间线独立成仓库、锚定同一根系统。

> 灵感来源：[Redesigning source control at Anthropic](https://www.anthropic.com/engineering/redesigning-source-control)（Scion）——"git 服务端不必和 git 客户端一样工作"。

## 设计目标

1. **完全兼容 git 命令行**：标准 `git clone / fetch / push` 直接可用（Smart HTTP v1 + receive-pack）。
2. **任意子目录可 clone 为独立仓库**：不是 partial clone 的"稀疏检出"，而是每个目录拥有自己的 commit timeline，`/r/{repo}/tree/{subdir}` 即一个语义完整的仓库。
3. **新的核心数据模型**：tree/blob 存储与 commit 存储分离；commit 可锚定（anchor）任意 tree；目录嵌套关系将各层 timeline 自底向上组装成整棵树。
4. **分布式 pack 存储**：packfile 为不可变分片单元，按内容哈希分片到多存储节点，idx 实现分布式对象检索，后台异步 repack。
5. **全新 CLI（`banyan`）**：对标 Scion 的 `sl`，读操作走服务端集中计算 RPC（log/blame/diff/file-history），本地保持 git 兼容仓形态。

## 非目标（当前阶段）

- 不实现 protocol v2（v1 足以服务标准 git 客户端，客户端自动协商回退）。
- 不自造客户端对象格式：`banyan` CLI 的本地仓就是标准 git 仓 + promisor remote。
- 不做 ACL / 多租户 / Web 界面。

## 文档索引

| 文档 | 内容 |
|---|---|
| [docs/00-overview.md](docs/00-overview.md) | 愿景、Scion 思想映射、总体架构 |
| [docs/01-data-model.md](docs/01-data-model.md) | **核心**：目录锚定时间线模型、组装算法、git 投影 |
| [docs/02-storage.md](docs/02-storage.md) | 分布式 pack 存储：分片 CAS、双 idx 体系、repack |
| [docs/03-git-compat.md](docs/03-git-compat.md) | git 兼容层：Smart HTTP、虚拟子树仓库、服务端 filter |
| [docs/04-derived-data.md](docs/04-derived-data.md) | 派生数据：commit-graph、路径历史、blame、记忆化 |
| [docs/05-cli.md](docs/05-cli.md) | `banyan` CLI 设计（对标 `sl`） |
| [docs/06-roadmap.md](docs/06-roadmap.md) | 里程碑与演进路径 |

## 代码骨架

```
cmd/banyand/       服务端守护进程
cmd/banyan/        新 CLI
internal/model/   核心数据模型（anchored commit / timeline / assembly / projection）
internal/storage/ 分布式 pack 存储（shard / metastore / packfile / repack）
internal/gitwire/ git 兼容层（smart HTTP v1 / upload-pack / receive-pack / filter）
internal/rpc/     banyan CLI 的服务端 RPC
internal/derived/ 派生数据引擎（接口占位）
```

## 快速开始（M1：git server MVP 可用）

**测试约定**：所有测试仓库与服务端数据统一放在 `F:\Workspace\test\`（样本仓库 `go-git`、根/子树 clone、小仓库、服务端数据 `banyand-data/`）。

```bash
go build ./... && go vet ./... && go test ./...

# 启动服务端（单机模式：内存元数据 + 本地盘分片；重启即失，持久化见 M2）
go run ./cmd/banyand --listen 127.0.0.1:8097 --root F:/Workspace/test/banyand-data

# 任意 git 仓库推上来（首次 push 自动建仓）
git remote add banyan http://127.0.0.1:8097/r/myrepo.git
git push banyan main

# 根仓库 clone（零翻译投影）
git clone http://127.0.0.1:8097/r/myrepo.git

# ★ 任意子目录 clone 为独立仓库——完整历史（DAG 版 subtree split，
#   合并拓扑保留），git log / git show / git fsck 全部可用
git clone http://127.0.0.1:8097/r/myrepo.git/tree/plumbing/format/packfile
```

已用 go-git 仓库实测（4043 提交 / 28649 对象）：根 clone HEAD 逐字节一致；`plumbing/format/packfile` 子树 clone 得 521 个提交（`--full-history` 口径）、工作树即该目录内容、`git fsck` 干净。详见 [docs/06-roadmap.md](docs/06-roadmap.md)。

## 开发与提交约定

阶段性提交、可独立回滚：

- **按层拆分**（自底向上）：`docs` → `model` → `storage` → `gitwire` → `serve`（cmd/rpc/derived），每个代码提交**独立可构建、单测通过**（`go build ./... && go test ./...`）；
- **里程碑必提交**：每完成一个可验证的能力（如 M1 端到端、索引加速）即提交，出问题可 `git revert` / `git checkout` 回滚到上一个稳定点；
- 提交信息格式：`层: 动作描述（关键点）`，如 `storage: 分片 pack 存储与三层索引`。

## 技术基座

- Go + [go-git](https://github.com/go-git/go-git)（对象模型、packfile/idx 编解码、协议 v1 库）
- 自研：服务端 filter、目录时间线模型、分布式存储、虚拟仓库视图、smart HTTP 层、`banyan` CLI

go-git 能力边界调研结论见 [docs/00-overview.md](docs/00-overview.md#基座选型go-git)。
