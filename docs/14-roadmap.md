# 14 · 路线图

## M0 · 设计 + 骨架（本次交付）

- [x] 设计文档 6 篇（数据模型 / 存储 / 兼容层 / 派生数据 / CLI / 路线图）
- [x] Go 代码骨架：模型层类型与算法签名、存储层接口（shard 完整实现）、gitwire/rpc/derived 接口占位
- [x] `go build` / `go vet` 通过

## M1 · 单机端到端（核心链路已达成，2026-10-17）

目标：`git clone` / `git push` 对根仓库与虚拟子树仓库全链路可用。

- [x] smart HTTP v1 端点（info/refs / upload-pack / receive-pack）
- [x] 单机存储（ShardStore 本地盘 + MetaStore 内存实现；**进程内态，重启即失**——持久化在 M2）
- [x] AnchoredCommit 编解码（含 banyan-anchor header）
- [x] receive-pack：pack 入库 → 锚定 → head CAS（新建/更新/删除 ref 全支持；未广告能力不谎报）
- [x] upload-pack：投影 → 闭包 → NAK + 在线合成 pack（流式裸 pack 应答）
- [x] subtree split 投影（**DAG 版**：全父遍历、合并提交保留双 parent 边；经真实仓库验证）
- [x] push 自动建仓（demo 便利；M2 收紧为 ACL）
- [x] 目录锚点 push 的向上组装（AssembleRoot 已接线：子树 push → synthetic commit 入根时间线，三层可见性实测 522/1019/4044）
- [x] `banyan clone` / `banyan fetch-objects`（CLI 侧；服务端 /api/v1/objects RPC 已就绪）
- [x] have 减除（2026-10-18 关闭 docs/04 §10 偏离 #1，见 M3/T19；此条为 M1 遗留记录）
- [x] **薄工作区 CLI 首批命令**（2026-10-23，超越原 M1 设定）：clone（最小化下载，无 .git/无历史闭包）/ status（离线）/ commit（服务端 changeset 组装，docs/06 §4.2）/ pull（服务端 tree-diff 增量）/ log / cat / fetch-objects / branch/tag / repo ls；T22/T22b 以真 git 客户端交叉验证

**已验证的端到端链路**（样本：go-git 仓库，4043 提交 / 28649 对象 / 11MiB pack）：

```
git push → 根仓库 clone（4043 提交，HEAD 逐字节一致，755 文件）
        → 子目录 clone /tree/plumbing/format/packfile
          （521 提交 = git --full-history 语义，合并拓扑保留，
           git log / git show / git fsck 全部通过，工作树即子目录内容）
        → 增量 push 二次提交 → 子树仓库 pull 到新提交（历史连续）
```

已知语义差异（记录在案）：子树投影采用 `--full-history` 口径（含合并点全部变更边），比 `git log -- <path>` 的默认简化历史更全；路径被整段删除的提交（子树不存在）暂不投影。

## M2 · 分布式与元数据落地

- [x] MetaStore 持久化（SQLite：接口四角色分权 docs/03 §2.1、双后端一致性测试、重启持久化 T17；MySQL/PG 为生产接替路径）
- [ ] ShardStore 远端实现（HTTP 节点间协议）+ rendezvous 放置 + R 副本（→ 由 HA-1 吸收，见 [10-ha.md](10-ha.md)）
- [ ] commit-graph + merge-base（generation number）
- [x] log RPC（/api/v1/log，路径过滤 + hash 一致视图）
- [x] 服务层抽取（internal/service，2026-10-23：rpc 全量薄化为适配层、fetch-trees/commit/refs/objects 扩展/resolve/repos 落地、log 下沉共享实现；gitwire 发布路径迁移至 CommitService 留待 Gitaly 前端，docs/05 §6）
- [ ] 路径历史 / diff RPC
- [x] 全局对象索引批量定位 + pack cache（内存层 M1 已落地；CommitGraph/TreeMemo 派生索引见 docs/03 §1.3）

## M3 · 派生数据与下载优化

- [ ] blame RPC（递归 + 记忆化）
- [ ] 可达性位图（upload-pack 闭包加速）
- [ ] 服务端 filter 完整规格（blob:none / tree:0 / sparse:oid / subtree:<path>）
- [x] 热路径整 pack 直传（已提前实现，docs/03 §1.3：闭包=整 pack 时字节直传，serve 2.77s→24ms）
- [x] repack（碎片合并 + delta 化，2026-10-18：合并 pack 注册先行→旧分片元数据/文件/缓存清理；sqlite 对象行 UPSERT 重指向；rpc /api/v1/repack + Repacker 阈值触发；T21/T21b 钉住——对象保全、字节收缩、重启存活）
- [x] 浅克隆支持（2026-10-18 提前实现，docs/04 §3a：deepen N / 加深 / unshallow / 客户端边界裁剪，对照 http-backend 参考流，T15/T16 钉住）

## M4 · 高并发工作流

设计 v2 定稿见 [15-workflow.md](15-workflow.md)（2026-10-25：网关本地
批队列 + 一次 CAS 批追加 + 收敛 merge train + OCC 环；refs 零改动）。

- [ ] M4.1 重放原语抽取（diff/applyPaths/提交合成）+ treeKnown 索引（纯重构）
- [ ] M4.2 网关批队列 + 一次 CAS 批追加（只串行化，收敛默认关）
- [ ] M4.3 收敛 merge train：路径级合并 + 冲突隔离 + OCC 环（默认开，repo 级可关）
- [ ] M4.4 指标 + agent-burst bench 场景 + SLO 门
- [ ] 快照式本地状态（banyan snapshot / status）
- [ ] 栈式变更（banyan stack）
- [ ] `banyan sync`（子目录显式 rebase 对齐根）

## 风险登记

| 风险 | 影响 | 缓解 |
|---|---|---|
| 投影 SHA 稳定性被算法演进破坏 | 客户端缓存/协商失效 | 投影算法版本化（trailer 带 algo-ver），新版本=新缓存空间 |
| 组装风暴（多目录高频 push） | 根时间线写热点 | M1 同步简单正确；M4 合并队列批量组装 |
| go-git 协议边界 bug | wire 兼容性 | 协议层薄封装，可换自研解析（pkt-line 解析工作量可控） |
| subtree split 历史膨胀 | 大仓库虚拟 clone 变慢 | split 缓存 + 深度截断 + 懒 split（按需展开历史窗口） |

## HA · 高可用与集群部署（[10-ha.md](10-ha.md)；HA-1 核心已实施 2026-10-24）

- [x] **HA-1 核心**（T23-T26 全绿）：三角色拆分（`banyand -role all-in-one|gateway|refs|storage`，
      同一二进制）+ pack 散布随机多副本写（W=quorum 确认）+ refs 服务
      （RemoteMeta/RefsClient 内部 RPC、CommitPush 两步语义保留——孤儿无害）+
      节点健康状态机（healthy/suspect/dead/maintenance）+ reconciler
      （欠副本补齐/超副本剪枝/校验和验证）+ 仓库级动态 placement API +
      `GET /api/v1/cluster/nodes` + Docker 集群 demo（compose + 走查脚本）
- [ ] HA-2：refs 后端 MySQL/PG 与内嵌 raft、节点间直连扇出写（当前复制经 refs
      进程）、节点回归的影子转正与校验扫描（当前简化为心跳重注册+超副本剪枝）、
      备份与恢复工具
- [ ] HA-3：机架/可用区感知放置、跨集群异步复制（容灾）、S3 类对象存储后端

v1 实施偏差（docs/10 §0 记录）：refs 后端仅 SQLite（重启 RPO=0 已钉住，T26）；
CommitPush 单事务未做（保持 PutPack+CASHead 两步，崩溃窗口=无害孤儿 pack）。

## perf · 性能优化（规划 2026-10-24，[11-performance.md](11-performance.md)；perf-1/perf-2 主体已落地 2026-10-25）

- [x] perf-1（P0）：定位批量+LRU 缓存（含 repack 失效与跨网关陈旧重试）+
      织入物化批量化（pre-CAS flush 保登记即事实）——集群读/写主路径（T31）
- [x] perf-2（P1）主体：idx 单对象解压（/internal/object，修 idx 配对错位
      + 补 crc）、fetch-trees 二进制帧（T33）、性能 SLO 基准门
      （`cmd/banyan-bench`：git http-backend 对等基线 + 三口径 + 20% 红线，
      实测数字 docs/11 §3）、**增量 fetch 字节效率**（serve 路径 delta 化 +
      filter/allow-reachable 广告，T14/T29）、**protocol v2**（ls-refs/fetch
      命令面，T30；push 按语义恒 v0）
- [x] perf-3（P2）主体（2026-10-25）：网关磁盘 pack cache（cache.pack_dir +
      读放大翻转，T36）、闭包响应包缓存（T35）、push 后预热织入（T34）、
      **真 thin pack**（自研 diff-delta + thin 写出器 + 同路径基选择，T37；
      bench 增量 fetch 1.66x → **0.87x** 反超 git，docs/11 §3）
- [ ] 残余：写路径扇出下沉（P2-4，归 HA-2）、同目录写队列（P2-5，归 M4）
- 同目录写队列（P2-5）并入 M4（参照 tekeburak land worker 投机流水线）

## M5 · 平台集成（企业代码托管兼容）

详见 [01-big-picture.md](01-big-picture.md)：协议层已就绪（L1），镜像拓扑靠
GitLab 侧配置即可起步（L2）；banyan 侧待建（L3）——

- [x] tags 与任意 refs/<ns> 命名空间（2026-10-18：辅助 ref 全名存储/原样广告/tag 对象入包闭包剥离；T20 钉住——annotated/lightweight/删除/自定义命名空间/浅克隆 tag）
- [x] 授权抽象 + demo 策略 + 个人分支可见性（2026-10-25，docs/07：仓库/目录/分支 × 下载/提交
      三粒度门禁全部上 wire（v1+v2）；refs/heads/<user>/* 个人分支仅本人（+admin）可见——
      monorepo 分支过多解法；gateway 经 refs 策略快照走同一抽象；TS Web 管理面 + 文件浏览；
      T38/T38b。认证为 demo 级 Basic，token 认证仍待建）
- [x] git-over-SSH 传输（2026-10-25，docs/04 §7b：`ssh://` 直连 + 公钥认证（IdentifyKey
      身份缝，docs/07 §7）+ v0 有状态协商/v2 命令会话/receive-pack；同一授权矩阵；T39）
- [x] 存储寻址双模式 + rename（2026-10-25，docs/03 §6：named 可读目录 vs hashed
      @hashed 不透明键——映射在网关解析、rename 零存储动作；不变 storage_id；
      T40。reconciler 自动补齐 named 改名残留归 HA-2）
- [ ] token 认证（PAT / CI job token 透传，仓库级 R/W scope）+ 自动建仓收紧开关（已收紧：授权
      开启时建仓需 push 权）+ RPC 层细粒度授权（rename 端点现为认证门）
- [ ] Gitaly gRPC 兼容前端 P1（Server/Repository/SmartHTTP/Ref/Commit/Blob/Diff 子集；GitLab 直连 banyand 为存储节点，docs/05 §4）
- [ ] webhook 派发（push event → 平台 trigger）
- [ ] 镜像管理工具（banyan CLI：拓扑 A/B 的建立/巡检/对账）
- [ ] 命名空间映射（平台 group/project ↔ banyan repo）
