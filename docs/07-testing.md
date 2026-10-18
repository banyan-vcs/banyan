# 07 · 集成测试

六轮手工测试（MVP → 统一 DAG → 轮询协商 → 交叉压测 → 多人协作）的
全部验证点已固化为**可自动回归的集成测试**（`integration/integration_test.go`）。

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
| T10 | `TestMultiUserSameFileCollaboration` | **视图 tip 链式化**：同文件三视图五轮串行协作零冲突；blob hash 一致；提交三视图同 hash |
| T11 | `TestFilteredLogRPC` | /api/v1/log 路径过滤（X 视角归一：树不含路径首段⇒整树即 X 视角） |

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
