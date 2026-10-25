# 16 · 性能对比实测报告（banyan vs git 原生）

> 工具：`cmd/banyan-bench`（docs/11 §3）；对等基线：真 `git http-backend`
> CGI，同机同传输同对象集。本文是实测快照与归因；方法与场景矩阵见
> docs/11，本文不重复。原始样本由工具 `--csv` 再生，不入库。

## 1. 多轮验证与数字稳定性（2026-10-25，Windows 开发机）

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

## 2. 劣化场景归因（对象集全部验证相同）

三个 banyan 落后的指标逐个做了 pack 级取证（独立 probe 仓 + bench
保留工作目录，`verify-pack -v` 逐对象对比）：

**① clone 全量 1.07x —— 双方 delta 化时机不同，非实现缺陷。**
对象数与 delta 数完全相同（1087=1087），逐对象 delta 后大小差 ~3.5%。
banyan 走 ExactPackHit **verbatim 直传客户端 push 的 pack**（零拷贝）；
git http-backend 每次服务**现场重新 delta 化**（C pack-objects）。两次
delta 化（客户端打包时 vs 服务端服务时）的基选择不同 → ~8% 总差。
**repack 反而更差**：对同仓跑 banyan repack 后 serve 字节 229.9KB
（> verbatim 215.2KB > git 199.4KB）——banyan 的 Go delta 编码器
（go-git EncodePackDelta 移植）压缩质量低于 C diff-delta。
**结论：现状即最优解**——7% 字节换零拷贝直传；repack 的价值在碎片
合并运维，不在 serve 字节（部署不必为带宽而 repack）。

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

## 3. 结论

| 维度 | banyan vs git | 性质 |
|---|---|---|
| 结构能力（子树克隆/目录级推送） | 全量的 5.9–14%；git 无对应 | 模型级优势，随规模放大 |
| 并发收敛（burst-8） | 耗时 0.52–0.66x，字节持平 | 协议往返次数优势（收敛列车 vs rebase 重试） |
| 稳态传输（全量/增量/推送） | 字节 0.87–1.07x | 持平；7% 上限来自 verbatim vs 现场 delta 的取舍 |
| 离散能力（浅化/partial） | 0.65–1.16x | 编码层波动，对象集恒等 |

复现：`go run ./cmd/banyan-bench --profile medium|large --runs 5
--md report.md --csv samples.csv`（多轮验证重复执行多次取区间）。

历史快照：2026-10-25 首轮报告曾置于 workspace 根目录，本文即其入库
版本并补充了三轮验证与归因。
