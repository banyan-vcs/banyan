# 02 · 分布式 pack 存储

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

- 磁盘层：push pack 原样落盘为 `pack-<oid>.pack` + `pack-<oid>.idx`（idx v2，真实 offset，Scanner 头序与 Parser 插入序按位配对；crc 段暂 0，M2 补）；
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

### 2.3 SQLite 实现要点

- 驱动 `modernc.org/sqlite`（纯 Go、无 cgo——OS 无关，见 AGENTS.md）。
- PRAGMA：`journal_mode=WAL`、`synchronous=NORMAL`、`busy_timeout=5000`。
- `objects` 表 `WITHOUT ROWID`（主键聚簇，点查即索引扫描）；插入按 500 行/事务分块。
- CAS：`BEGIN IMMEDIATE` 内 SELECT-比-UPDATE/DELETE，失败回滚映射 `ErrNonFastForward`（与 git push 语义一致，docs/01 §4.3）。
- 幂等：packs/objects 一律 `INSERT OR IGNORE`（首写者胜；pack 不可变 ⇒ 无冲突语义）。

## 3. 读路径（服务一次 fetch）

```
wants = 客户端协商所得对象集合
  ① 全局索引批量定位：oid → (pack_oid, node_id)
  ② 本地 pack cache（LRU，按 pack_oid 键）命中检查
  ③ 未命中的 pack：从 node_id 拉取
       M1/M2：整 pack 拉取 + 入 cache（简单，冷启动友好）
       M3  ：idx 协商区段拉取（pack 巨大且只要少量对象时）
  ④ 从 pack/idx 中解码所需对象
  ⑤ 在线合成响应 pack（go-git Encoder，无 delta，正确性优先）
  ⑥ side-band-64k 流式回写
```

缓存三层同构：同一套「定位 → 拉取 → 编码」逻辑运行在 banyand 进程内 cache、独立的 cache 层、存储节点上——pack 不可变 ⇒ 缓存键即 pack_oid ⇒ 无失效问题。

## 4. 写路径（服务一次 push）

```
receive-pack 收到 (ref 命令行, pack 流)
  ① 流式落盘临时文件 + 校验 pack trailer
  ② 解包索引：解码对象 OID 清单（不展开对象内容）
  ③ 校验：可达性（新 head 必须包含旧 head 或显式 force）、锚点合法性
  ④ pack 原样写入分片（rendezvous × R 副本）          ← 不解散！
  ⑤ 元数据事务（WAL 单写者）：
       ObjectIndex 批量 upsert（oids → 新 pack）
       Timelines head CAS 前进
       （锚定 /src/foo 等目录锚点时：追加组装任务）
  ⑥ 组装执行（M1 同步，见 01 §4.2）：
       读 CurrentTree("/") 相关 tree → ReplaceSubtree → synthetic commit
       → synthetic commit 对象入 CAS（小 pack）→ 根 head CAS
  ⑦ report-status 回写客户端
```

步骤 ④ 与 ⑥ 的对象写入互相独立、可并行；串行点只有 ⑤⑥ 的 head CAS。

## 5. 后台 repack（碎片合并与 delta 化）

push pack 通常小而碎（一次 push 一个 pack），长期累积会拖垮读路径（单对象读取最多触达 N 个 pack）。后台 repack：

```
触发：碎片率 = packs(repo) > 阈值（默认 64）或对象冗余 > 阈值
计划：RepackPlan 圈定一批小 pack → 按时间/路径局部性排序
执行：
  ① 目标对象集 = 圈定 pack 的并集（排除已被更新 pack 收编的）
  ② delta 化：滑动窗口 + 路径局部性（同目录 blob 优先做 delta base）
  ③ 写出新 pack + idx（origin=repack）
  ④ 元数据版本化切换：ObjectIndex 批量改指向新 pack（单事务）
  ⑤ 旧 pack 引用计数（在途读 + 在途 cache）归零后删除
```

与 git `pack-objects` 的差异：**不追求全局最优 pack**，追求"合并碎片 + 局部性 delta"的成本收益；大 pack 的进一步优化交给 M3 的分层策略（热路径 pack 直传：客户端要的对象恰好整 pack 命中时，直接把存储侧 pack 作为响应体回传，零重编码）。

## 6. 生产选型（设计层面，M2+）

| 组件 | 选型 | 说明 |
|---|---|---|
| 元数据服务 | 分布式 SQL（TiDB / CockroachDB）或 MySQL + 单写者 | WAL 与 CAS 需要线性化 |
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
