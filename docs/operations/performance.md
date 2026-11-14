# 性能优化改进方案

> 依据：与同类目录锚定系统的对比分析（2026-10-24）中与性能直接
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
| B7 | **同目录写竞争**：last-writer-wins，无队列/退避 | docs/design/data-model.md §13 开放项 | Agent 同目录高频提交 |
| B8 | **无 thin pack / delta-against-haves**：增量 fetch 对象集已最优（have 减除），但每个对象全量发送——git 服务器对照客户端已有对象出 REF_DELTA（客户端 --fix-thin 修补），源码 delta 比 2–5× | uploadpack 在线 EncodePack 零 delta（同 B5 的根因，但 thin pack 是独立缺口） | git 协议增量 fetch 字节效率 |
| B9 | **子树 clone 恒在线编码**：投影/织入对象 OID ≠ 任何存储包，ExactPackHit 永不命中 | 投影链合成提交（docs/design/data-model.md §12） | 子树虚拟仓库 clone；跨请求无缓存复用 |
| B10 | **服务端 filter 未广告**：partial clone 客户端（--filter=blob:none）退化为全量拉取 | filter.go 就绪未广告（docs/gateway/git-compat.md §8） | 大仓 clone 带宽 |

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
  真值；
- **内容哈希终验（2026-11-05 追加）**：抽取结果对请求 oid 做
  model.ObjectOID 终验，失配即错（腐坏副本能 inflate 出"可解析但错误"
  的字节——内容寻址系统里每个读出字节都要过哈希；散布存储失配时回退
  下一 holder）。每个单对象抽取多一次哈希——安全换性能，量级可忽略。

**P1-2 fetch-trees 二进制化（治 B4）✅**
- "format":"bin" → 二进制帧（magic + JSON 头行 + kind/oid/varint 记录），
  base64 的 33% 直接消失；JSON 保留兼容。T33：两帧语义全等 + 实测收缩。

**P1-3 性能 SLO 基准门（工程纪律，参照教训 3）✅**
- `banyand bench`（§3）：种子仓 + git http-backend 对等基线 + 三口径
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
- `cache.pack_dir`（gateway 角色）：内容寻址磁盘 LRU（docs/storage/storage.md 三层缓存
  第一层），重启重扫重建索引；对象读走"廉价整包"路径（磁盘命中 → 一次
  解码全包 memoize），单对象读同一 pack 连续 8 次后自动翻转为整包取回
  ——冷网关克隆从 N 次单对象 RPC 降为 8+1；T36（缓存命中后节点请求冻结）。翻转仅集群线（backend.PrefersWholePack：集群每次单对象读是 RPC round trip）；all-in-one 本地后端永不翻转——idx 抽取即文件读，翻转只会把整包驻留内存（T50 复盘，docs/web/console.md §7）。

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
| P2-4 写路径扇出下沉 | docs/storage/ha.md HA-2 | 归 HA-2（主节点写+节点间扇出，砍网关出带宽 R×） |
| P2-5 同目录写队列 | B7 | 归 M4（tekeburak 形态 change request 状态机 + 投机流水线） |

## 3. 基准设计与实测（P1-3 落地 + 2026-10-25 v2 扩展：`banyand bench`）

方法对齐三方对比报告的 tekeburak 教训 + hyperfine/scion bench-real 方法论
（同机对等基线、多口径诚实标注、统计采样、20% 红线进夜间门）：

- **对等基线**：真 `git http-backend`（进程内 CGI 包装，同一 smart HTTP
  传输、同一仓库对象集、同机同栈）——不是"和本地 git 比"；
- **温度口径**：冷（种子 push 后首个请求——重启后首访，织入/视图/定位
  全冷）/ 暖（同服务端重复操作）/ 增量（已持有历史的客户端取 1 个新
  提交——协商与 have 减除效率）；
- **工作流口径**（v2，scion bench-real 的 land 形态）：dev-loop（稳态
  协作循环，每轮 commit+push+fetch 一个样本）；burst-writers-8（8 并发
  写者各改不同文件同时 push 同分支——git 侧 non-FF 自拒后 pull --rebase
  重推（git 文档化恢复路线），banyan 收敛列车服务端合并；记录
  total/p50/p99）；
- **协议能力口径**（v2）：clone 浅化（--depth 1）、clone partial
  （--filter=blob:none）——双方都实现，同口径可比；
- **统计采样**（v2，hyperfine 方法）：每场景 runs=5，中位数 + Tukey
  fence（1.5×IQR）离群计数 + ±离散度；`--csv` 导出全样本
  （workload,tool,run,ms,bytes）；`--md` 导出 Markdown 报告；
- **确定性**：固定种子生成仓库（small 30c×5d×5f；medium 120×10×8；
  **large 500×20×12**——v2 规模阶梯），同一种子产出相同对象集；
- **指标**：双向字节（服务端计数代理逐请求统计——字节是结构性指标，
  不受进程 spawn 噪声影响）+ 墙钟；
- **门**：`--save` 存基线（机器本地 JSON），`--check` 任一指标回归
  >20% 退出码 1（夜间跑，不进 go test 热路径）。

本机实测（Windows 开发机，runs=5；随机器浮动，回归判断以 `--check`
门为准；**多轮验证与三个劣化指标的 pack 级归因见 §3a**）：

| 场景 | medium 字节比/耗时比 | large 字节比/耗时比 |
|---|---|---|
| clone 全量（cold≈warm） | 1.07x / 0.49x | 1.07x / 0.61x |
| clone 浅化 depth=1 | 1.15x / 0.58x | 1.14x / 0.69x |
| clone partial blob:none | **0.65x** / 0.48x | 0.91x / 0.83x |
| clone 子树 | 36.4KB（全量 14%，git 无） | 87.2KB（全量 **5.9%**，git 无） |
| fetch 增量（1 提交） | **0.87x** / 0.52x | **0.87x** / 0.55x |
| push 1 提交 | 0.92x / 0.39x | 0.92x / 0.40x |
| push 子树 | ~0.3KB 每推（git 无对应） | 同 |
| dev-loop 每轮 | 0.89x / 0.47x | 0.89x / 0.47x |
| **burst-8 total** | 0.98x / **0.55x** | 0.99x / **0.53x** |
| **burst-8 p50** | — / **0.55x** | — / **0.52x** |
| ls-remote | 0.85x / 0.54x | 0.85x / 0.48x |

读数（诚实标注）：

1. **克隆字节持平**（ExactPackHit 整包直传，跨三档规模 1.07x 稳定）。
   耗时 ~0.5x 主要来自 git 侧 CGI 每请求冷启动（进程 spawn），常驻服务
   形态下会收敛——所以字节才是结构性指标；
2. **子树克隆是结构性优势且随规模放大**：下载量 = 子树闭包，small
   29% → medium 14% → large 5.9%——大 monorepo 下目录级团队只为自己
   目录付费；git 基线无对应能力，这是 banyan 的存在理由之一；
3. **burst-8 是工作流主场**：同等流量（0.98x）下耗时减半（0.52–0.55x）
   且随规模扩大（git 侧每轮 rebase 拉全史）——优势来自协议往返次数
   （git 重试风暴 vs 收敛列车一轮落地），非压缩；
4. **fetch 增量 0.87x——B8 缺口关闭并小幅反超**：真 thin pack（P2-6，
   对客户端 haves 出 REF_DELTA）+ ACK 祖先剪枝（镜像 git THEY_HAVE 语义，
   此前每个已知 have 白付 ~45B 响应）前为 1.66x；
5. **partial clone 优势在大仓收窄**（0.65x→0.91x）：双方都退化为
   提交+树，banyan 的树遍历服务端消化优势被摊薄；
6. 诚实项：浅克隆字节 1.14x（浅边界协商编码开销）；cold==warm 字节
   （视图/定位/闭包缓存生效，织入亚毫秒）；网关重启路径由磁盘 pack
   cache 兜底（P2-1，T36：缓存命中后节点请求数冻结）。

与集成测试的关系：集成用例（T1–T44）锁正确性；bench 门锁性能——两者
都不豁免。扩展位（后续）：集群口径的 RPC 计数（refs RPC/对象数比）、
scion 复刻对等基线（需 Rust 工具链；tekeburak 自己的对等基准数字也
注明了要打折读）、阶段分解 trace（scion 的 SCION_TRACE 形态——服务端
各阶段 span 汇总找 critical path）。

### 3a. 多轮验证与劣化归因（原 16 号实测报告并入，2026-10-25）

> 原独立报告（16-benchmark-report.md）并入本节。工具与基线即 §3；
> 本节是实测快照与归因，原始样本由工具 `--csv` 再生，不入库。

medium（120c × 10d × 8f）完整三轮（每轮每场景 5 采样）+ large（500c ×
20d × 12f）一轮：

| 场景 | medium 三轮（字节比） | large（字节比） | 稳定性 |
|---|---|---|---|
| clone 全量 | 1.07x / 1.07x / 1.07x | 1.07x | **字节三轮位级稳定**（260.2–260.4KB，差异 <0.2%） |
| clone 浅化 depth=1 | 1.15x / 1.15x / 1.16x | 1.14x | 稳定 |
| clone partial | 0.65x ×3 | 0.91x | 稳定 |
| clone 子树 | 36.4KB（全量 14%） | 87.2KB（全量 5.9%） | 稳定（git 无对应能力） |
| fetch 增量 | 0.87x ×3 | 0.87x | 稳定 |
| push 1 提交 | 0.92x ×3 | 0.92x | 稳定 |
| dev-loop 每轮 | 0.89x ×3 | 0.89x | 稳定 |
| burst-8 total | 0.98–0.99x | 0.99x | 稳定 |
| ls-remote | 0.85x ×3 | 0.85x | 稳定 |

耗时类中位数轮间波动 ±10% 以内（burst total 0.55–0.61x、clone 0.49–
0.61x）——**字节是结构性指标、耗时受进程 spawn 与调度噪声主导**的实证；
离群计数（Tukey）普遍 0–2/5。结论数字（耗时取三轮区间中位）：

- **burst-8**（8 并发写者）：total 0.55–0.66x、p50 0.52–0.66x 且随规模
  扩大（git 侧每轮 rebase 拉全史）；
- 全量/增量/推送/循环等稳态场景耗时 0.39–0.61x（git CGI 每请求 spawn
  是主因，常驻 git 服务形态下会收敛）。

**劣化场景归因**（对象集全部验证相同；三个落后指标逐个 pack 级取证，
独立 probe 仓 + bench 保留工作目录，`verify-pack -v` 逐对象对比）：

**① clone 全量 1.07x —— 双方 delta 化时机不同，非实现缺陷。**
对象数与 delta 数完全相同（1087=1087），逐对象 delta 后大小差 ~3.5%。
banyan 走 ExactPackHit **verbatim 直传客户端 push 的 pack**（零拷贝）；
git http-backend 每次服务**现场重新 delta 化**（C pack-objects）。两次
delta 化（客户端打包时 vs 服务端服务时）的基选择不同 → ~8% 总差。
**repack 反而更差**：对同仓跑 banyan repack 后 serve 字节 229.9KB
（> verbatim 215.2KB > git 199.4KB）——banyan 的 Go delta 编码器
（go-git EncodePackDelta 移植）压缩质量低于 C diff-delta。
**结论：现状即最优解**——7% 字节换零拷贝直传；repack 的价值在碎片
合并运维，不在 serve 字节（部署不必为带宽而 repack）。注：pack 尺寸
本身没有 4GiB 限制——idx v2 的 64 位大偏移表即为此设计，banyan 的
偏移自 2026-10-25 起全链 uint64——此处差异纯在 delta 选择。

**② clone 浅化 1.14x —— 对象集相同，编码差异，方向随形态波动。**
bench 场景（多 pack 状态）下浅化克隆对象数相同（116=116）、pack 级
1.065x；但独立 probe（干净仓、blob 相似度高的形态）banyan 浅化**反超
至 0.85x**（banyan 在线 delta 化在小闭包上更优）。差异全部在压缩/delta
编码层（对象集计算已由 T15 钉死正确），波动区间约 ±15%。
**可选优化**：在线编码路径（浅化/子树/partial 的 closure pack）将
`compress/zlib` 换 klauspost/compress（纯 Go、zlib 兼容、压缩率高
5–10%）——优先级低：该路径多数场景已反超，仅浅化一项略落后。

**③ partial clone 大仓收窄至 0.91x** —— 双方都退化为提交+树，banyan
树遍历服务端消化的优势被摊薄，非劣化。**无优化动作**。

**结论矩阵**：

| 维度 | banyan vs git | 性质 |
|---|---|---|
| 结构能力（子树克隆/目录级推送） | 全量的 5.9–14%；git 无对应 | 模型级优势，随规模放大 |
| 并发收敛（burst-8） | 耗时 0.52–0.66x，字节持平 | 协议往返次数优势（收敛列车 vs rebase 重试） |
| 稳态传输（全量/增量/推送） | 字节 0.87–1.07x | 持平；7% 上限来自 verbatim vs 现场 delta 的取舍 |
| 离散能力（浅化/partial） | 0.65–1.16x | 编码层波动，对象集恒等 |

复现：`go run ./cmd/banyand bench --profile medium|large --runs 5
--md report.md --csv samples.csv`（多轮验证重复执行多次取区间）。

### 3b. 真实仓库样本（2026-11-14）：`--repo` 模式与它摇出的三个问题

合成种子是确定性且快的，但它们**单分支、纯文本、blob 尺寸均匀、零合并**
——真实历史（合并拓扑、数百 tag、二进制 blob、深度 delta 链）走的协商/
闭包/打包/织入路径种子根本碰不到。`banyand bench --repo <本地仓>` 把上级
目录的真实仓镜像进两个对端（git CGI 直接服镜像；banyan 经 git push 吃进
全部分支+tag），跑同一套场景矩阵：

	banyand bench --repo F:\Workspace\go-git --runs 3 --md r.md --csv s.csv
	banyand bench --repo F:\Workspace\git --scenarios clone-full,ls-remote \
	    --runs 3            # 大样本：--scenarios 过滤场景族

实现要点（internal/bench/real.go）：镜像仓的默认分支如实注入（git.git 是
`master`，本地 go-git 检出在 `merge-ort`——不猜 `main`）；子树场景骑文件
数最多的顶层目录（go-git=plumbing、git.git=t）；burst 写者分布到真实顶层
目录；增量基线**按对端各自克隆**（见下②——镜像基线会被织入视图判
non-fast-forward，那是正确语义）。

**摇出的四个问题**（真实样本的价值所在；前三个当日修复，第四个登记）：

1. **GetObject 每次单对象读都重新读盘+重解码整个 idx**（shardstore.go）：
   idx 是内容寻址且不可变的，却没有任何 memo——视图物化按目录逐个
   SplitHistory 全史重走时，4K 提交的仓把一个 780KB idx 重解码了数万次，
   冷克隆 **17 分钟未完成**（CPU 持续 40% 燃烧，goroutine 栈钉在
   DecodeIdx）。修复=LocalShardStore idx memo（128MiB 字节预算 LRU，
   最新条目永不逐出；回归测试 TestIdxMemoServesWithoutIdxFile：删掉 idx
   文件后读仍命中）。修复后同一冷克隆 **39 秒**完成。
2. **合并富历史在广告层被织入重线性化**（设计使然，非 bug）：根视图是
   线性无合并链（docs/design/data-model.md §13），合成种子的线性史走了
   identity 快路径所以 OID 原样；真实合并拓扑每个提交都被重写。tag 是
   逐字 aux ref 钉住**原始**历史，分支广告**织入**重写——克隆因此同时
   携带两套提交对象（go-git：原始 5,550 → 服务 9,598 个提交对象）。
   harness 修复=增量基线按对端克隆（镜像基线对 banyan 推送必吃
   non-fast-forward——receivepack 的 race guard 语义正确）。
3. **bench 的 git CGI 包装漏转发 HTTP_CONTENT_ENCODING**：git remote-curl
   对大 fetch 请求体发 gzip（`Content-Encoding: gzip`），包装没按 CGI 规范
   转成 `HTTP_*` 变量 → http-backend 把 gzip 字节当 pkt-line 解析，
   "bad line length character" 死给 502。种子请求体太小从不触发。修复=
   转发该头 + 502 响应体现场带 CGI stderr（本次排查靠独立 repro 才看到
   真实错误）。
4. **git.git 尺度的冷广告是 O(顶层目录 × 全史) 物化，当日未修**（登记
   为后续项）：ls-refs 的 ProjectionRefs 对每个顶层目录各跑一次
   SplitHistory 全史走（82K 提交 × ~30 目录），并把每个目录的投影提交
   全部落成合成对象——诊断 dump 里服务 goroutine 停在 SplitHistory、
   进程内存涨到 **9GB**、首个冷克隆 **20+ 分钟不返回**（goroutine 存活
   且 runnable——不是死锁，是组合量级）。go-git 尺度同一路径 39 秒完成，
   从未暴露。两个已登记的结构修复方向：广告期所有目录**共享一次全史走**
   （一次根树读服务全部目录的 SubtreeAt）；投影 ref 的 head-only 快速
   推导（广告只需要 head，不必持久化整条投影史——物化留给子树面真被
   访问时）。

**go-git 全矩阵实测**（4,039 提交 · 95 refs · 11.8MiB · branch merge-ort，
runs=3，2026-11-14；同机同栈同口径，对照 §3 合成数字读）：

| 场景 | 字节比 | 耗时比 | 合成种子（medium/large） |
|---|---|---|---|
| clone 全量（cold / warm） | **2.06x / 2.05x** | 1.39x / **2.11x** | 1.07x / 0.49–0.61x |
| clone partial blob:none | **0.45x** | 1.86x | 0.65–0.91x |
| clone 浅化 depth=1 | 1.05x | 1.42x | 1.14–1.15x |
| clone 子树 plumbing/ | 7.1MiB（全量 **35%**，git 无） | 19.9s | 全量 5.9–14% |
| fetch 增量（1 提交） | 0.92x | 1.36x | **0.87x** |
| push 1 提交 | 1.09x | 0.51x | 0.92x |
| dev-loop 每轮 | 1.04x | **0.56x** | 0.89x |
| burst-8 total | 1.06x | **1.46x** | **0.53–0.55x** |
| ls-remote | 1.08x | 0.66x | 0.85x |

诚实读数（真实形态改写了哪些合成结论）：

- **全量克隆字节 2x**：来源=①织入重线性化后克隆携带两套提交对象（②的
  结构代价）；②多 pack 状态下 ExactPackHit 不命中（织入合成 pack +
  推送 pack），走在线编码。耗时 2.11x（warm）来自每克隆重复付闭包重走
  （28–50s/次）+ 在线编码（22s/次）——**闭包计算无跨请求复用**是下一个
  优化面（种子仓太小从未暴露）；
- **burst-8 耗时反转为 1.46x**：8 个并发推送各自触发织入物化（每目录
  SplitHistory 全史走），冷织入税在争用下叠加；种子仓目录少、历史浅，
  曾是 0.55x 的主场。收敛列车的往返优势被物化成本吃掉——同目录队列
  （M4）+ 物化增量化（见下）之前的真实形态读数；
- **仍然成立的优势**：partial clone 0.45x（树遍历服务端消化）、
  dev-loop 0.56x、push 0.51x、子树克隆 35%（git 无对应）；fetch 增量
  0.92x 字节（thin pack 把未减除的树闭包 delta 掉——闭包对象集里
  140 树/767 blob 全被对端持有基压缩回收）；
- **已登记的后续项**：克隆闭包的跨请求缓存（闭包是 wants 的纯函数，
  P2-2 已缓存响应包但闭包重算）、广告期逐目录 SplitHistory 的共享一次
  全史走（当前 O(目录×历史)）、burst 的冷织入增量化。

**方法论硬化 v2（2026-11-14 下午，配对交错轮）**：真实样本跑数暴露的
另一类偏差——两个目标各跑完整块，主机漂移（热降频/后台负载/页缓存状
态）落在两块之间，直接污染耗时比。v2 起 harness 改为**每个场景族内按
轮配对**：banyan 与 git 的同轮样本背靠背执行，漂移作用于轮内两侧、比
值免疫；每轮前采一次**控制通道**（固定小仓本地克隆——进程生成+包写+
检出，纯主机热敏，不经任何被测目标），报告头部打印其中位与 Tukey 修
剪离散度，离散超过中位 1/4 时显式宣告"耗时比可信度降级（字节比不受影
响）"；Env 指纹记录 cpu/maxprocs/go/git 版本。合成验证当场抓到本机
±30% 漂移（control 421ms ±126ms）——这正是此前耗时类数字轮间波动
±10% 之外的未显性成分。§3 的旧合成数字采集于双块方案：字节可比，耗时
比仅供参考。

**配对模式当场纠正了一个上午的读数**（go-git 聚焦族复测，runs=3，
--memlimit 4096；control 462ms ±387ms 漂移告警生效）：git 侧全量克隆
墙钟 2.7–2.9s——上午双块方案里同仓同字节（9,826.8KB 位级一致）是
24–44s，上午的"clone warm 2.11x"被 git 侧异常窗口抬高了 git 成本、
**比值被美化**；配对复测下 banyan 真实仓全量克隆墙钟 **~9x**（26.2s
vs 2.9s，字节 2.05x 位级稳定）——闭包重算 + 多包在线编码的结构成本
如实显形，两个登记项（闭包跨请求复用、织入包合笼）的优先级论据随之
加强。其余：push 0.24x 耗时（0.5–1.2s→0.2s 级）、ls-remote 0.43x、
fetch 增量 0.92x 字节持平；字节类全部与上午位级一致——**字节是结构
性指标**的又一次实证。

**逐项优化落地（2026-11-14 晚，三项全实施 + 配对实测）**：

1. **响应包缓存门修正（①+② 的重复面）**：旧门用 **raw（解压后）闭包字节**
   对比 128MiB 缓存预算——而缓存存的是**编码后**包。go-git 的 raw 闭包
   超 128MiB、编码响应仅 ~20MB → 缓存永不存入 → 每次克隆重算+重编码。
   改为按编码字节入缓存（LRU 本就以此计）。实测 warm 克隆 26.2s →
   **1.8s**（closure-cache 命中，闭包整个跳过）。
2. **追加式合笼 + 超集直传（② 的冷面）**：物化合成包与推送包分离击穿
   ExactPackHit。合笼走**追加式**——主包条目区原样拷贝（OFS_DELTA 是
   相对偏移，前缀拷贝后依然有效）+ 追加对象 delta-free 落尾，不重编码
   （此前整包重编码用 Go 编码器把 C-git 推送包 10.4MB 膨胀到 20MB）；
   服务梯新增 **pack-superset-stream**——单包 ⊇ wants 时整包 verbatim
   流出（git 容忍多余对象，协议合法）。合笼调度改**合并式**（忙时标记
   重跑，不丢最后一次状态）。实测 cold 克隆 36.7s/19.0MB → **3.8s/
   11.4MB**（字节 1.94x→1.16x，耗时 13.8x→1.46x）。
3. **广告共享一次全史走（③）**：`model.ProjectionHeads` 单次 DFS 一次
   根树读推导**全部**顶层目录的投影链（逐目录 SplitHistory 的共享版，
   等价性有测试钉死，含 adoption-merge 身份规则）；视图层
   `HeadsFor` 按 tip 记忆化、整链经 synth batch 持久化为一个包
   （广告的 OID 必须可取回——gitaly fsck 逐 ref 读、GitLab 投影 fetch
   配方直接取 refs/banyan/tree/*，gitaly 集成测试实锤过这条契约）；
   有本地时间线的目录回落 SubtreeHead。**git.git 解锁**：ls-remote
   （2,031 refs + ~30 目录投影，链持久化 32.8 万合成提交）
   **47 分钟物化致死 → 1.17s / 113KB / 字节 1.01x**。

go-git 聚焦族终测（runs=3、--memlimit 4096、配对交错）：cold 11.4MB/
3.8s（1.16x/1.46x）、warm 1.9s（0.50x，快于 git CGI）、fetch 增量
0.92x 字节持平、push 0.05x 耗时、ls-remote 0.45x 耗时。剩余差距登记：
冷克隆的织入物化与枚举（~1.5s；同一成本在 git.git 尺度升级为发现⑤——
见旗舰样本）、warm 字节 1.32x（增量后包增长）、Go 编码器质量（追加式
合笼已绕开主路径）。

**下载效率的 reftable 视角（2026-11-14 分析，未实施）**：git 的
reftable 是 refs 的有序块索引格式（O(log n) 查找、前缀扫描、栈式原子
更新），治的是 packed-refs 线性扫描与松散 ref stat 风暴——**ref 存储**
的伸缩性。banyan 的 refs = timelines（meta DB 索引行）+ 派生视图：存
储层的伸缩性已在位（DB 索引），发现④的冷广告瓶颈在**派生**（每目录
全史走），把 ref 存储换成 reftable 不解决问题。reftable 真正可借的是
三条性质，映射为 banyan 层的等价物：

1. 廉价枚举 → **广告快照**：push 后增量维护一份物化好的广告 blob
   （全 refs 含投影），广告从派生计算降为读快照——冷启动/重启后 O(1)；
2. 前缀索引 → **ls-refs 按前缀惰性**：客户端带 ref-prefix 的定向
   fetch 只解析命中的 ref（完整 clone 不带前缀，此项只救定向场景）；
3. 栈式增量更新 → 快照按 push append + 后台合并（reftable 文件栈的
   同构）。

与 reftable 无关、但按实测优先级更高的下载效率项：④共享一次全史走 +
投影 head-only 推导（广告快照的实现载体）；闭包跨请求复用（clone
warm 2.11x 耗时的主因——闭包是 wants 纯函数，重算是纯浪费）；织入
合成包与推送包的**合笼**（多 pack 状态击穿 ExactPackHit，20MB 克隆走
在线编码——把织入产物 fold 进主包可恢复 verbatim 直传）；客户端侧
partial clone（0.45x 字节）仍是现成的最大杠杆。**结论：先修派生与
闭包；reftable 不进存储层——meta DB 已在它的位置上，它的形态学作为
广告快照的实现参考。**

**git.git 旗舰样本**（85,613 提交 · 2,031 refs · 324MiB · branch master）：
发现④修复前**整体不可测**（第一个 ls-remote 的投影物化 47 分钟无输出死亡、
诊断期残留进程常驻 9GB——push 与摄入本身正常，82,466 提交的轻量索引 27 秒）。
③ 落地后广告解锁，但全历史 wants 的 clone 形态撞上**发现⑤**——分档如实记录：

**可测车道**（默认配置、runs=3、配对交错、--memlimit 6144，2026-11-14）：

| 场景 | 字节比 | 耗时比 | 备注 |
|---|---|---|---|
| ls-remote（冷广告=投影链共享走+持久化） | **1.01x**（113.3 vs 111.8KB） | 1.29x | 修复前 47 分钟物化致死 → 1.1s |
| clone-shallow depth=1 | **1.08x**（14.7 vs 13.6MB） | **0.54x**（31.9 vs 59.0s） | 旗舰尺度首条反超车道：闭包=织入 tip+HEAD 快照，不碰全史 |

（本轮 control 1849ms ±1601ms——漂移告警生效，耗时比降级读，字节位级稳定。）

**发现⑤：全历史 wants 的 clone 服务在 git.git 尺度无界累积（2026-11-14 实测登记）**。
clone-full / clone-partial / clone-subtree 三形态三次独立复现（默认配置两次、
单包化实验一次）：冷服务 **20+ 分钟零字节送出**、进程 WS 8.5→9.1GB 爬升、
12 核全燃（GC 极限运转）、`--memlimit 6144` 被活集合击穿，按内存纪律三次击杀。
机制两层：

1. **织入根链未持久化（主因）**：根视图是线性无合并链（docs/design/
   data-model.md §13），git.git 的合并富历史织入后几乎每个提交都被重写——
   这些织入对象**不在任何 pack 里**（广告期只持久化了目录投影链 328K 个）。
   clone wants 含整条织入链 → ExactPackHit/PackSupersetHit 永不命中 →
   每请求在线物化+编码整条链。
2. **合笼分组上限是次要放大器**：Consolidate 的 256MiB 每组输入上限把
   [投影包 134MB, 推送包 310MB] 拆成两包（3→2），t/ 子树闭包（提交在投影包、
   树/blob 在推送包）横跨两包同样击穿超集梯。A5 实验
   （`BANYAN_BENCH_CONSOLIDATE_MAX=1GiB` 强制单包，日志验证
   `2 → 1 packs, 492,771 objects`）后冷克隆**依然 9GB 打转**——单包化治不了
   织入链缺失，主因坐实；go-git 尺度同一机制顺畅（全量 ~20MB 单包直传 3.8s）
   只因它的合笼产物本就单包且织入链物化只有 ~9.6K 提交。

卡死期间合成批**确有增量落盘**（mid-clone 日志 `consolidate 738,940 objects`，
较首合笼 +246K）——不是死锁，是慢且无界的物化；16GB 主机撑不到收敛点。
诊断 dump（`TestRealSampleDiag`，6 分钟预算复现）钉住服务 goroutine 停在
`enumerateClosure → Prefetch → LocateObjects` 的 meta 锁等待——持有者正是
本次克隆**自己**调度的后台 Consolidate（`registerAndSwap → PutPack` 正在
注册 32.9 万对象的合笼包）：自伤循环=枚举物化 → 合成批 flush → 调度合笼 →
锁竞争+位置缓存失效 → 枚举代价重付；GC 在 ~9GB 堆上全核运转。
修复方向登记：①织入根链在 weave/广告期持久化（照 HeadsFor 投影链的模式：
共享一次全史走 + treeMemo 每树一读）；②服务端闭包枚举改共享走+流式有界；
③两者落地后 clone wants ⊆ 刷新包 → 超集 verbatim 直传（go-git 3.8s 模式
放大到旗舰尺度）；④**合笼静默窗口**——枚举/流式服务期间挂起后台合笼
（请求末再触发），消除自伤性锁竞争与 loc 失效。**其余场景族
（fetch-incremental / push-one-commit / dev-loop / burst / clone-partial /
push-subtree）的 setup 都要克隆对端**，被 ⑤ 一并挡住——修复后补测。

复现：`go run ./cmd/banyand bench --repo <path> --runs 3 --work <dir>
--md report.md --csv samples.csv`；大样本用 `--scenarios` 聚焦（全部族：
clone-full、ls-remote、fetch-incremental、push-one-commit、clone-shallow、
clone-partial、dev-loop、burst、clone-subtree、push-subtree）+ `--memlimit`
设软上限；发现⑤的单包化对照实验加 `BANYAN_BENCH_CONSOLIDATE_MAX=<bytes>`
（诊断旋钮，非调优建议）。

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

## 6. 仓库规模统计与全 clone 预算（2026-10-25，T48）

网关维护仓库级规模统计：`RepoBytes = Σ pack 字节数`（尺寸内容不可变故
只探测一次；push/repack 在源头直接 prime，`/api/v1/repos/<repo>/size`
返回 `{bytes, packs}`），并以 `full_clone_max_bytes`（默认 100GiB，-1 关闭）
约束 **clone 形态** fetch（无 haves/depth/filter/shallow 且全部 want 为
广告 tip——"把整个仓库给我"的签名）：

- 拒绝响应带可行动信息：改用 `--depth 1` 浅化或（filter 广告后）
  `--filter=blob:none` 部分克隆；v2 中 ERR 包置于 packfile 段头**之前**
  （段后 ERR 会被解析成 sideband 帧位——"bad band #N"教训）。
- 豁免形态：增量 fetch（有 haves）、浅化（depth）、partial lazy 单对象
  fetch（want 非 tip）、子树视图 clone（天然有界）。
- 统计失败 fail-open（规模查不到不阻断 fetch）；`repo_bytes`
  同值注入 hook 回调 payload（docs/gateway/git-compat.md §7），规模策略可写在回调服务里。

动机：多 pack 大仓的完整 clone 走 O(闭包) 在线编码路径（曾是 docs/storage/storage.md
开放项）——预算闸门先于该路径把最坏情况挡在门外。**2026-10-27 更新**：
该路径已流式化（docs/storage/storage.md §3a，T55——72MiB/3 pack clone 在 64MiB 软限制
下峰值 12.3MiB），预算闸门保留为规模治理手段而非内存兜底。**同日二轮**：
deepen/unshallow（浅化豁免形态里唯一会拉全史的）也已流式化——同一
rung、类型化枚举带深度/嫁接/have 裁剪、与 ladder 集合等价（T58：
54MiB/3 pack unshallow 峰值 12.2MiB）；节点客户端超时分层（Transport
级拨号/响应头预算 + ctx 管总量，取代全局 5 分钟 Timeout——GB 级 pack
传输不再被无进展无关的墙钟掐断）。剩余开放项：带 haves 增量 fetch 的
极端无公共提交形态（见 docs/storage/storage.md §3）。

## 7. 运维面观测（2026-10-26，T54）

性能排查不再依赖进程内插桩：`debug.pprof_addr`（独立辅助监听，
docs/gateway/service-api.md §5a）开放 pprof 全家 + `/debug/stats`（GOMEMLIMIT 读数与
MemStats 精要）+ `/debug/vars` 的**服务路径计数器**（fetch 五级阶梯
与 push 计数）——"实际走的哪级服务阶梯"从日志考古变成即时读数；
T46/T50 式内存链测量也从测试专有采样（BANYAN_HEAPPROF）获得生产
等价物。
