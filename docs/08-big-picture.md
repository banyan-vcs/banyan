# 08 · Big Picture：通用代码存储层与企业平台集成

## 1. 定位：存储层，不是平台

banyan 是**通用代码存储层**（code storage layer）：对上提供轻量级代码托管能力
（git 协议接入、对象存储、视图派生），对下不假设任何平台形态。它**不是**
DevOps 平台——MR/issue/CI/权限体系/审计报表不在边界内。

| 能力 | 归属 | 说明 |
|---|---|---|
| 代码存储（pack/索引/repack） | **banyan** | 分片 pack CAS + 中心元数据 |
| git 协议接入（clone/push/pull） | **banyan** | smart HTTP v1，客户端零改动 |
| 视图派生（子树 clone/读时装配） | **banyan** | `/tree/<path>` 虚拟仓库，O(子目录) 传输 |
| 读侧服务（log/blame/diff RPC） | **banyan** | 服务端集中计算 |
| 协作流程（MR/review/issue） | 已有平台 | GitLab/Gitea/自建，**不做** |
| CI/CD 流水线 | 已有平台 | Runner 按协议取 banyan 或平台代码 |
| 组织/权限/合规 | 已有平台 | banyan 只做存储层凭证（见 §3.3） |

类比：Gitaly 之于 GitLab——默认走**协议原生**路线（对任何平台都是标准 git 远端，
git wire 协议是二十年稳定的公共契约）；另提供**可选的** Gitaly gRPC 兼容前端，
让 GitLab 直连 banyand 作为底层存储节点（决策更新 2026-10-22：原"不实现私有
API"改为分阶段兼容，私有契约差异隔离在适配层内，见 [10-service-api.md](10-service-api.md)）。

**差异化价值**（为什么平台自带存储之外还需要 banyan）：

1. **子树即仓库**：任意目录可独立 clone/push（目录时间线），monorepo 场景
   下开发体验与仓库大小解耦；
2. **零跨目录锁**：A/B 目录并行迭代互不阻塞（v5 写路径只 CAS 本目录时间线），
   仅同目录并发出现 git 式 fetch-first；
3. **根历史线性整洁**：读时装配（assembly-on-read）无收编 merge 垃圾；
4. **轻量**：单二进制 + SQLite 起步，按需生长到分布式分片；
5. **Agent 友好**：AI 编码工具要的是子树稀疏工作区 + O(子目录) 传输 +
   服务端派生（log/blame），恰是存储层能力，而非平台能力。

## 2. 架构大图

```
┌─────────────────────────── 生态接入 ───────────────────────────┐
│   git CLI    IDE/编辑器    CI Runner    AI Agent/编码工具    banyan CLI │
└─────┬────────────┬────────────┬────────────┬────────────┬──────┘
      │        git smart HTTP v1（含浅化/partial 回退）       │ JSON RPC
      │        任何 git 客户端零配置可用（docs/03 §9 实测矩阵） │
┌─────▼────────────▼────────────▼────────────▼────────────▼──────┐
│                        banyand 服务进程                          │
│                                                                 │
│  ┌──────────────┐ ┌────────────┐ ┌───────────────────────────┐ │
│  │ gitwire      │ │ rpc        │ │ view（读时装配，v5）        │ │
│  │ upload-pack  │ │ log        │ │  根视图 = 线性织入          │ │
│  │ receive-pack │ │ objects    │ │  子树视图 = 稳定前缀+追加   │ │
│  │ /tree/<path> │ │（逐对象懒取）│ │  /.woven 增量织入锚        │ │
│  └──────┬───────┘ └─────┬──────┘ └────────────┬──────────────┘ │
│  ┌──────▼───────────────▼────────────────────▼───────────────┐ │
│  │ model：timeline / anchored commit / projection（↔git 对象） │ │
│  └────────────────────────────┬──────────────────────────────┘ │
│  ┌────────────────────────────▼──────────────────────────────┐ │
│  │ storage：MetaStore（mem/SQLite：repos/timelines/packs/objects）│
│  │           ShardStore（pack+idx；本地实现，远端分片 M2 规划）  │ │
│  │           三层索引 / 整包直传 / repack（规划）               │ │
│  └───────────────────────────────────────────────────────────┘ │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐ │
│  │ 平台适配（规划，见 §3）：token 认证 / webhook 派发 / 镜像工具  │ │
│  └───────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

部署形态：单机（单进程 + SQLite，开发/小团队/边缘节点）→ 分布式
（banyand 无状态多副本 + 存储节点分片 + 中心元数据，docs/02）。

代码现实映射：`cmd/banyand` / `internal/{gitwire, view, model, storage, rpc,
config, derived}`——图中每一格都有对应包；**平台适配格当前为空**，是下节主题。

## 3. 企业平台兼容（GitLab 等）

### 3.1 关键认知：兼容分三层，逐层递进

| 层 | 内容 | 状态 |
|---|---|---|
| L1 协议兼容 | GitLab 的一切 git 操作（mirror/CI fetch/开发者 remote）把 banyan 当普通远端 | **已完成**（git 2.50 实测矩阵 T13–T16：v2 客户端降级、partial 回退、浅化全通） |
| L2 数据互通 | 镜像拓扑：真相源唯一，另一侧为投影 | **规划**（GitLab 侧配置即可起步，banyan 侧零开发；管理工具待 banyan CLI） |
| L3 流程集成 | 认证桥接 / 命名空间映射 / webhook 触发 CI | **规划**（banyan 侧需 token auth + webhook 派发，见 3.3） |

### 3.2 集成拓扑（L2）：写路径在哪，真相源在哪

```
拓扑 A：GitLab 为主（现状迁移最平滑）
  dev ──push──▶ GitLab ──push mirror──▶ banyan
  Agent/CI ◀──子树 clone── banyan          ← 读加速 + 子树视图价值点
  语义：GitLab→banyan 推的是根历史，verbatim 落地，无损

拓扑 B：banyan 为主存储（monorepo 重目录场景）
  dev ──push──▶ banyan（/tree/<dir> 直推）  ← 零跨目录锁迭代
  GitLab ◀──pull mirror── banyan            ← MR/review/CI 界面照常
  CI Runner ──fetch──▶ GitLab（或直连 banyan）
  语义：GitLab 拉的是根视图（线性织入产物），fsck 干净的合法 git 历史
```

两个方向都安全的原因（v5 语义）：

- **GitLab → banyan**（push mirror）：根仓库 push 走 verbatim 落地，OID 原样；
- **banyan → GitLab**（pull mirror）：根视图是纯线性、无 merge 脚手架的标准
  git 历史，对 GitLab 是普通仓库；
- **唯一注意点**：子树视图里的提交 SHA ≠ 根视图 SHA（织入重写，blob 恒等，
  docs/01 §13）。CI 若按 SHA 触发/校验，用根视图 OID（webhook 事件即根视图
  OID）；开发者在子树视图看到的 SHA 不用于跨系统定位。

拓扑 C（双向双写网关：push 同时 fan-out 两侧）一致性弱、冲突面大，
**不作为首选**，仅在强需求下评估。

### 3.3 流程集成（L3）：banyan 侧待建能力

| 能力 | 设计 | 现状 |
|---|---|---|
| 认证 | token 中间件：Personal Access Token / CI job token 透传（`Authorization` / git URL 凭证）；仓库级 R/W scope | **无认证**（当前 push 自动建仓是 demo 便利，M1 已注明收紧）——**企业集成前置条件 #1** |
| 命名空间映射 | GitLab `group/project` slug ↔ banyan repo id（MetaStore repos 表加映射列或约定式命名） | repos 表已有，映射待定 |
| webhook | receive-pack 成功后派发 push event（根视图 OID + ref）→ GitLab trigger API 触发流水线 | 无，gitwire 尾部挂钩即可 |
| 权限 | 仓库级 token 起步；目录时间线天然提供**路径级写隔离**原语（同目录 CAS 才互斥）——平台 ACL 映射到 banyan 凭证 scope | 设计点，未实现 |
| CI 取码 | Runner 直连 banyan（协议兼容，可用 `--depth` 减量）或镜像后走 GitLab | 协议侧就绪，无额外开发 |
| 存储后端直连 | Gitaly gRPC 兼容前端（P1 读+git 传输 → P2 网页写路径）：GitLab 把 banyand 当 Gitaly 存储节点，免镜像、单真相源 | 规划（[10-service-api.md](10-service-api.md) §4） |

### 3.4 明确的非目标

- praefect 复制/事务协议不承诺（直连单节点 Gitaly 模式先行，多副本走 docs/02 自身路线）；
  Gitaly 兼容为可选分阶段前端，私有契约随 GitLab 版本的漂移在适配层吸收（docs/10）；
- 不做平台 UI、MR 流水线、组织权限树；
- 不做双向强一致多主存储（真相源唯一原则）。

## 4. 演进视角（与 docs/06 对应）

```
M1 单机端到端 ──▶ M2 持久化/分布式 ──▶ M3 派生数据/传输优化 ──▶ M4 高并发工作流
      └──────────────▶ M5 平台集成（本篇）：token auth → 自动建仓收紧 →
                        webhook 派发 → 镜像管理（banyan CLI）→ 命名空间映射
```

M5 的每一步都**不改变数据模型与协议承诺**——平台适配是服务进程边缘的
薄层，这是"存储层定位"带来的架构红利。

## 5. 一句话收束

> banyan 管"代码住在哪里、怎么高效取用"；GitLab 管"人怎么围绕代码协作"。
> 两者以 git wire 协议 + 单真相源镜像解耦——协议是二十年公共契约，
> 这层解耦不依赖任何一方的私有 API。
