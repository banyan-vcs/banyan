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

> 治理状态（2026-10-25，perf-1/perf-2/perf-3 主体全部落地；基线数字见 §3）：
> P0-1/P0-2/P1-1/P1-2/P1-3/P1-5/P2-1/P2-2/P2-3/P2-6 ✅；P1-4 ✅（含真
> thin pack）；B7（同目录队列）归 M4、P2-4（扇出下沉）归 HA-2。

### P0（perf-1：集群读写主路径，预期一个数量级）✅ 已落地

**P0-1 定位批量 + 定位缓存（治 B1）✅**
- 网关 oid→packOID LRU（不可变内容寻址：位置只在 repack 时变，
  失效由 repack 事件驱动 + 跨网关陈旧读一次失效重试兜底）；
- closure/fetch-trees 的树展开点批量 Prefetch（一次 RPC 定位整棵树的
  子对象），协商 have 检查改走批量 Known（不取对象字节）；
- 实测收益：集群冷走查 refs RPC 从 O(对象数) 降为 O(树数)。

**P0-2 织入物化批量化（治 B3）✅**
- 一次 weave 物化的全部 synthetic 缓冲后合为一个 pack 一次 IngestPack
  （BeginSynthBatch/EndSynthBatch 按仓引用计数嵌套）；**pre-CAS flush**
  保证"登记即事实"（头提交前对象必须已注册——集群下其他网关可见）；
- 兼得 repack 收益（碎片不再按织入次数产生）。T31：一次子树物化 = 1 个
  合成 pack。

### P1（perf-2）

**P1-1 idx 单对象解压（治 B2）✅**
- idx 偏移 + 对象头解析 + OFS/REF delta 基链递归解压（packobj.go，对照
  pack-format.txt）；集群 wire 从整包降为单对象（/internal/object）；
- 落地时击落既有 bug：idx oid→offset 此前按 Scanner/Parser 顺序按位
  配对，delta pack 上错位——改用 Parser Observer 权威源，并补齐 crc 段
  真值。

**P1-2 fetch-trees 二进制化（治 B4）✅**
- "format":"bin" → 二进制帧（magic + JSON 头行 + kind/oid/varint 记录），
  base64 的 33% 直接消失；JSON 保留兼容。T33：两帧语义全等 + 实测收缩。

**P1-3 性能 SLO 基准门（工程纪律，参照教训 3）✅**
- `cmd/banyan-bench`（§3）：种子仓 + git http-backend 对等基线 + 三口径
  + `--check` 20% 红线；夜间跑，不进单元 CI 热路径。

**P1-4 增量 fetch 字节效率（治 B8+B10）——部分落地 ✅**
- serve 路径 delta 化 ✅：原始负载过 64KiB 阈值后在线 delta 编码
  （OFS_DELTA 优先/REF_DELTA 兜底；修复 go-git 编码器 window=0 语义
  陷阱——repack 此前实际未产出 delta）；T29/T20 钉住；
- `filter` 广告 ✅：blob:none / tree:0 + allow-reachable-sha1-in-want
  惰性取回全链路（T14）；v2 `fetch=filter` 同批（T30）；

**P1-5 protocol v2（fetch 侧命令面）✅**
- ls-refs（ref-prefix/peel/symrefs/unborn）+ fetch（acknowledgments/
  shallow-info/packfile 段、sideband pack 帧、done 省 acks 段——两处
  wire 细节由真实 git 客户端钉住）；receive-pack 恒 v0；T30。

### P2（perf-3：2026-10-25 主体落地，P2-4/P2-5 分别归 HA-2/M4）

**P2-1 网关磁盘 pack cache（治 B6）✅**
- `cache.pack_dir`（gateway 角色）：内容寻址磁盘 LRU（docs/02 三层缓存
  第一层），重启重扫重建索引；对象读走"廉价整包"路径（磁盘命中 → 一次
  解码全包 memoize），单对象读同一 pack 连续 8 次后自动翻转为整包取回
  ——冷网关克隆从 N 次单对象 RPC 降为 8+1；T36（缓存命中后节点请求冻结）。

**P2-2 闭包响应包缓存（治 B9）✅**
- clone 形态（无 haves/无浅化/无 filter）的响应包是 wants 的纯函数 →
  按 wants 键缓存 verbatim（128MiB LRU），子树/织入根视图的重复克隆
  免在线编码；tips 变化 → 新 wants → 新键，永不失效；带 haves 的请求
  （thin 领域）天然不进缓存；T35。

**P2-3 push 后预热织入（治 B6）✅**
- receive-pack 头 CAS 成功后异步 fold 视图（根推送暖根织入；子树推送
  暖作用域视图+根织入），首个广告不再冷；幂等 + CAS 安全，push 响应
  从不等待；T34（无需任何读请求 /.woven 即推进）。

**P2-6 真 thin pack（治 B8）✅**
- 自研 diff-delta 编码器 + thin pack 写出器（REF_DELTA 对客户端持有
  基对象）+ 同路径基选择（新提交 vs 客户端持有的父提交的路径感知树
  diff）；thin 优先于整包直传（存储包自包含，增量的大对象在包内是全量）；
  v1 广告 thin-pack / v2 接受 thin-pack arg；§3 实测增量 fetch
  1.66x → **0.87x**；T37。

| 项 | 治 | 状态 |
|---|---|---|
| P2-4 写路径扇出下沉 | docs/11 HA-2 | 归 HA-2（主节点写+节点间扇出，砍网关出带宽 R×） |
| P2-5 同目录写队列 | B7 | 归 M4（tekeburak 形态 change request 状态机 + 投机流水线） |

## 3. 基准设计与实测（P1-3 已落地：`cmd/banyan-bench`）

方法对齐三方对比报告的 tekeburak 教训（同机对等基线、三口径诚实标注、
20% 红线进夜间门）：

- **对等基线**：真 `git http-backend`（进程内 CGI 包装，同一 smart HTTP
  传输、同一仓库对象集、同机同栈）——不是"和本地 git 比"；
- **三口径**：冷（种子 push 后首个请求——重启后首访，织入/视图/定位全
  冷）/ 暖（同服务端重复操作）/ 增量（已持有历史的客户端取 1 个新提交
  ——协商与 have 减除效率；即 scion"服务端暖客户端冷"的等价场景）；
- **确定性**：固定种子生成仓库（small：30 提交 × 5 目录 × 5 文件；
  medium：120 × 10 × 8），同一种子产出相同对象集；
- **指标**：双向字节（服务端计数代理逐请求统计，头开销对称不计）+ 墙钟；
- **门**：`--save` 存基线（机器本地 JSON），`--check` 任一指标回归
  >20% 退出码 1（夜间跑，不进 go test 热路径）。

本机示例（Windows 开发机，small/medium 各 3 轮；perf-3 落地后数字，随
机器浮动，回归判断以 `--check` 门为准）：

| 场景 | banyan | git http-backend | 字节比 | 耗时比 |
|---|---|---|---|---|
| clone 全量 cold（small/medium） | 37.2KB / 260.0KB | 34.4KB / 242.5KB | 1.07–1.08x | 0.46–0.49x |
| clone 全量 warm | 37.2KB / 260.0KB | 34.4KB / 242.5KB | 1.07x | 0.47–0.48x |
| clone 子树 | 9.1KB / 34.0KB | —（git 无子树克隆） | — | — |
| fetch 增量（1 提交） | 0.8KB | 0.9KB | **0.87x** | 0.51x |
| push 1 提交 | 0.3KB | 0.3KB | 0.92x | 0.38–0.43x |
| ls-remote | 0.2KB | 0.3KB | 0.85x | 0.40–0.46x |

读数（诚实标注）：

1. **克隆字节持平**（ExactPackHit 整包直传）。耗时 2x 差主要来自 git 侧
   CGI 每请求冷启动（进程 spawn），常驻服务形态下会收敛——所以字节才是
   结构性指标；
2. **子树克隆是结构性优势**：下载量 = 子树闭包（全量的 13–24%），git
   基线无对应能力，这是 banyan 的存在理由之一；重复克隆走闭包响应缓存
   （P2-2），字节逐次一致；
3. **fetch 增量 0.87x——B8 缺口关闭并小幅反超**：真 thin pack（P2-6，
   对客户端 haves 出 REF_DELTA）+ ACK 祖先剪枝（镜像 git THEY_HAVE 语义，
   此前每个已知 have 白付 ~45B 响应）前为 1.66x；
4. cold==warm 字节（视图/定位/闭包缓存生效），cold 耗时 ≈ warm（此规模
   织入亚毫秒）；网关重启路径由磁盘 pack cache 兜底（P2-1，T36：缓存
   命中后节点请求数冻结）。

与集成测试的关系：集成用例（T1–T37）锁正确性；bench 门锁性能——两者
都不豁免。扩展位（后续）：集群口径的 RPC 计数（refs RPC/对象数比）与
scion 复刻对等基线（需 Rust 工具链；tekeburak 自己的对等基准数字也
注明了要打折读）。

## 4. 排期对位

```
perf-1（P0）✅：定位批量+缓存、织入批量化（2026-10-25 落地）
perf-2（P1）✅ 主体：idx 单对象解压、fetch-trees 二进制、SLO 基准门、
              serve 路径 delta + filter 广告、protocol v2
perf-3（P2）✅ 主体（2026-10-25 落地）：磁盘 pack cache（P2-1）、闭包
              响应缓存（P2-2）、push 后预热织入（P2-3）、真 thin pack
              （P2-6，§3 增量 fetch 1.66x→0.87x）
P2-4 扇出下沉：归 HA-2（主节点写+节点间扇出）；P2-5 同目录队列：归 M4
```

依赖关系（落地后复核）：P0-1/P0-2 已落地；P1-1 落地时一并修复 idx 配对
错位并补齐 crc（原注记的"依赖 idx 写盘质量"即此）；P1-3 基线已建
（本机数字见 §3，各部署用 --save 自建）；P1-5 与 P1-4 的 filter 广告
确已同批。P2-6 落地时被真实客户端钉住两处：REF_DELTA 头部 size 字段
= delta 流长（非目标对象长）；v2 ACK 须按 THEY_HAVE 祖先剪枝。

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
