# 06 · 路线图

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
- [x] have 减除（2026-10-18 关闭 docs/03 §10 偏离 #1，见 M3/T19；此条为 M1 遗留记录）
- [x] **薄工作区 CLI 首批命令**（2026-10-23，超越原 M1 设定）：clone（最小化下载，无 .git/无历史闭包）/ status（离线）/ commit（服务端 changeset 组装，docs/05 §4.2）/ pull（服务端 tree-diff 增量）/ log / cat / fetch-objects / branch/tag / repo ls；T22/T22b 以真 git 客户端交叉验证

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

- [x] MetaStore 持久化（SQLite：接口四角色分权 docs/02 §2.1、双后端一致性测试、重启持久化 T17；MySQL/PG 为生产接替路径）
- [ ] ShardStore 远端实现（HTTP 节点间协议）+ rendezvous 放置 + R 副本（→ 由 HA-1 吸收，见 [11-ha.md](11-ha.md)）
- [ ] commit-graph + merge-base（generation number）
- [x] log RPC（/api/v1/log，路径过滤 + hash 一致视图）
- [x] 服务层抽取（internal/service，2026-10-23：rpc 全量薄化为适配层、fetch-trees/commit/refs/objects 扩展/resolve/repos 落地、log 下沉共享实现；gitwire 发布路径迁移至 CommitService 留待 Gitaly 前端，docs/10 §6）
- [ ] 路径历史 / diff RPC
- [x] 全局对象索引批量定位 + pack cache（内存层 M1 已落地；CommitGraph/TreeMemo 派生索引见 docs/02 §1.3）

## M3 · 派生数据与下载优化

- [ ] blame RPC（递归 + 记忆化）
- [ ] 可达性位图（upload-pack 闭包加速）
- [ ] 服务端 filter 完整规格（blob:none / tree:0 / sparse:oid / subtree:<path>）
- [x] 热路径整 pack 直传（已提前实现，docs/02 §1.3：闭包=整 pack 时字节直传，serve 2.77s→24ms）
- [x] repack（碎片合并 + delta 化，2026-10-18：合并 pack 注册先行→旧分片元数据/文件/缓存清理；sqlite 对象行 UPSERT 重指向；rpc /api/v1/repack + Repacker 阈值触发；T21/T21b 钉住——对象保全、字节收缩、重启存活）
- [x] 浅克隆支持（2026-10-18 提前实现，docs/03 §3a：deepen N / 加深 / unshallow / 客户端边界裁剪，对照 http-backend 参考流，T15/T16 钉住）

## M4 · 高并发工作流

- [ ] 快照式本地状态（banyan snapshot / status）
- [ ] 栈式变更（banyan stack）
- [ ] 组装异步化：合并队列（受影响测试集）
- [ ] `banyan sync`（子目录显式 rebase 对齐根）

## 风险登记

| 风险 | 影响 | 缓解 |
|---|---|---|
| 投影 SHA 稳定性被算法演进破坏 | 客户端缓存/协商失效 | 投影算法版本化（trailer 带 algo-ver），新版本=新缓存空间 |
| 组装风暴（多目录高频 push） | 根时间线写热点 | M1 同步简单正确；M4 合并队列批量组装 |
| go-git 协议边界 bug | wire 兼容性 | 协议层薄封装，可换自研解析（pkt-line 解析工作量可控） |
| subtree split 历史膨胀 | 大仓库虚拟 clone 变慢 | split 缓存 + 深度截断 + 懒 split（按需展开历史窗口） |

## HA · 高可用与集群部署（[11-ha.md](11-ha.md)；HA-1 核心已实施 2026-10-24）

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

v1 实施偏差（docs/11 §0 记录）：refs 后端仅 SQLite（重启 RPO=0 已钉住，T26）；
CommitPush 单事务未做（保持 PutPack+CASHead 两步，崩溃窗口=无害孤儿 pack）。

## perf · 性能优化（规划 2026-10-24，[12-performance.md](12-performance.md)，未实施）

- [ ] perf-1（P0）：定位批量+定位缓存（治逐对象 LocateObjects RPC）+ 织入
      物化批量化（治单对象 pack 风暴）——集群读/写主路径
- [ ] perf-2（P1）：idx 单对象解压、fetch-trees 二进制化（去 base64 33%）、
      性能 SLO 基准门（>20% 回归变红）、**增量 fetch 字节效率**（服务路径
      delta 化 + filter 能力广告，治 B8/B10）、**protocol v2**（ls-refs/fetch
      命令面：按需 ref 广告 + 服务端驱动协商；push 按语义恒 v0）
- [ ] perf-3（P2）：网关磁盘 pack cache、子树投影包缓存（治子树 clone 恒
      在线编码）、push 后预热织入、写路径扇出下沉（与 HA-2 合并）、
      真 thin pack（对 haves 出 REF_DELTA）
- 同目录写队列（P2-5）并入 M4（参照 tekeburak land worker 投机流水线）

## M5 · 平台集成（企业代码托管兼容）

详见 [08-big-picture.md](08-big-picture.md)：协议层已就绪（L1），镜像拓扑靠
GitLab 侧配置即可起步（L2）；banyan 侧待建（L3）——

- [x] tags 与任意 refs/<ns> 命名空间（2026-10-18：辅助 ref 全名存储/原样广告/tag 对象入包闭包剥离；T20 钉住——annotated/lightweight/删除/自定义命名空间/浅克隆 tag）
- [ ] token 认证（PAT / CI job token 透传，仓库级 R/W scope）+ 自动建仓收紧开关
- [ ] Gitaly gRPC 兼容前端 P1（Server/Repository/SmartHTTP/Ref/Commit/Blob/Diff 子集；GitLab 直连 banyand 为存储节点，docs/10 §4）
- [ ] webhook 派发（push event → 平台 trigger）
- [ ] 镜像管理工具（banyan CLI：拓扑 A/B 的建立/巡检/对账）
- [ ] 命名空间映射（平台 group/project ↔ banyan repo）
