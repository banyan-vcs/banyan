# 00 · 总览：banyan 是什么

## 一句话

banyan 是一个 git 兼容的服务端版本控制系统：**客户端仍是标准 git，服务端换掉了数据模型与存储架构**——用"目录锚定时间线"替代"单一 root tree 历史"，用"分片 pack CAS + 中心元数据"替代"单机松散对象 + 本地 repack"。

## 为什么这么做：Scion 文章的启示映射

Anthropic 的 Scion 迁移（2025 年 6 月完成）验证了一组判断，banyan 逐条对应：

| Scion 的判断 | Scion 的做法 | banyan 的做法 |
|---|---|---|
| 客户端永远在线 → 服务端集中计算 | diff/blame/merge/commit 全部服务端，客户端只管理工作副本 | `banyan` CLI 的读操作全部走服务端 RPC；git 客户端照旧走 wire 协议 |
| 数据结构决定扩展性 | changeset 写前日志（文件级增量，无 tree 遍历） | 目录锚定 commit（tree/blob 与 commit 分离），子目录提交 O(子目录大小) |
| ref 是元数据瓶颈 | changeset 栈替代 99% ref | timeline head 即元数据表一行，CAS 更新 |
| 获取协议应该是"要什么"而非"有什么" | 客户点单式 RPC，逐树逐 blob | 服务端逐对象批量 RPC（banyan CLI 懒回填）+ git 侧 partial clone filter |
| 不可变内容可无限缓存 | blob 内容寻址 + snapshot header 缓存 | pack 即不可变分片，daemon/cache/server 三层同构缓存 |
| 服务端 ≠ 客户端架构 | 三层服务架构（daemon/cache/中心服务） | 存储节点无状态分片 + 中心元数据单写者 |

关键定位来自文章结论：

> "A git server doesn't have to work the same way as a git client does; novel server architectures exist that provide significant scaling improvements."
> （git 服务端不必和 git 客户端一样工作；新颖的服务端架构能带来显著的扩展性改进。）

banyan 把这句话作为第一性约束：**对客户端的承诺只有 git wire 协议语义；服务端内部自由重构。**

## 总体架构

```
                    ┌──────────────────────────────────────────────┐
                    │                客户端生态                       │
                    │  标准 git CLI      banyan CLI（对标 sl）         │
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
| protocol v2 | ❌ | 不做；标准 git 客户端对 v1 服务端自动回退 |
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

先读 [01-data-model.md](01-data-model.md)（理解模型分歧点），再读 [02-storage.md](02-storage.md) 与 [03-git-compat.md](03-git-compat.md)（两个承诺：分布式与兼容性），最后 [04](04-derived-data.md)、[05](05-cli.md)、[06](06-roadmap.md)。
