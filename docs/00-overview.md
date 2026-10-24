# 00 · 总览：banyan 是什么

## 初衷：为什么做 banyan

**git 是绕不开的行业标准。** 诞生二十余年，git 从个人项目到超大规模企业无处不在，git 工作流早已是整个行业的肌肉记忆，围绕它的工具链、CI 与平台生态无可替代。任何新方案的第一约束由此确定：**不要求任何人离开 git**。

**但 git 并不擅长大型仓库的托管。** Android、Chromium 这类巨型项目早已触到 git 的能力边界：仓库体积以数十 GB 计、对象数以百万计、一次 clone 以小时计。社区用 shallow / sparse / partial clone 缓解症状，但"仓库即单机目录、可达即全量"的底层假设从未改变。

**Monorepo 是企业级代码维护的方向。** 在现代化构建系统（Bazel / Buck 等）之上，**源码依赖是最高效的持续维护方式**——跨包原子变更、无发版延迟、一次提交协同多方。越来越多企业把代码资产收敛进单仓。

**而 Monorepo 必然要求目录级权限。** 单仓共享意味着部门边界、合规隔离、外包可见性都要在**目录粒度**上控制。原生 git 的权限模型止步于仓库粒度；"一个目录拆一个仓库"的模拟又摧毁了 monorepo 的完整性——这是一个原生 Git 无法很好解决的结构性矛盾。

**AI Agent 让矛盾激化。** Agent 对软件开发的渗透持续加深，仓库协作频率从"人天级"跃升到"Agent 分钟级并发"。git 集中式 ref 协作模型（push 锁、全量协商、force push / rebase 级联）的性能瓶颈与协作风险被同步放大。

## banyan 的答案

不推翻 git，而是**重新设计 git 兼容的代码存储服务端**：

- **分布式设计为默认形态**——分片 pack CAS + 中心元数据，存储可横向扩展、不绑定单机，从架构上解决代码这一企业核心资产的存储可靠性问题；
- **目录锚定时间线数据模型**——每个目录拥有自己的提交历史：目录级协作（子目录即仓库、O(子目录) 传输、零跨目录锁）与目录级管理（权限边界即时间线边界）成为**原生能力**，而非外围补丁。

## 远景

让代码存储成为像数据库一样**独立的基础设施层**：任何平台（GitLab / Gitea / 自建）、任何客户端（git CLI / IDE / CI / AI Agent）、任何规模（个人项目到企业 monorepo），都以标准协议访问同一个存储层。banyan 管"代码住在哪里、怎么高效取用"；协作流程归平台，组织与权限归企业——目录时间线把两者的边界对齐在同一粒度上。（定位展开见 [08-big-picture.md](08-big-picture.md)。）

## 一句话

banyan 是一个 git 兼容的服务端版本控制系统：**客户端仍是标准 git，服务端换掉了数据模型与存储架构**——用"目录锚定时间线"替代"单一 root tree 历史"，用"分片 pack CAS + 中心元数据"替代"单机松散对象 + 本地 repack"。

## 为什么这么做：核心设计判断

六条判断贯穿 banyan 的全部设计：

| 判断 | banyan 的做法 |
|---|---|
| 客户端永远在线 → 服务端集中计算 | `banyan` CLI 的读操作全部走服务端 RPC；git 客户端照旧走 wire 协议 |
| 数据结构决定扩展性 | 目录锚定 commit（tree/blob 与 commit 分离），子目录提交 O(子目录大小) |
| ref 是元数据瓶颈 | timeline head 即元数据表一行，CAS 更新 |
| 获取协议应该是"要什么"而非"有什么" | 服务端逐对象批量 RPC（banyan CLI 懒回填）+ git 侧 partial clone filter |
| 不可变内容可无限缓存 | pack 即不可变分片，daemon/cache/server 三层同构缓存 |
| 服务端 ≠ 客户端架构 | 存储节点无状态分片 + 中心元数据单写者 |

第一性约束：**git 服务端不必和 git 客户端一样工作**——对客户端的承诺只有
git wire 协议语义，服务端内部自由重构。

## 总体架构

```
                    ┌──────────────────────────────────────────────┐
                    │                客户端生态                       │
                    │  标准 git CLI      banyan CLI（薄工作区）      │
                    │  clone/fetch/push  log/blame/diff/懒取对象      │
                    └───────┬─────────────────────┬────────────────┘
                            │ Smart HTTP v1        │ JSON RPC (HTTPS)
                    ┌───────▼─────────────────────▼────────────────┐
                    │              banyand 服务进程                    │
                    │  ┌──────────────┐  ┌──────────────────────┐   │
                    │  │ gitwire 层    │  │ rpc 层                │   │
                    │  │ upload-pack  │  │ 逐对象批量获取          │   │
                    │  │ receive-pack │  │ log/blame/diff/file-h │   │
                    │  │ 虚拟子树仓库   │  │ （派生数据引擎支撑）     │   │
                    │  │ server filter│  └──────────┬───────────┘   │
                    │  └──────┬───────┘             │               │
                    │  ┌──────▼─────────────────────▼───────────┐   │
                    │  │        model 层（核心数据模型）            │   │
                    │  │  anchored commit / timeline / assembly  │   │
                    │  │  projection（timeline ↔ git 对象互转）     │   │
                    │  └──────┬────────────────────────────────-┘   │
                    │  ┌──────▼─────────────────────────────────-┐  │
                    │  │        storage 层（分布式 pack 存储）       │  │
                    │  │  ShardStore（本地/远端节点）  MetaStore    │  │
                    │  │  pack+idx 读写   对象→pack→node 全局索引    │  │
                    │  │  后台 repack（碎片合并 + delta 化）         │  │
                    │  └────────────────────────────────────────┘  │
                    └──────────────────────────────────────────────┘
                                        │
              ┌─────────────────────────┼─────────────────────────┐
              ▼                         ▼                         ▼
        ┌──────────┐             ┌──────────┐              ┌──────────┐
        │ 存储节点 A │   ......    │ 存储节点 N │              │ 中心元数据  │
        │ pack 分片  │  (rendezvous │ pack 分片  │              │ 线性化 WAL │
        │ + idx     │   hashing)   │ + idx     │              │ (DB)     │
        └──────────┘             └──────────┘              └──────────┘
```

部署形态：单机模式（所有角色单进程，用于开发/演示）与分布式模式（存储节点独立部署、元数据服务独立部署、banyand 无状态多副本 + LRU pack cache）。

## 基座选型：go-git

调研结论（截至 2026-10，go-git main 分支 COMPATIBILITY.md）：

| 能力 | go-git | banyan 用法 |
|---|---|---|
| packfile/idx decode/encode | ✅ | 存储层与 wire 层的编解码底座 |
| 对象模型（blob/tree/commit/tag，SHA-256） | ✅ | model 层的 git 投影直接复用 |
| 协议 v0/v1 客户端 + `transport/server` 服务端 | ✅ | wire 层参考/复用其协商代码 |
| protocol v2 | ❌（已立项 perf-2，docs/12 P1-5） | v1 服务全量存量用例；v2 补 ls-refs/fetch 命令面（按需 ref 广告 + 服务端驱动协商），push 恒 v0 |
| 服务端 filter（"Not offered when serving"） | ❌ | **自研**（子目录 clone 的关键） |
| 客户端 `--filter` 懒回填缺失对象 | ❌（可发 filter 请求、记录 promisor 状态，不能按需取回） | `banyan` CLI 自研逐对象批量 RPC 补齐 |
| multi-pack-index / cruft packs | ❌ | 分布式 idx 布局自行定义 |

结论：go-git 当**编解码与协议库**用，不当**框架**用。模型层、存储层、wire 层的核心逻辑自研，接口边界（`MetaStore`、`ShardStore`、`model.*`）全部自有定义，保证 go-git 可替换。

> **v6 main 观察记录（2026-10-16 源码分析）**：未发布的 v6 主干（`github.com/go-git/go-git/v6`，无 release tag）已开始实现 protocol v2——客户端 `Git-Protocol` 头协商、`packp.CommandRequest` 命令帧、fetch 路径均存在（`transport/upload_pack.go`）；但其自带 COMPATIBILITY.md 仍标 v2 ❌（未完成验收），且 v6 **移除了 `transport/server` 整个进程内服务端包**、storer 接口变形（新增 `RawObjectWriter`）、`NewParserWithStorage` 改为 `NewParser(opts)`。Banyan 决策：**锁定 v5.13.0（已发布、集成已验证）**；go-git 依赖隔离已审计（全项目仅 `internal/storage/packfile.go` 一处 import，换版本成本≈改一个文件）；v6 出正式 tag 且 v2 标 ✅ 后再评估迁移，届时收益是 `banyan` CLI 客户端可协商 v2。另：v5.13.0 与 v6 均无 blame 实现（仅有 `object.MergeBase`/`DiffTree`），`derived.Blame` 必须自研。

## 与 git 源码的对照

本仓库设计以 `F:\Workspace\git`（git 源码树）为行为基准，关键对照点：

| git 源码位置 | 行为 | banyan 对应 |
|---|---|---|
| `upload-pack.c` | `uploadpack.allowFilter` 广告 `filter` 能力；v1 会话中解析 `filter <spec>` 行 | gitwire/filter.go：能力广告 + `subtree:<path>` 扩展规格 |
| `receive-pack.c` | ref 命令行 → pack 流 → 解包 → ref 事务更新 → report-status | gitwire/receivepack.go：解包 → 锚定 timeline → head CAS → report-status |
| `http-backend.c` | `/info/refs?service=`、`/git-upload-pack`、`/git-receive-pack` 端点路由 | gitwire/http.go：同端点 + 虚拟仓库路由 |
| `Documentation/technical/partial-clone.adoc` | filtered packfile + promisor remote + 懒取 | rpc/objects.go：逐对象批量获取端点 |
| `Documentation/technical/pack-heuristics.adoc` | pack 组织与 delta 策略 | storage/repack.go：碎片合并与 delta 化策略 |

## 阅读顺序

先读 [01-data-model.md](01-data-model.md)（理解模型分歧点），再读 [02-storage.md](02-storage.md) 与 [03-git-compat.md](03-git-compat.md)（两个承诺：分布式与兼容性），最后 [04](04-derived-data.md)、[05](05-cli.md)、[06](06-roadmap.md)；定位与平台集成看 [08-big-picture.md](08-big-picture.md)。
