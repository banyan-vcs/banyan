# Merge Requests 与分支保护

轻量 merge request（MR）：open → approve → 服务端合并，加一档新的分支保护级别
**mr-only**——受保护分支的唯一进入路径是 MR 合并。归属 **gateway 面**（`/api/v1`，
物理 repo 名）：SPA 经 web 层通用代理直接可见，console 拓扑经 `repos/{g}/{p}/… →
repos/{uid}/…` 重写自动可达；merge 执行与保护执法都在 gateway 进程内，规则读 meta
本面，无快照同步。

## 1. 状态机与并发

```
open ──merge──► merged（终态，记录 merge_commit 与 merged_by）
 │ ▲
 │ └── reopen（closed → open；IID 永不复用）
 └──close──► closed
```

- **IID**：每仓单调分配、永不复用——merged/closed 的 MR 链接永久有效。
- **合并采用预留-落地-定稿序**：`TransitionMR(open→merged)` 是排他预留（两个并发
  合并只有一个能过），任何失败（冲突/目标移动/审批不足）回滚回 open，台账永不谎称
  未落地的合并。落地 CAS 以目标 raw 头为期望，`ErrNonFastForward` = 目标已移动（可
  重试）。
- **审批**：`mr_approvals(repo, iid, username)`；**作者不可自批**（ApproveMR 直接拒
  绝）；审批不因 source 分支后续推进而重置（诚实记录——合并取当时 tip，详情页的
  mergeable 是活算的）。

## 2. 服务端合并（gitaly applyMerge 的 service 化）

raw 时间线头为双亲与 CAS 基（客户端期待是 advertised 值；合并与 CAS 用 raw）；merge
base = 双向父链拓扑游走的**首个公共祖先**（单基近似，5 万提交帽——merge.Trees 只吃
一个 base，递归虚拟基不在范围内，无 rename 检测）；`merge.Trees` 三方合并（复用
service/sync.go 的 store 适配器）；双亲 `AnchoredCommit`（author=MR 作者、
committer=合并人）；`FlushSynthBatch` 持久化门 → 目标时间线 CAS。

**织入事实（e2e 钉死）**：root 织入视图把多亲提交**重线性化**为折叠合成——raw 双亲
合并提交留在时间线与对象库，但 clone 解析到的是 advertised 折叠。因此 **MR 记录
advertised OID 为 merge_commit**（可分享链接必须在 fresh clone 里可解析）。这与子树
收养（AdoptSubtreePush）走同一织入机制；`--merges` 结构对 clone 不可见是已知边界，
织入引擎若将来支持多亲直通，此处只改记录值。

## 3. 分支保护三档

`branch_rules(repo, branch, level, approvals)`，admin 读写
（`GET/PUT/DELETE /api/v1/repos/{repo}/branch-rules[/{branch}]`——branch 段
可含 `/`，路径尾段整体即分支名，如 `release/v2`；匹配按分支全名精确）：

| level | 直推 | MR 合并 |
|---|---|---|
| （无规则） | 按策略（developer+；快照 protected 则 owner+） | 分支感知 CanPush（同直推语义） |
| `protected` | 快照语义：owner+（规则面列出以便一屏可读） | owner+ |
| `mr-only` | **任何人拒——含 admin**（git/receive-pack、ref CAS、commit API 三执法点） | developer+，且 approvals ≥ N（默认 1）；**mr-only 在合并路径上超越快照保护**（否则默认 protected ["main"] 下形同虚设） |

## 4. API 与 URL

- `GET|POST /api/v1/repos/{repo}/merge-requests[?state=]`；`GET …/{iid}`（活算
  mergeable/conflicts/merge_base/approvals/rule）；`POST|DELETE …/{iid}/approve`；
  `POST …/{iid}/merge`（409：CONFLICT_MR——冲突路径/目标移动/审批不足/状态）；
  `POST …/{iid}/close|reopen`。
- **Web URL**：`/r/{repo}/-/merge_requests/{iid}`——`/-/` 是项目保留段约定（gitlab 档
  SPA 挂在 `/-/banyan`）；子视图走查询参数（`?tab=overview|diff`、
  `?view=split|unified`，view 另存 localStorage、URL 优先）——粘贴链接落在分享时的
  视图；未知 IID 页内 not found。history 的 ref 同轮改为 `?ref=`（`:ref` 路径参数永
  远匹配不到带 `/` 的分支名，旧链接 301 到查询参数形态）。
- **CLI**：`banyan mr ls|open|show|approve|merge|close|reopen`（工作区 repo）。

## 5. 展示组件

- **diff review**：`@git-diff-view/react`（GitHub 风格 split/unified、lowlight/
  highlight.js 语法高亮——与 Blob 渲染同引擎、无 WASM、全量打包可离线；Shiki 引擎可
  后续平替）。适配层把 /api/v1/diff 的 unified 文本逐文件转 hunk 块 + 语言 id，
  vitest 钉死；diff 引擎 chunk（~1.1MB）懒加载在 changes 页签后。
- **代码展示/在线编辑**：Monaco（OpenSumi 的编辑器内核）直接 ESM + Vite 打包 worker
  （无 loader CDN）；只注册 Monarch 基础语言（无 TS 语言服务）；编辑提交走薄 CLI 同
  款服务端装配（objects/content + commit），保护围栏原样生效；Monaco chunk 懒加载在
  editor view/edit 之后。**MR 页内联编辑**：changes 视图每个 source 侧文件带 edit
  入口（删除/二进制不参与），编辑器打开 source tip 的 blob、提交 CAS 该 tip——落地
  后 diff/mergeability/审批即时活算重取（页内解决冲突即此路径）；OpenSumi 完整
  workbench 经再评估仍不引入：lite 纯前端面不含 SCM/diff 视图（本功能恰是其缺项），
  且 workbench+扩展机器与 web 层零 Node 运行时冲突，Monaco 已是其编辑器内核。

## 6. 边界与诚实清单（v1）

- 仅 root-anchor 分支（与 `UpdateRef` 同边界）；子树 MR 记 follow-up。
- 审批不因 source 前进重置；合并取当时 source tip。
- 无 labels/reviewers/评论/CI 状态。
- 合并记录 advertised OID（§2 织入事实）。
- mr-only 规则不参与快照 v2——执法点即数据点，都在 gateway。
- 预留-定稿间进程崩溃会留下 state=merged 而 merge_commit 为空的 MR（诚实窗口，
  文档记录；恢复=手工 reopen）。
