# 12 · 性能优化改进方案

> 依据：三方对比分析（两个 Scion 复刻 vs banyan，2026-10-24）中与性能直接
> 相关的三条硬教训 + banyan 代码现状逐条审计。方案只规划不实施；每项标注
> 代码依据与预期收益，排期对位见 §4。

## 0. 现状盘点

**已落地的性能资产**：整包直传（ExactPackHit，serve 2.77s→24ms）、have 减除
（增量包 11→4 对象）、代数索引剪枝（descends O(历史)→O(距离)）、树条目
memo（TreeMemo）、薄 CLI 的 tree-diff 增量（下载量=两检出差异）、repack
（delta 化 + 碎片合并）。

**已知瓶颈（代码级核实）**：

| # | 瓶颈 | 代码依据 | 触发场景 |
|---|---|---|---|
| B1 | **逐对象定位 RPC**：每次 Raw() 先发一次单 oid 的 LocateObjects | shardstore.go:206（lookupPack 每 OID 一查） | 集群模式：织入一次读数百对象=数百次 refs RPC |
| B2 | **缓存脱靶全包解码**：miss 即拉整包字节 + DecodePack 全量解析 | GetDecoded 无 idx 偏移路径（TODO(M2) 注释在案） | 大 pack（MB 级）冷读：为一个对象付整包传输+解压 |
| B3 | **织入的单对象 pack 风暴**：每个 synthetic 一次 EncodePack+IngestPack（集群= R 次 PUT + 1 次 meta RPC） | shardstore.go:77-102（PutSynthetic 逐对象） | 一次根视图物化产生 N 个织入对象 → N×(R+1) 次网络操作 |
| B4 | **fetch-trees base64 膨胀**：树/blob 字节走 JSON []byte = +33% 传输 | fetch.go:37（map[string][]byte） | 薄 CLI clone/pull 全程 |
| B5 | **应答零 delta**：serve 路径 EncodePack 固定 delta=false | uploadpack.go:160 | 非 ExactPackHit 命中的大 fetch |
| B6 | **网关冷启动**：pack/解码/视图缓存全内存，重启即冷 | ObjectService.cache/packOrder 内存态 | 重启后首个 clone 全量重算+重传 |
| B7 | **同目录写竞争**：last-writer-wins，无队列/退避 | docs/01 §13 开放项 | Agent 同目录高频提交 |

## 1. 参照系的三条硬教训（三方对比报告直接命中）

1. **land 流水线的实测数字**（tekeburak）：每仓库串行 worker + CAS 在飞时
   投机预 rebase → 16 并发写者总耗时 0.1× git（295ms vs 3022ms）且
   p99=285ms 极稳。banyan 的跨目录零锁已消解大半争用，**B7（同目录）是
   这个数字指向的最后一域**——M4 合并队列的直接参照。
2. **"树放哪"的基准反转**（tekeburak 决策记录）：逐指针跳读的树遍历里，
   批量访问的元数据库赢过逐对象 KV——**读放大发生在遍历模式里**。banyan
   的树在 pack + TreeMemo 常驻内存方向正确；教训落在 B1（遍历中每步一次
   RPC 是同款读放大，只是发生在网络层）。
3. **性能 SLO 进 CI**（tekeburak Phase 10）：倍率红线（sparse clone
   ≤0.1×git、16 写者 ≤0.1×）+ 任何行回归 >20% 夜间基准变红。banyan 的
   性能主张目前是**一次性文档数字**——这是工程纪律差距，不是算法差距。

## 2. 优化项清单

### P0（perf-1：集群读写主路径，预期一个数量级）

**P0-1 定位批量 + 定位缓存（治 B1）**
- 网关加 oid→packOID LRU（不可变内容寻址：位置只在 repack 时变，
  失效由 repack 事件驱动——命中率高且语义安全）；
- closure/fetch-trees/织入的批量读改走批量 LocateObjects（RPC 契约已有
  数组入参，改调用方攒批即可）；
- 收益：集群织入/克隆的 refs RPC 从 O(对象数) 降到 O(pack 数)。

**P0-2 织入物化批量化（治 B3）**
- 一次 weave 物化的全部 synthetic（织入锚、重写提交、新树）缓冲后
  **合为一个 pack 一次 IngestPack**——集群网络操作从 N×(R+1) 降为 R+1；
- 兼得 repack 收益（碎片不再按织入次数产生）。
- 实施点：view 引擎在 fold 结束后统一 flush；PutSynthetic 保留为兜底。

### P1（perf-2）

**P1-1 idx 单对象解压（治 B2）**
- 用已写盘的 idx v2 拿偏移，仅解压目标对象及其 delta 基链
  （OFS/REF 递归），替代全包 DecodePack；
- 首先覆盖集群 miss 路径（免整包 HTTP 传输），单机路径次之（内存缓存
  命中率高，收益小）。

**P1-2 fetch-trees 二进制化（治 B4）**
- 响应改二进制帧（或 gzip 的 JSON）；base64 的 33% 直接消失。
  薄 CLI 是自己人，无兼容包袱。

**P1-3 性能 SLO 基准门（工程纪律，参照教训 3）**
- `internal/bench`：固定种子仓（小 / 中 / go-git 4k），口径分
  冷（重启后首访）/ 暖 / 增量；
- 场景：clone 字节与耗时、增量 fetch 对象数、push 耗时、织入物化耗时、
  集群 R=3 写路径 RPC 计数；
- 门：任一指标回归 >20% 变红（夜间跑，不进单元 CI 热路径）。

### P2（perf-3 及以后）

| 项 | 治 | 方案 |
|---|---|---|
| P2-1 应答 delta 化 | B5 | 非 ExactPackHit 路径按收益阈值选 EncodePackDelta（CPU 换字节） |
| P2-2 网关磁盘 pack cache | B6 | 内容寻址磁盘 LRU（docs/02 三层缓存的第一层落地），重启不冷 |
| P2-3 push 后预热织入 | B6 | receive-pack 成功后异步触发 RootTip（写路径派生急算，docs/04 既有分类），首个广告不再冷 |
| P2-4 写路径扇出下沉 | docs/11 HA-2 已列 | 主节点写+节点间扇出，砍网关出带宽 R×；W=quorum 同步 + 其余异步由 reconciler 补 |
| P2-5 同目录写队列 | B7 | M4：tekeburak 形态的 change request 状态机 + 投机流水线（FIFO 公平 + p99 稳定 + 重启恰好一次） |

## 3. 基准设计（P1-3 展开）

```
场景 × 口径矩阵（记录字节 / 耗时 / RPC 计数三类指标）：
  clone(root)         冷 / 暖 / 集群冷
  clone(subtree)      冷 / 暖
  fetch(增量 1 提交)   暖（have 减除生效）
  fetch-trees(薄CLI)  全量 / tree-diff 增量
  push                首推 / 增量 / 织入触发
  repack 前/后         serve 耗时与字节
红线：相对基线回归 >20%（基线随特性落地滚动更新并记录）
```

与现有测试的关系：集成用例（27 个）锁正确性；bench 门锁性能——
两者都不豁免（tekeburak 的"三口径诚实标注"方法照搬：冷/暖/服务端暖
客户端冷分开报，不挑好看的口径）。

## 4. 排期对位

```
perf-1（P0）：定位批量+缓存、织入批量化          → 集群读/写主路径
perf-2（P1）：idx 单对象解压、fetch-trees 二进制、SLO 基准门
perf-3（P2）：应答 delta、磁盘 cache、预热、扇出下沉（部分与 HA-2 合并）
M4（既定）  ：同目录写队列（P2-5，参照 land worker）
```

依赖关系：P0-1/P0-2 无前置，可立即开工；P1-1 依赖 idx 写盘质量（crc 段
目前为 0，需补齐或跳过）；P1-3 建议在 P0 落地后建基线（否则基线立刻作废）。
