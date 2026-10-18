# 09 · 索引与打包：对象检索效率 + 客户端传输轻量化

对照基准：git 源码 `commit-graph.c`（代数与 GDAT chunk）、`pack-objects.c`
（排序/delta 窗口）、`upload-pack.c`（have 减除 = mark-uninteresting）。

## 1. 索引现状（三层 + 轻量层）

| 层 | 内容 | 位置 |
|---|---|---|
| 磁盘 | 每 pack idx v2（oid→offset），IngestPack 时写入 | `storage/idx.go` |
| 内存 | CommitGraph（oid→tree/parents/when）+ TreeMemo（tree→entries） | `storage/index.go` |
| 全局 | oid→(pack,node) 表 = MIDX 形态（跨 pack 定位） | MetaStore objects |
| 轻量 | bmsg 提交索引（docs/01 §11，消息内容寻址去重） | `storage/index.go` |

## 2. 本轮新增：代数 + 树反向索引（2026-10-18 落地）

**代数（generation numbers）**：`gen(c) = 1 + max(gen(parents))`，git
commit-graph 语义。记忆化惰性计算（`Generation`），遍历顺带回填提交图与
树索引。**精确性协议**：祖先缺失（不该发生——对象集完备不变量）时该值是
下界，标记 tainted（`genTainted`）并返回 exact=false；**剪枝只使用精确值**
——低估的代数若参与剪枝会误剪真祖先（守卫误拒）。新增提交只增不改，记忆
永不失效（不可变 DAG）。

消费点：

- `view.descends`（v5 全部守卫/标记的基石）：`gen(from) ≤ gen(target)` 直接
  判否；BFS 中 `gen(parent) < gen(target)` 的父链整枝剪掉。O(历史) →
  O(距离)，且首次计算后全记忆命中。
- 单元测试：`storage.TestGenerationDiamond`（菱形 DAG 精确值 + 缺失父污染）。

**树→提交反向索引**（`treeCommits`）：每提交一条（总内存 O(提交数)，故意
不设上限——截断会漏掉真正携带该树的祖先，TreeKnown 假阴性 = 织入重复）。
`view.TreeKnown`（收编去重的关键判定）从全历史线性扫描 → 候选查表 +
代数剪枝祖先检查；先 `Generation(from)` 预热回填，保证候选完备。

## 3. 打包优化

### 3.1 have 减除（已实现，docs/03 §10 偏离 #1 关闭）

语义对齐 `upload-pack.c` mark-uninteresting：客户端 have X ⇒ 其持有 X 的
完整闭包（git 仓库不变量），故 **haves 已知提交可达的对象全部排除**，主遍
历不发射、不展开；wants 自身永不排除（退化客户端 want=have 也要交付）。

**浅客户端修正**（关键，T15/T16 钉住）：浅仓客户端在嫁接边界之下没有对象，
不变量不成立——

1. 排除集的提交遍历在客户端上报的 shallow 边界**止步**（边界提交及其树可
   排除，其父链不可）；
2. 主遍历对被排除提交**穿过而不发射**（受深度边界裁剪约束）：unshallow 要
   的恰是排除集之下的对象，只有穿过排除提交才可达。非浅客户端在排除前沿
   直接剪枝（省遍历）。

代价：每次增量 fetch 多一次 have 侧遍历（与 closure 同阶）——git 无位图时
同样付出；位图（§3.3）是终解。实测（T19）：单文件增量 =
**{1 提交, 2 树, 1 blob}**，对照全量闭包 11 对象。

### 3.2 delta 化（设计，repack 落地）

现状：`EncodePack` 无 delta（`packfile.NewEncoder(w, st, false)`，正确性优
先）；全量克隆走 **ExactPackHit 整包直传**（客户端收到的就是推送时的原始
pack，含 delta）——轻量化缺口在**合成路径**（增量包、子树投影包）。

git 对照（`pack-objects.c`）：对象按（类型→目录路径→name-hash→大小倒序）
排序后滑窗（默认 window=10、depth=50），`diff-delta` 选基（同目录 blob 优
先），写 OFSD_DELTA（链内偏移寻基）优先于 REF_DELTA。

落地路径（优先级序）：

1. **repack（M3，桩已就位）**：碎片合并时统一 delta——PutSynthetic 单对象
   pack 是最大碎片源；合并 + 窗口 delta 后，ExactPackHit 命中率与直传带宽
   同步改善。repack 与服务路径解耦（后台事务切换 pack 版本）。
2. **合成路径按需 delta**：have 减除后增量包本身很小，delta 收益有限、
   CPU 在线付出——仅当基准测试证明必要时启用（`NewEncoder` 第三参已支持）。
3. **可达性位图**（对照 pack bitmaps）：闭包/排除集从遍历 → 位图并集，
   消除 §3.1 的 have 侧遍历代价；挂在 repack 产物上（git 同款绑定）。

### 3.3 性能账本（演进记录）

| 指标 | 值 | 来源 |
|---|---|---|
| 28,649 对象闭包（go-git 仓） | 88ms | docs/02 §1.3 |
| ExactPackHit 整包直传 | serve 2.77s → 24ms | docs/02 §1.3 |
| 子树 clone 对象比 | 1547 vs 30203（19.5×） | docs/07 |
| 增量 fetch（have 减除后） | 11 → 4 对象 | T19 |
| descends/TreeKnown | BFS/线性扫描 → 代数剪枝/候选查表 | 本篇 §2 |

## 4. 测试矩阵

`integration/packing_efficiency_test.go`（T19：rawFetch 协议级断言增量包
精确构成 + 真实客户端端到端）+ `storage.TestGenerationDiamond`（代数精确/
污染语义）。登记于 docs/07。
