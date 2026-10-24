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
| B8 | **无 thin pack / delta-against-haves**：增量 fetch 对象集已最优（have 减除），但每个对象全量发送——git 服务器对照客户端已有对象出 REF_DELTA（客户端 --fix-thin 修补），源码 delta 比 2–5× | uploadpack 在线 EncodePack 零 delta（同 B5 的根因，但 thin pack 是独立缺口） | git 协议增量 fetch 字节效率 |
| B9 | **子树 clone 恒在线编码**：投影/织入对象 OID ≠ 任何存储包，ExactPackHit 永不命中 | 投影链合成提交（docs/01 §12） | 子树虚拟仓库 clone；跨请求无缓存复用 |
| B10 | **服务端 filter 未广告**：partial clone 客户端（--filter=blob:none）退化为全量拉取 | filter.go 就绪未广告（docs/03 §8） | 大仓 clone 带宽 |

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

**P1-4 增量 fetch 字节效率（治 B8+B10，场景二 review 产出）**
- 服务路径 delta 化（原 P2-1 升档）：在线服务按收益阈值选
  EncodePackDelta——吃掉 thin pack 收益的大头；
- 广告 `filter` 能力：filter.go 已就绪，广告后 partial clone 客户端
  （--filter=blob:none）不再退化为全量拉取——大仓 clone 带宽直降；
- 真 thin pack（对客户端 haves 出 REF_DELTA）依赖编码器能力，列远期。

**P1-5 protocol v2（fetch 侧命令面）**
- git 协议 v2 只定义 ls-refs 与 fetch（receive-push 按 git 语义恒 v0）；
- 对 banyan 的效率价值：**按需 ref 广告**（ref-prefixes——多 ref/辅助
  命名空间仓的每次 fetch 不再全量广告）与服务端驱动协商（减少复杂
  fetch 的协商轮次）；与 B10 的 filter 广告天然同批；
- 现状 v1-only 是实测兼容的（v2 客户端自动回退，T13），v2 是效率与
  生态位项而非兼容项。

### P2（perf-3 及以后）

| 项 | 治 | 方案 |
|---|---|---|
| P2-1 网关磁盘 pack cache | B6 | 内容寻址磁盘 LRU（docs/02 三层缓存的第一层落地），重启不冷 |
| P2-2 子树投影包缓存 | B9 | 投影闭包按 (repo, sub, tips 代次) 缓存服务端合成包，跨请求复用；tips 变化才重算 |
| P2-3 push 后预热织入 | B6 | receive-pack 成功后异步触发 RootTip（写路径派生急算，docs/04 既有分类），首个广告不再冷 |
| P2-4 写路径扇出下沉 | docs/11 HA-2 已列 | 主节点写+节点间扇出，砍网关出带宽 R×；W=quorum 同步 + 其余异步由 reconciler 补 |
| P2-5 同目录写队列 | B7 | M4：tekeburak 形态的 change request 状态机 + 投机流水线（FIFO 公平 + p99 稳定 + 重启恰好一次） |
| P2-6 真 thin pack | B8 | 对客户端 haves 出 REF_DELTA（依赖编码器对缺席基对象的支持），P1-4 之后视残余收益评估 |

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
perf-1（P0）：定位批量+缓存、织入批量化              → 集群读/写主路径
perf-2（P1）：idx 单对象解压、fetch-trees 二进制、SLO 基准门、
              增量 fetch 字节效率（服务 delta + filter 广告）、protocol v2
perf-3（P2）：磁盘 cache、子树投影包缓存、预热、扇出下沉（部分与 HA-2 合并）、
              真 thin pack
M4（既定）  ：同目录写队列（P2-5，参照 land worker）
```

依赖关系：P0-1/P0-2 无前置，可立即开工；P1-1 依赖 idx 写盘质量（crc 段
目前为 0，需补齐或跳过）；P1-3 建议在 P0 落地后建基线（否则基线立刻作废）；
P1-5（v2）与 P1-4 的 filter 广告同批实施最省（协商面一次动完）。

## 5. 架构判据记录：pack 磁盘存储 vs per-object 对象存储（2026-10-24 review 定稿）

场景化 review（MR diff / clone 与增量效率 / 磁盘 IO 对比）的结论：

**对 banyan 的定位（git 兼容协议边界 + 自托管本地盘 + monorepo 密度），
pack 是正确的架构；per-object 为"服务端计算 + 客户端点取 + 三层缓存"的
访问模式而生，且依赖 S3 语义才能成立。** 三条决定性判据：

1. **协议边界即 pack**：git clone/fetch 的 wire 格式就是 packfile——存储
   即协议语言，ExactPackHit 的 verbatim 直传是理论上限；per-object 架构
   在此边界永远付翻译成本（OpenCSGs 方向 = 每次 clone 现场生成 pack）。
2. **tekeburak 树教训是镜像证据**：逐指针跳读的遍历里批量访问赢过逐对象
   KV——它把树挪回元数据库，等于承认 per-object 不适合遍历；banyan 树在
   pack + TreeMemo，遍历更接近批量一侧。
3. **per-object 的优势项不在 banyan 支配负载上**：点取被服务端计算消化
   （fetch-trees 一次往返）；细粒度 GC 对应远期合规；零维护（免 repack）
   是真优势，但 monorepo 尺度下密度（delta）赢。

**pack 路线要认的代价**（多数已列优化项）：点读不透明（B2→P1-1）、
repack 写放大与维护（限速已设计）、对象级保留缺失（远期，重写式 GC）、
跨 pack 重复存储至 repack 收敛（B3 批量化 + repack 双重收敛）。

**正确演进是混合而非换架构**：pack 文件放对象存储（HA-3 的 S3 后端）——
保留密度与顺序性，获得 S3 耐久与弹性；idx/定位已在元数据侧。
