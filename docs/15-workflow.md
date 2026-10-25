# 15 · 目录级高并发协作（M4 设计，v2 定稿）

> 2026-10-25 v2，取代同日初稿（refs 票据方案——经 §1 论证弃用）。
> 本稿基于代码实证：守卫语义（adoptSubtreePush/sameXView）、时间线
> CAS 路径、提交合成机械（service.CreateCommit / model.ReplaceSubtree）、
> T18 恢复路径。动机不变：docs/00 问题链最后一环——Agent 分钟级并发。
> 客户端零协议改动。

## 0. 问题的精确形态（代码实证）

- **串行点唯一**：目录时间线 head CAS（`adoptSubtreePack` 尾部）。字节面
  早已并行——pack 摄入是幂等 CAS，瓶颈与正确性都不在对象层；
- **竞争在 CAS 之前就被拒绝**：`sameXView(old, curTip)` 内容守卫对分歧
  链返回 `ErrNonFastForward` → 客户端 pull --rebase --push（T18 路径，
  已验证正确但每次失败=一整个客户端往返：fetch+rebase+re-push 上传）；
- **突发退化链**（B7）：N 并发同目录 → 恰 1 过、N-1 拒 → N-1 个客户端
  各自往返 → 落地窗口内再碰撞 → 重试风暴；
- **根本事实**：目录时间线是**串行日志**。同目录吞吐上界 = 追加成本 ×
  批深度，性能设计只有三条正道——①不拒绝可收敛者（消灭客户端往返）
  ②批追加（K 个 push 摊一次 CAS）③关键区只留"判定+CAS"（重放与织入
  移出关键区）。

## 1. 方案空间与对比（决策记录）

| 方案 | 机制 | 崩溃安全 | 公平 | 无竞争开销 | 吞吐 | 复杂度 | 结论 |
|---|---|---|---|---|---|---|---|
| **A** refs 票据队列（初稿） | refs 发"目录票"，gateway 持票跨服务完成守卫+CAS | 需**跨服务租约**/超时/续期语义 | 强 FIFO | 取票 RPC | 好 | 中（refs API+租约） | **弃**：锁跨服务，refs 从零改动变有状态，违背网关无状态红利 |
| **B** 网关本地批队列 + OCC 收敛环 | 每 gateway 每 timeline 有界 FIFO；drain=批内逐条收敛重放成链，**一次 CAS**；跨网关碰撞=CAS 失败→服务端重放重试（指数退避+抖动） | 队列进程内存、advisory；**CAS 恒为裁判**（崩溃=退化为 OCC） | 网关内 FIFO；跨网关靠退避抖动 | 队列为空即直通，**零开销** | **K push = 1 CAS** | 中 | **采用** |
| C 纯 OCC（无队列） | 每分歧 push 服务端重放+CAS 环 | 最简 | 高竞争下饥饿 | 零 | 无批摊销 | 低 | 保留为 B 的退化路径（队列空时 B≡C） |
| D 异步 change-request 状态机（tekeburak land worker 形态） | push 入库为 CR，后台 land，客户端查询状态 | 需持久状态机+新查询协议 | 好 | — | 最高（可投机流水线） | 高（协议扩展） | **v2 演进**：同步语义先行，D 在其上加流水线 |
| E 客户端引导重试（现状） | 拒绝+客户端 rebase | 已验证 | — | — | 差（每失败一往返） | 0 | 基线 |

选型判据：网关无状态承诺（docs/10 §2）、git 协议零改动、崩溃安全=CAS
兜底、无竞争路径零开销、refs **零改动**。→ **B**。

## 2. 方案 B 设计

### 2.1 入口与批量（gateway receive-pack）

- per-timeline FIFO（key=repo+anchor+branch；容量 64；进程内存）；
  入队前 CanPush/逐 ref 校验已完成，pack 已摄入；
- **drain 即 group commit**：队首醒来时取走**同 key 全部在队条目**，
  按 FIFO 逐条收敛（§2.2），K 条收敛结果首尾相连构成时间线追加段，
  **一次 CAS**（old → 末条新头）。时间线即日志——K 个 push = K 次
  追加，中间无需各自 CAS；
- 每个连接的 report-status 等自己那条落地后返回（批内串行毫秒级）。

### 2.2 收敛重放（merge train 核心，模型层新原语）

记号：客户端链 C=c1..ck（基=其广告的 old），当前组合视图 tip T
（SubtreeHead）。

1. **路径级合并**：Δi = diff(tree(c(i-1)), tree(ci))——变更路径与条目；
   演进树 M₀ = xviewTree(T)；逐条 Mᵢ = applyPaths(M(i-1), Δi)；
2. **冲突判定**：Δi 的路径 ∈ 变更集(base→T) 且内容相异 → **该票
   `ng conflict`**（隔离失败，列车继续下一票）；
3. **提交合成**：cᵢ′ = AnchoredCommit{tree: Mᵢ, parents: [c(i-1)′]}，
   c₁′.parent = 本地时间线当前头——复用 CreateCommit/ReplaceSubtree
   组装机械 + PutObjects（客户端对象已摄入，合成只补 K 个提交对象）；
4. **no-op 互证**：Mᵢ 已 treeKnown → 跳过（防重复折叠，与织入三重
   去重同语义）；
5. **root push 不收敛**：全树重放语义复杂（跨目录文件移动无目录级
   边界），v1 保持拒绝+客户端 rebase——目录才是协作热路径（诚实边界）。

### 2.3 OCC 环（跨网关/批次碰撞）

CAS 失败（他网关先落地）→ 取新 tip 重放（**服务端**重试，零客户端
往返）；上限 8 次 × 指数退避+抖动，超限 `ng busy`。队列本身消除网关
内碰撞，跨网关碰撞率 = 网关数 × 批率，很小。

### 2.4 崩溃与 HA

- 网关崩溃：连接断、客户端 git 原生重试（等价 C 路径）；队列随进程
  消失——**advisory**，不构成正确性依赖；
- refs 重启：CAS 语义不变，兜底如故；
- 合成对象先注册后 CAS：崩溃窗口=无害孤儿（与 push 两步语义一致，
  docs/10 v1 偏差同款）。

### 2.5 性能模型（量化）

| 口径 | 现状（E） | 方案 B |
|---|---|---|
| N 并发客户端往返 | 1 + (N-1)×(fetch+rebase+re-push) | **每人恒 1** |
| 服务端 CAS | N 次竞争（N-1 拒） | ⌈N/批⌉ 次（每网关每批 1 次） |
| 服务端新增工作 | — | N 次树合成（内存树操作）+ K 个小提交对象 |
| warm（织入预热） | N 次 | 每批 1 次 |
| 无竞争路径 | — | **零新增开销**（队列空直通） |
| 跨目录 | 零锁 | 不变（不同 key 不相遇，T44 钉住） |

### 2.6 观测与 SLO

指标：`queue_depth`/`queue_wait_p99`、`batch_size`、
`convergence_rebases`/`convergence_conflicts`、`cas_retries`。
bench 场景 **agent-burst**：8/32/128 并发同目录，单网关与多网关两种
拓扑；SLO：零拒绝（可收敛者全落地）+ p99 wait 界 + 混合目录吞吐不降。

## 3. 实施分片（修订，refs 零改动）

> **落地状态（2026-10-25，db2af27/3c4ea8c）**：M4.1（internal/merge 纯 Go
> 三方合并，git merge-file oracle 逐字节钉住）+ M4.2/M4.3 主体（队列 +
> 收敛 + 冲突隔离 + OCC）已交付，T41/T42 全绿；config
> `workflow.convergence: auto(默认)|reject`。**实证补充的语义事实**：真
> git 客户端对顺序性非 FF push 在发送前就自行拒绝（"fetch first"），
> 到不了服务端——收敛的真实作用面是**并发竞争窗口**（广告已取、他人
> 落地在即），T41 的 8 并发突发正是该形态；顺序分歧语义用 rawPush 直
> 钉 wire。**fast path 回滚守卫**（T43，docs/04 §6a）：force push 的
> old 恒=广告值，回滚/重写形态会被 fast path 误判为连续 push 绕过列车
> ——new 链内容前向校验（Descends/sameXView/TreeKnown 三形态）封死，
> rawPush 伪造 old 形态则由收敛重放天然免疫（三方 base=客户端基底，
> 回滚意图在其 diff 中不可表达）。未完：M4.4（指标 + agent-burst bench
> 场景 + 一次 CAS 批追加优化）、treeKnown 树→提交索引。

```
M4.1  重放原语抽取（diff/applyPaths/合成，纯函数）+ treeKnown 树→提交索引
      （撤 10 万上限）——无行为变化
M4.2  网关批队列 + 一次 CAS 批追加（只串行化，收敛默认关）
M4.3  收敛 merge train（路径级合并 + 冲突隔离 + OCC 环；默认开，repo 级
      convergence: auto|reject 可关）
M4.4  指标 + agent-burst 场景 + SLO 门
```

## 4. 测试计划（docs/13 约定）

| # | 用例 | 防回归点 |
|---|---|---|
| T41 | 8 客户端并发同目录：全部落地、FIFO 有序、零拒绝、一次 CAS（计数断言） | 批队列正确性 |
| T42 | 收敛语义：不同文件自动合并（内容逐文件断言）；同文件冲突 ng 隔离、后续票继续；收敛链可再被客户端 rebase 续推（T18 交叉） | merge train |
| T43 | **已用于 force push 回滚守卫**（docs/04 §6a——fast path 的内容前向校验，回滚形态 force push 拒绝 + 恢复路线 + 根视图单调） | 回滚守卫 |
| T44 | **已用于仓库级 force push 开关**（docs/04 §6a——opt-in 回滚落地 + /.woven 重置重织、祖先视图 commits 重建） | 受控回滚 |
| T45 | 队满 backpressure / 超时 busy / 网关重启（队列消失）后客户端重试仍正确 | 边界与崩溃 |
| T46 | 4 目录 × 8 并发混合：吞吐与单目录对照不降 | 跨目录零干扰 |
| T47 | 双网关拓扑同时批：最终一致、零客户端拒绝（OCC 环） | 跨网关 |

## 5. 边界（诚实清单）

- **无跨目录原子**：多目录 push 对象原子、时间线各自（永久边界）；
- **auto 收敛改写提交 OID**：与投影重写语义一致，repo 级 reject 逃生门；
- **收敛只做路径级合并**：同文件双改=冲突是 v1；v1.1 落地路径已论证
  （merge-ort 调研：xdl_merge 算法结构移植、diff 引擎复用 go-diff、
  git merge-file 为 oracle、重命名类显式冲突化、二进制不合并——
  go-git v5/v6 均无 3-way 实现，必须自研）；
- **root push 不收敛**：拒绝+客户端 rebase（§2.2.5）；
- **跨网关极端持续竞争理论饥饿**：退避+抖动缓解并记录；终解是 D 方案
  （异步 CR 状态机，v2 演进）；
- 同步等待上限 30s：网关/负载均衡超时需匹配（部署注意项）。
