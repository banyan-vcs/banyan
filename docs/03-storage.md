# 03 · 分布式 pack 存储

## 设计约束（用户原话锚定）

> 采用 **packfile + idx 存储，而非松散对象**。packfile 采用**分布式存储方案**，并能通过 **idx 索引实现分布式检索、打包**。

三条硬约束：

1. 服务端不产生松散对象——push 进来的 pack 本身就是存储单元；
2. pack 分布式放置于多存储节点；
3. idx 是跨节点定位对象、以及后台重打包的基础。

## 1. 分片模型

### 1.1 pack 即不可变分片

```
Shard ::= {
  pack_oid   OID        // pack 内容哈希（pack trailer checksum），全局唯一
  bytes      []byte     // 标准 git packfile（v2）
  idx        IdxV2      // 标准 git idx v2：oid → (pack 内 offset)
  origin     enum { push, projection, repack }   // 来源
  repo       RepoID
}
```

- **不可变**：pack 写入后永不修改（repack 产生新 pack，旧 pack 延迟 GC）。
- **来源三类**：
  - `push`：receive-pack 收到的客户端 pack，解包校验后**原样入库**（不解散成松散对象——这正是与 git 服务端的根本差异）；
  - `projection`：虚拟子树仓库的投影对象在线合成出的 pack；
  - `repack`：后台碎片合并 + delta 化产物。

### 1.2 放置：rendezvous hashing

```
节点选择：node = argmax_{n ∈ 活跃节点} H(pack_oid, n)
副本因子 R（默认 3）：取哈希序前 R 个节点
写入：主节点写成功 + R-1 副本确认 → 元数据提交
```

选 rendezvous 而非一致性哈希环：节点数变化时只重映射 1/N 的 key，无需虚拟节点层，代码可读（骨架中为完整实现）。

### 1.3 双 idx 体系

| 层 | 内容 | 位置 |
|---|---|---|
| **pack 局部 idx** | oid → pack 内 offset，git idx v2 格式（兼容 git 工具链） | 随 pack 存于分片节点 |
| **全局对象索引** | oid → (pack_oid, node_id[, offset]) | 中心元数据服务 |
| **内存派生索引** | CommitGraph（oid→tree/parents/when）+ TreeMemo（oid→解析条目） | ObjectService，push 时构建（`internal/storage/index.go`） |

**实现状态（2026-10-17，M1 已落地）**：

- 磁盘层：push pack 原样落盘为 `pack-<oid>.pack` + `pack-<oid>.idx`（idx v2，真实 offset 与 crc32，均来自 Parser Observer 回调——按位配对在 delta pack 上会错位，P1-1 单对象解压时被击落）；
- 内存层：CommitGraph/TreeMemo 在 IngestPack 时零额外 IO 构建；闭包计算与投影遍历全部走索引——**28,649 对象的闭包 88ms**（此前为逐对象解码的主导开销）；
- **热路径整 pack 直传**（原 M3 项提前实现）：闭包恰等于某已存 pack 的对象集时直接流式回传 pack 字节（`ExactPackHit`），跳过重哈希+重压缩——根仓库 clone 的 serve 阶段从 2.77s 降到 **24ms**，端到端 4.4s → 1.7s。

- 全局索引是"分布式检索"的入口：任何 banyand 进程拿到 wants 后，一次批量查询即得到每个对象所在 pack 与节点。
- offset 冗余存于全局索引是 M2 优化（省一次 idx 往返）；M1 可只存 (pack_oid, node_id)，读时用 pack idx 解析 offset。
- **删除是元数据层的**：repack 后旧 pack 的对象条目被新 pack 条目替换（版本化批量事务），旧 pack 等引用计数归零后物理删除。

## 2. 元数据服务（MetaStore）

中心元数据是唯一串行点，承载：

```
MetaStore
├── Repos            仓库注册表
├── PackRegistry     pack_oid → {nodes, size, idx 摘要, origin, 状态}
├── ObjectIndex      oid → (pack_oid, node_id)         ← 全局对象索引
├── Timelines        (repo, anchor, name) → {head OID, base marker, 版本}
├── WAL              每 repo 一条线性化日志（append-only）
└── RepackPlan       后台重打包任务队列
```

- **线性化保证**：每 repo 的 WAL 由单写者顺序消费（M1：进程内互斥锁 + 内存态；M2：独立元数据服务，DB 事务；生产选型见 §6）。对象 CAS 写入不进 WAL（幂等），只有 head 前进与索引提交进 WAL。
- **head CAS**：`UPDATE timelines SET head=:new WHERE repo=:r AND anchor=:a AND name=:n AND head=:expected`——乐观锁，失败即非 fast-forward。
- **接口即抽象**：Go 接口 `storage.MetaStore`，M1 内存实现 / M2 落地实现可替换，上层（model/gitwire）不感知。

### 2.1 接口分权（M2 落地，2026-10-18）

`MetaStore` 拆为四个角色接口的组合——每块数据形态与访问模式不同，未来可按块更换后端而 gitwire 不感知：

```go
type RepoRegistry   interface { CreateRepo, GetRepo }                  // repo registry
type PackRegistry   interface { PutPack, GetPack, ListPacks }          // central packs: pack → replica nodes
type ObjectIndex    interface { LocateObjects }                        // object lookup: oid → (pack, node[, offset])
type TimelineStore  interface { Timeline, CASHead, EnsureTimeline, ListTimelines }  // branch ref-map
type MetaStore      interface { 上述全部 }                              // composite (consumers keep the whole)
```

- **分支 ref-map**：`(repo, anchor, name)` 即 ref 的唯一键——根 anchor 下 name=分支；子树 anchor 下 = 目录时间线。`ListTimelines(repo, anchor)` 服务广告（每次 clone/fetch 都要），是唯一范围扫描。
- **中央 pack**：`PutPack` 幂等注册（pack 不可变 ⇒ INSERT-ONLY）；`GetPack/ListPacks` 服务跨节点取包与 repack 枚举。
- **对象检索**：`LocateObjects` 批量点查（wants → pack 集）。**offset 列冗余**（省一次 idx 往返）为 schema 预留、M3 填充。

### 2.2 关系型 vs KV：M2 落地选型

按数据人口与访问模式逐表分析（选型的第一性依据）：

| 表 | 行数（大仓目标） | 可变性 | 主要访问 |
|---|---|---|---|
| repos | 10^0–2 | 极少 | 点查 |
| timelines（ref-map） | 10^1–4 / repo | **CAS 头更新（热）** | 点查 + **前缀枚举（广告）** |
| packs | 10^2–5 / repo | insert-only | 点查 + 枚举 |
| objects（对象索引） | 10^6–9+ | **insert-only，永不变**（pack CAS ⇒ 映射不可变） | 批量点查 |

- **CAS（RMW 线性化）**：ref-map 头推进是读-比-写，天然要事务。关系库一行 `BEGIN IMMEDIATE … UPDATE … WHERE head=:expected` 即得；KV 需单键事务或客户端 CAS 循环拼装，各库方言不一。
- **枚举/范围扫描**：广告路径 `ListTimelines(repo, anchor)` 是前缀扫描——关系库 `WHERE repo=? AND anchor=?` 直达；KV 要靠键编码（repo+anchor 作前缀）自建，且失去运维期 ad-hoc 查询能力（排障成本）。
- **规模压力只有一张表**：objects 的"insert-only + 纯点查"正是 LSM/KV 的教科书负载；B-tree 关系库在 10^9 行量级写放大 ~3–5×。但 M2 单机区间（≤10^8）SQLite 批量插入 + `WITHOUT ROWID` 聚簇主键完全胜任，且该表已隔离为 `ObjectIndex` 独立接口——亿级以上时**单独**毕业到 KV/LSM（Pebble/RocksDB 类），控制面（ref-map/pack/事务）留在关系库。
- **运维形态**：SQLite = 单文件 + WAL + 复制即备份，与"单写者元数据"（§2 线性化设计）一一对应；MySQL/PG 是多进程同型（§6 生产路径不变）。

**结论：M2 选关系型（SQLite 起步，MySQL/PG 平滑接替）；KV 不作整体首选**——其优势区（亿级 insert-only 点查）目前仅 objects 一张表，且已通过接口隔离预留迁移路径；把 ref-map 的 CAS 与枚举塞进 KV 是用短板换长板。

### 2.3 SQL 后端实现要点（SQLite 单机 / MySQL、PostgreSQL 集群）

- 驱动 `modernc.org/sqlite`（纯 Go、无 cgo——OS 无关，见 AGENTS.md）。
- 方言层（2026-10-27，HA-2）：MySQL（go-sql-driver）/PG（pgx）共享一套 schema 与全部语句，分歧点=DLL 类型与索引长度/OR IGNORE 与 UPSERT 拼写/占位符 rebind（?→$N）/唯一冲突判定（1062/23505）；CAS 在 MVCC 后端改为 SELECT...FOR UPDATE + 插入竞态按 PK 冲突归 ErrNonFastForward；conformance 矩阵四后端同一套（env 门控 BANYAN_TEST_{MYSQL,POSTGRES}_DSN）。
- PRAGMA：`journal_mode=WAL`、`synchronous=NORMAL`、`busy_timeout=5000`。
- `objects` 表 `WITHOUT ROWID`（主键聚簇，点查即索引扫描）；插入按 500 行/事务分块。
- CAS：`BEGIN IMMEDIATE` 内 SELECT-比-UPDATE/DELETE，失败回滚映射 `ErrNonFastForward`（与 git push 语义一致，docs/02 §4.3）。
- 幂等：packs/objects 一律 `INSERT OR IGNORE`（首写者胜；pack 不可变 ⇒ 无冲突语义）。

## 3. 读路径（服务一次 fetch，2026-10-25 起窗口化）

```
wants = 客户端协商所得对象集合
  ⓪ clone 形态快路径（无 haves/depth/filter/shallow）：
       枚举闭包——只收集 OID，类型导向遍历（commit/tree 走元数据
       索引；tree entry 按 mode 分类：目录→子树、160000→外部仓提交
       （本仓不持有，跳过）、其余→blob 叶，**blob 字节永不触碰**；
       want tip 用零 IO 类型探针（synth 表 + commit/tree 索引），
       未命中才一次 Raw（annotated tag 剥皮））
       → ExactPackHit：对象集 == 某存储 pack 的对象集时
         该 pack 流式直传（节点盘 → 网络 → 响应，256KiB 窗口；
         网关磁盘缓存命中则盘 → 响应，未命中边流边写穿缓存）
       ——闭包（对象字节驻留）完全跳过
  ⓪' 多 pack clone 形态（精确命中 miss，docs/03 §3a 流式编码）：
       响应缓存前置（同 wants 重复克隆免走查——旧路径命中缓存
       仍要付全量 closure 计算）→ 类型化枚举（⓪ 同款）→
       Placements 按 pack 分组（组内/组间排序定序，合成对象居首）
       → StreamEncodePack 逐条目写（边哈希边 zlib，delta-free），
       对象经读取梯子逐个窗口读——除当前对象外零驻留；
       pack 组总字节 < 32MiB 的小仓回退 ⑤（保响应缓存收益）
  ⓪'' deepen 形态（depth>0 或携带 shallow：shallow clone / deepen /
       unshallow，2026-10-27）：同一流式 rung，枚举带上深度边界
       （shallowCut 裁剪：边界提交含其树、父链止步）、客户端嫁接点
       （穿透排除提交继续走）与 have 排除（haveClosure，与 ⑤ ladder
       集合等价、单测钉死）；浅化段是协议层、早已上线（v1
       writeShallowSection / v2 shallow-info），rung 只产 pack。
  ① 快路径未命中 → 全局索引批量定位：oid → (pack_oid, node_id)
  ② 本地 pack cache（LRU，按 pack_oid 键）命中检查
  ③ 未命中的对象：单对象区间读取（idx 偏移 + 文件窗口解压，
     O(对象) 内存；delta 链递归同样窗口化）
  ④ 整包拉取 + 解码 memoize 仅接管 ≤32MiB 的 pack
     （PackSize 探测：网关缓存索引 / 节点 HEAD；大小不可变故缓存）
     ——大包永不整包驻留网关内存；单对象读的"8 次翻转整包"
     仅集群线（PrefersWholePack：每次单对象读是 RPC），本地后端
     永不翻转（抽取即文件读；T50 复盘，见 docs/17 §7）
  ⑤ 在线合成响应 pack（含响应缓存 / thin；走 closure，小仓主路径）
  ⑥ side-band 流式回写
```

缓存三层同构：同一套「定位 → 拉取 → 编码」逻辑运行在 banyand 进程内 cache、独立的 cache 层、存储节点上——pack 不可变 ⇒ 缓存键即 pack_oid ⇒ 无失效问题。

### 3.1 全链路内存预算（T46 钉住）

小内存可用性是设计约束而非副产物：`integration/memory_chain_test.go`
以 64MiB 软内存限制（GOMEMLIMIT 语义）+ 堆峰值采样，验证 150MiB
不可压缩仓库经 git → 网关 → 3 存储节点的 push/clone（v2+v0）全链路：

| 方向 | 修复前峰值 | 现峰值 |
|---|---|---|
| push（150MiB 包 ×3 副本） | 783 MiB | **13.5 MiB** |
| clone v2 | 611 MiB | **12.4 MiB**（11.3s → 2.4s） |
| clone v0 | 512 MiB | **9.9 MiB** |

修复前的三层整包驻留：节点 PUT `io.ReadAll`（×3 副本并发）、
节点 GET 整包读出 + 单对象提取整包 ReadFile（每次对象读都整包！
alloc profile 占 93.8%）、网关 `GetPack` 整包拉取 + closure 全图驻留。
窗口化后各环节内存 ∝ 窗口（64KiB~256KiB）+ 对象数元数据，与包大小无关。

### 3a. 多 pack clone 形态流式编码（2026-10-27，T55）——"进得去、出不来"关闭

分批流式导入（§4）天然产多 pack 大仓，其完整 clone 的闭包不等于任何
单 pack——精确命中必 miss，旧路径落进 closure() 的 O(闭包) 对象图
（72MiB 仓在 64MiB 软限制下不可能完成）。三段流水线修复（§3 ⓪'）：
**类型化枚举**（OID+类型，元数据索引，零对象字节）→ **按 pack 分组**
（Placements 批量定位；组内组间排序定序输出，合成对象居首——集群线
上 LRU 连续性防解码缓存抖动，本地线上即页缓存友好）→ **逐条目流式
写出**（StreamEncodePack：pack 头带对象数故先枚举后写体；边哈希边
zlib；delta-free——用 wire 尺寸换内存纪律，增量收益归 thin/在线
delta 路径）。响应缓存前置：同 wants 重复克隆连枚举都免（旧路径
命中缓存仍付全量 closure 计算）。实测 T55：**72MiB / 3 pack 完整
clone 在 64MiB 软限制下完成，峰值堆 12.3MiB**（0.17×仓）。

仍为 O(闭包/段) 的已知路径（开放项，2026-10-27 再收窄）：**带 haves
的增量 fetch** 走 closure 阶梯（结果集 = 增量、通常天然有界；极端
形态——客户端无公共提交——等价全量 clone 且不受预算闸门保护，闸门
只认 clone 形态）。deepen/unshallow 已流式化（⓪''，T58：54MiB 三
pack 仓 unshallow 峰值 12.2MiB）。已窗口化的配套路径（2026-10-25
二轮审查）：reconciler 副本修复（源流式校验 + 目标各自流式拉取，
节点落地二次校验）、repack 合并（组输入字节上限 256MiB + spool
编码 + 流式入册，不再双重解码）、decoded-pack memoize（总量 LRU
上限 256MiB，单包 32MiB 闸门之上再封顶）、repack 丢弃路径的存储键
修正（hashed 寻址下旧分片此前删不到）。

## 4. 写路径（服务一次 push，2026-10-25 起流式化）

```
receive-pack 收到 (ref 命令行, pack 流)
  ① spool：请求体流式落盘临时文件（64KiB 窗口），滑动窗口哈希
     边流边校验 pack trailer——内存与 pack 大小无关
  ② 流式扫描：Parser 无 storage 模式（seekable 文件）只收集
     order/offsets/crcs 元数据 + 写 idx v2（偏移全链 uint64，
     大偏移走 8 字节表——单 pack 无 4GiB 限制）
  ③ 校验：head 对象存在性（全局索引）、锚点合法性
  ④ pack 流式写入分片：每副本独立打开 spool 文件（io.Copy，
     不解算不驻内存；rendezvous × R 副本）；节点侧同样窗口化——
     PUT 请求体直接流式落盘（临时文件 + 滑动窗口哈希校验 trailer
     + OID 同一性 + Content-Length 核对，原子 rename）   ← 不解散！
  ⑤ 元数据事务（WAL 单写者）：
       ObjectIndex 批量 upsert（oids → 新 pack）
       Timelines head CAS 前进
  ⑥ commit/tree 索引惰性回填：首次读取时从 idx 偏移按需解压
     （CommitInfo/TreeEntries 的 miss→解包→回填路径）；
     整包解码缓存不预热——大仓导入的常驻内存 ∝ 对象数元数据
  ⑦ report-status 回写客户端，spool 删除
```

空 pack（0 对象）是合法 wire 形态（客户端推服务端已有对象）——直接
跳过落库。步骤 ④⑥ 并行；串行点只有 ⑤ 的 head CAS。

**存量大仓分批导入**（`banyan import`，2026-10-27，docs/06 §3.6）：客户端
编排工具把源仓按拓扑序切成字节预算内的段逐段 push——每段走一遍上述流式
写路径，内存 ∝ 段大小；段间碎片一等公民（repack 前 ExactPackHit/流式
编码器照常服务）。断点续传=重跑从远端 tip 起算。

## 5. 后台 repack（几何打包，2026-10-25 起）

push pack 通常小而碎（一次 push 一个 pack），长期累积会拖垮读路径。
repack 采用 **git 几何打包策略**（`--geometric=2`，builtin/repack.c 的
分组语义）：

```
触发：packs(repo) ≥ 阈值（默认 64）
分组：pack 按对象数升序；相邻比 p[i+1] < 2×p[i] 的连续段为一组
      （相邻比而非累计——累计判据会滚雪球吞掉大 pack）
      组输入字节超上限（默认 256MiB）再拆——等尺寸大包不会组团
      突破合并内存预算（对象数判据对字节不设防）
合并：仅组内 >1 的组——每组合成一个 delta pack；编码直写 spool 文件
      + 流式入册（无整包输出缓冲、无二次解码）；单元素组不动——
      大 pack 永不重打，合并成本 ∝ 碎片段而非仓库
协议：注册先行（对象位置重指向）→ 删旧分片（存储键而非仓库名——
      hashed 寻址）；中断任意点最多超副本
```

与旧全量合并的差异：300GB 仓不再触发 300GB 重打（旧策略在阈值之上
不可行）；碎片合并与 delta 化的成本被几何序列约束在最小段。

## 6. 存储寻址模式与 rename（2026-10-25，T40）

仓库名 → 存储键的两种映射（`storage.mode`，gateway/refs 须一致；存储
节点无感知——它只是键的仓库）：

| | named（默认） | hashed |
|---|---|---|
| 磁盘/网络键 | 仓库名（目录可读） | `@hashed/<xx>/<yy>/<sha256(storage_id)>` |
| rename/组迁移 | 集群动作：先在全部持有节点移动目录（幂等，可重跑），后翻转注册表 | 仅注册表翻转，存储层零动作 |
| 名字可读性 | 保留在存储层 | 存储层移除——映射在**网关**（ObjectService 经 refs 注册表解析名字→键） |
| 适用 | 小集群/调试友好 | 大集群推荐（GitLab @hashed 同构）：rename/迁移零成本 |

**不变 storage id**：每仓库在 CreateRepo 时铸造（crypto/rand，落
SQLite，老库打开时自动 ALTER+回填）；hashed 键 = 它的哈希 fanout，
因此 **rename 后键不变**，新旧名共享同一存储树。元数据行（timelines/
packs/objects/placement…）仍按名字键控，rename 为单事务全表改名。

**rename 语义**（`POST /api/v1/repos/<repo>/rename`，docs/07 §5 认证门）：
- hashed：refs 注册表一次翻转；
- named：网关向健康视图内**所有**节点发 `POST /internal/pack/rename`
  （缺源目录=OK 幂等），全部确认后才翻转注册表——数据始终先于名字。
  **边界（诚实）**：改名瞬间存在短暂不可读窗口；不在健康视图内的节点
  保留旧目录，节点回归后重跑同一 rename 补齐（reconciler 自动补齐为
  HA-2 项）；目标名已存在则拒绝（不做合并）。

## 6a. 生产选型（设计层面，M2+）

| 组件 | 选型 | 说明 |
|---|---|---|
| 元数据服务 | 独立 ref 服务承载前序判断（[10-ha.md](10-ha.md) §0 v0.3 定稿：与 pack 存储分离；后端 MySQL/PG 或内嵌 raft） | WAL 与 CAS 需要线性化；pack 大流量不经串行点 |
| 存储节点 | 无状态 HTTP 服务，底层对象存储（S3 类）或本地盘 | pack 不可变，S3 语义天然契合 |
| 副本策略 | rendezvous × R，或对象存储自带 EC | pack 大且不可变，纠删码收益高 |
| 缓存层 | 独立 cache 进程（LRU by pack_oid） | 三层同构，见 §3 |

## 7. 骨架对应

| 本文档概念 | 代码位置 |
|---|---|
| rendezvous / 副本放置 | `internal/storage/shard.go`（完整实现） |
| ShardStore 接口 + 本地盘实现 | `internal/storage/shardstore.go` |
| pack/idx 读写封装 | `internal/storage/packfile.go` |
| MetaStore 接口（四角色组合）+ 内存实现 | `internal/storage/metastore.go` |
| MetaStore SQLite 实现（WAL + IMMEDIATE CAS） | `internal/storage/metastore_sqlite.go` |
| repack 骨架 | `internal/storage/repack.go` |
