# 07 · 集成测试

六轮手工测试（MVP → 统一 DAG → 轮询协商 → 交叉压测 → 多人协作）的
全部验证点已固化为**可自动回归的集成测试**（`integration/`，断言用
testify，文件按测试目标拆分）：

| 文件 | 测试目标 |
|---|---|
| `helpers_test.go` | 共享基建：进程内服务端装配、git CLI 封装、种子/克隆/断言辅助 |
| `transport_basic_test.go` | 基础传输：根/子树克隆往返、引用操作、增量推送拉取（T1–T4） |
| `subtree_collaboration_test.go` | 子树协作语义：线性织入、连续推送、非干扰/路径内互斥、多用户同文件、多作用域稳定性（T5/T7/T8/T10/T18） |
| `push_guards_test.go` | 服务端守卫与协商：v1 have 协商终态 ACK、竞态窗口 sameXView 兜底（T8b/T9） |
| `protocol_compat_test.go` | git 协议兼容矩阵：版本降级、partial 回退、浅化 deepen/unshallow（T13–T16） |
| `persistence_test.go` | SQLite 重启持久化（T17） |
| `rpc_log_test.go` | /api/v1/log 路径过滤（T11） |
| `packing_efficiency_test.go` | 索引与打包效率：have 减除增量包精确构成（T19） |
| `refs_and_repack_test.go` | tags/自定义命名空间端到端 + repack 碎片合并/delta/重启（T20/T21/T21b） |
| `cli_workspace_test.go` | 薄工作区 CLI：最小化 clone/status/commit/pull/log/cat + 冲突恢复 + 子树工作区（T22/T22b） |

## 形态

- **进程内真实服务端**：httptest 起完整装配（MemoryMetaStore +
  LocalShardStore + gitwire + rpc，与 `cmd/banyand` 同构），毫秒级启动；
- **真实 git CLI 作客户端**：外调 `git push/clone/pull/ls-remote`——
  协议级 bug（report-status 顺序、协商死锁、bad pack header、delete-refs
  静默丢命令）只能被真实客户端暴露，mock 无法覆盖；
- **小仓库种子**（三文件单提交，毫秒级 push/clone）；
  go-git 4043 提交级重载验证保留为手工场景（README 快速开始）；
- 统一注入 `core.autocrlf=false`（消除 Windows CRLF 噪音）。

## 用例矩阵（每个用例锁定的历史回归点）

| # | 用例 | 防回归点（历史 bug） |
|---|---|---|
| T1 | `TestRootCloneRoundtrip` | report-status 首行必须是 "unpack ok"；闭包含 tree 边 |
| T2 | `TestSubtreeCloneUnifiedDAG` | 子树工作树=目录内容；原始 hash 进子树历史 |
| T3 | `TestRefOperations` | **delete-refs 能力必须广告**（否则客户端静默丢弃删除命令）；全零 OID 归一 |
| T4 | `TestIncrementalPushPull` | 增量 pack + 第二克隆 pull |
| T5 | `TestSubtreePushAdoption` | 收编 merge 双亲；C 同 hash 进根；文件落位；三视图 blob 一致 |
| T6 | （并入 T5） | fsck 三视图 |
| T7 | `TestConsecutiveSubtreePushes` | **resolveViewTip**：收编后广告 tip=被收编提交（连续 push 不被拒；自身 pull=up-to-date） |
| T8 | `TestStaleCrossViewPushRejected` | **sameXView**：陈旧跨视图 push 拒绝（防静默回滚）；rebase 恢复流 |
| T9 | `TestNegotiationPullStaleClone` | **终态 ACK 语义**：过期克隆 pull 不死锁不坏包 |
| T10 | `TestMultiUserSameFileCollaboration`（**已恢复**，v5 断言：五轮串行协作零拒绝、subject 恰一次、线性、blob 一致） | 同文件三视图五轮串行协作零冲突；根视图线性去重；blob hash 一致 |
| T11 | `TestFilteredLogRPC` | /api/v1/log 路径过滤（X 视角归一：树不含路径首段⇒整树即 X 视角） |
| T8b | `TestRaceWindowStalePushProtection` | rawPush 协议级竞态模拟：force 绕过客户端检查后服务端 sameXView 兜底；X 外变更不阻塞 |
| T12 | `TestMergeModePullWithLocalCommits` | 合并模式 pull（投影链全 X 作用域，merge-base 天然正确） |
| T13 | `TestProtocolVersionMatrix` | 协议版本矩阵：v0 显式 / v2 强制（降级走 v1）clone + 增量 fetch + 子树 URL（docs/03 §9） |
| T14 | `TestPartialCloneFilterFallback` | 未广告 filter → 警告 + 全量回退（对象集=全量 clone）+ promisor 配置下后续 fetch 不破坏 |
| T15 | `TestShallowClone` | **浅化协议**：depth 1/2/超深（空边界=裸 flush）+ 子树投影空间浅化/unshallow（docs/03 §3a 参考流对照） |
| T16 | `TestShallowFetchDeepen` | 浅仓增量：普通 fetch（上报边界裁剪、浅文件不变）/ 加深（边界下移+unshallow 回显）/ unshallow 清除 |
| T17 | `TestSQLiteMetaRestartPersistence` | **M2 持久化**：整进程重启后仓库/pack/对象索引/时间线头全存活；冷启动 nil-index 崩溃回归；重启后可继续 push |
| T18 | `TestSequentialMultiScopeStability` | 顺序多作用域到达：零重复折叠、tip 不漂移、X 外变更不阻塞；含冲突恢复后继续推送（投影守卫 TreeKnown） |
| T19 | `TestIncrementalFetchHaveSubtraction` | **have 减除**（docs/09 §3.1）：增量包=精确 delta（1 提交/2 树/1 blob），未触碰对象不入包；子树 URL 同理；真实客户端端到端 |
| T20 | `TestTagsAndAuxRefs` | **refs 命名空间泛化**：annotated/lightweight tag、tag 对象闭包、自定义 refs/<ns>、tag 删除与非 FF 拒绝、浅克隆 tag 剥离 |
| T21 | `TestRepackFragmentMerge` / `TestRepackRestartSurvival` | **repack**：碎片合并+delta（字节收缩、对象保全、ExactPackHit 恢复）、SQLite 重启后存活（UPSERT 重指向回归） |
| T22 | `TestCLIThinWorkspace` / `TestCLISubtreeWorkspace` | **薄工作区 CLI**（docs/05/10）：最小化 clone（无 .git、无提交闭包）、离线 status、服务端 changeset commit 经**真 git 客户端交叉验证**（fresh clone + fsck + subject）、log/cat 零下载、pull=服务端 tree-diff、base-moved 冲突退出码 1→pull→重提、子树工作区 commit 织入根视图 |

## 运行

```bash
go test ./integration/ -v          # 全量（~24s，需 PATH 中有 git）
go test ./integration/ -run TestMultiUserSameFileCollaboration -v   # 单用例
go test ./...                       # 单元 + 集成
```

无 git CLI 的环境自动跳过（`needGit` → `t.Skip`）。

## 新增功能的测试约定

新能力落地时同步新增集成用例（命名 `Test<场景>`，种子用 `seedRepo`），
并在此表登记防回归点。手工场景（大仓压测/带宽测量）不进 CI，保留在
`F:\Workspace\test\`（README 记录复现命令）。

## 审计补录（2026-10-18）

首轮 11 用例经审计发现缺口并补齐（现 13 用例）：

- **T8b `TestRaceWindowStalePushProtection`**：T8 的拒绝发生在客户端
  （git 自身祖先检查），**服务端 sameXView 防线未被锁定**。且审计实测
  确认关键协议事实：**git push 发送的 old 恒为本次广告的当前值**
  （force 只解除客户端检查、不改 old）——sameXView 的真实防区是
  【竞态窗口】（客户端取完广告后、POST 到达前另一路 push 落地）与
  非规范客户端。用 rawPush（手工 pkt-line 构造 receive-pack POST）
  确定性模拟该窗口：X 内容自分叉 → 拒绝（防静默回滚）；X 内容未变
  而根侧无关变更 → 放行（无关变更不阻塞协作）。
- **T12 `TestMergeModePullWithLocalCommits`**：T10 每轮先 pull 再提交
  （恒 fast-forward），链式 tip 修复真正服务的【合并模式】路径未覆盖。
  带本地提交 pull（同文件不同区域分叉）→ base 必须是 X 作用域旧 tip
  （文件在 base 存在 → modify/modify 干净合并）；若 base 落在全仓
  提交（bfb57fe 修复前缺陷）则 add/add 伪冲突。
- 次要缺口（未单独成用例，已由现有断言或设计意图覆盖）：T5 的收编
  merge 双亲数（消息断言为代理）；T3 的子树视图删除拒绝路径；
  T4 的 pull 输出"或"条件弱断言（由后续 hash 断言兜底）。

## v4 语义适配（2026-10-18，设计 §12）

一致性定义变更后用例矩阵更新：跨视图 SHA 同一不再是断言目标——

| 用例 | v4 断言要点 |
|---|---|
| T2 | 子树 clone **小型化**（对象数 < 全仓）+ blob 一致（原 hash 断言删除） |
| T5 | 父级视图内容到达（blob）；hash 断言仅保留根视图（收编第二父） |
| T8→NonInterference | 根改 X 外 → 陈旧子树 push **直接过**（非干扰）；根改 X 内 → 拒绝 → 真冲突显式解决 |
| T10 | 根视图 subject 恰一次 + 线性零 merge；作者视图保留原 hash；三视图 blob 一致（v5：跨视图 SHA 允许不同） |
| T12 | 合并模式 pull（投影链全 X 作用域，merge-base 天然正确） |

CI 效率基准（go-git 实测）：子树 clone **1547 对象** vs 全仓 30203（19.5×）。

单元侧：`storage.TestGenerationDiamond`（代数精确/污染语义 + 树反向索引，docs/09 §2）。
