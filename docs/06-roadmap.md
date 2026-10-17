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
- [ ] `banyan clone` / `banyan fetch-objects`（CLI 侧）
- [ ] have 增量协商（当前忽略 have、回全量闭包，正确但非最优）

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

- [ ] MetaStore 持久化实现（SQLite/MySQL，WAL 语义）
- [ ] ShardStore 远端实现（HTTP 节点间协议）+ rendezvous 放置 + R 副本
- [ ] commit-graph + merge-base（generation number）
- [x] log RPC（/api/v1/log，路径过滤 + hash 一致视图）
- [ ] 路径历史 / diff RPC
- [x] 全局对象索引批量定位 + pack cache（内存层 M1 已落地；CommitGraph/TreeMemo 派生索引见 docs/02 §1.3）

## M3 · 派生数据与下载优化

- [ ] blame RPC（递归 + 记忆化）
- [ ] 可达性位图（upload-pack 闭包加速）
- [ ] 服务端 filter 完整规格（blob:none / tree:0 / sparse:oid / subtree:<path>）
- [x] 热路径整 pack 直传（已提前实现，docs/02 §1.3：闭包=整 pack 时字节直传，serve 2.77s→24ms）
- [ ] repack（碎片合并 + delta 化）
- [ ] 浅克隆支持

## M4 · Scion 级工作流

- [ ] 快照式本地状态（banyan snapshot / status）
- [ ] 栈式变更（banyan stack）
- [ ] 组装异步化：合并队列（受影响测试集，对照 Scion commit-vote）
- [ ] `banyan sync`（子目录显式 rebase 对齐根）

## 风险登记

| 风险 | 影响 | 缓解 |
|---|---|---|
| 投影 SHA 稳定性被算法演进破坏 | 客户端缓存/协商失效 | 投影算法版本化（trailer 带 algo-ver），新版本=新缓存空间 |
| 组装风暴（多目录高频 push） | 根时间线写热点 | M1 同步简单正确；M4 合并队列批量组装 |
| go-git 协议边界 bug | wire 兼容性 | 协议层薄封装，可换自研解析（pkt-line 解析工作量可控） |
| subtree split 历史膨胀 | 大仓库虚拟 clone 变慢 | split 缓存 + 深度截断 + 懒 split（按需展开历史窗口） |
