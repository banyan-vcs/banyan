# 18 · Gitaly 集成方案（研究稿）

> 状态：**实施中——首切片已落地**（协议 v19.5.2 vendored、鉴权/@hashed
> 寻址/Server+Repo+Ref+Commit+Blob 读面 32/163 RPC，见
> [19-gitaly-compat](19-gitaly-compat.md) 实现账本与覆盖矩阵）。本文保留
> 方案与研究结论：banyan 以什么形态接入 GitLab 生态、分几步、每步做什么
> 不做什么、风险在哪、工程量多大。立场沿用既定决策（docs/05 服务层）：
> **分阶段可选兼容，契约漂移隔离在适配层；praefect 不做**。

## 0. 结论摘要

- **形态**：banyan 以**单个 gitaly 节点**身份注册进 GitLab（`gitlab.rb` 的
  `gitaly[:storages]` 一条：名字 → banyand 的 gRPC 地址 + token）。不实现
  praefect/RefTransaction——GitLab 侧的路由与事务层是它自己的 HA 答案；
  banyan 的 HA 在元数据面（MySQL/PG，docs/10 §4）与字节面（散布多副本）
  另有原生答案，两套叠加没有意义。
- **分期**：P1a 只读接入（网页浏览 + clone/pull，零 Rails 回调依赖）→
  P1b 完整 git 传输（push 也可达，需 internal API 回调最小实现）→ P2 网页
  写与 SSH（OperationService + HookService 全量）→ P3 远端/备份/清理映射。
- **巧合同构**：GitLab 的仓库寻址就是 `@hashed/xx/yy/<sha256>.git`（按仓
  库 id 哈希的相对路径）——与 banyan 的 hashed 寻址（docs/03 §6）天然同构，
  relative_path 可直接充当 banyan 仓库名或 storage_id，无翻译层。
- **依赖**：grpc-go + protobuf——这正是当年 gRPC 评估里写定的触发条件
  （"启动 Gitaly P1 反正引 grpc，内外共享基础设施"），NodeClient 的 gRPC
  化与本项同批。

## 1. 集成对象速写（Gitaly 是什么）

- **架构**：gitlab-rails ←gRPC→ gitaly 节点（中间可选 praefect）。git 数据
  = 节点磁盘上的 bare 仓；gitaly 的本质是"把 git 操作变成 gRPC"。
- **寻址**：Storage 级配置（名称 → address + token）；每个 RPC 请求携带
  `Repository{storage_name, relative_path}`。
- **鉴权**：gRPC metadata `gitaly-token`，per-storage 静态令牌。
- **proto 面**（2026-10 调研，[官方协议文档](https://gitlab-org.gitlab.io/gitaly)）：

| 服务族 | 代表 RPC | P1a | P1b | P2+ |
|---|---|---|---|---|
| ServerService | ServerInfo / DiskStatistics / ReadinessCheck | ✅ | ✅ | ✅ |
| RepositoryService（读） | RepositoryExists / GetRepository / HasLocalBranches | ✅ | ✅ | ✅ |
| SmartHTTPService（读侧） | InfoRefsUploadPack / PostUploadPack(WithSidechannel) | ✅ | ✅ | ✅ |
| RefService（读） | ListRefs / FindDefaultBranchName / FindAllBranches | ✅ | ✅ | ✅ |
| CommitService（读） | FindCommit / ListCommits / CommitsBetween / CountCommits / GetCommitMessages | ✅ | ✅ | ✅ |
| BlobService | GetBlob / GetBlobs / ListBlobs | ✅ | ✅ | ✅ |
| DiffService | CommitDiff / RawDiff / FindChangedPaths / DiffStats | ✅ | ✅ | ✅ |
| SmartHTTPService（写侧） | InfoRefsReceivePack / PostReceivePack(WithSidechannel) | — | ✅ | ✅ |
| SSHService | SSHUploadPack* / SSHReceivePack / SSHUploadArchive | — | — | ✅ |
| OperationService（网页写） | UserCommitFiles / UserMergeBranch / UserCherryPick / UserRevert / UserSquash | — | — | ✅ |
| HookService / RefTransaction | Pre/Post/UpdateReceiveHook、ReferenceTransaction | — | 最小 | ✅ |
| RepositoryService（写/维护） | CreateRepository / RemoveRepository / CreateFromBundle / OptimizeRepository | — | 部分 | ✅ |
| RemoteService（mirror） | FindRemoteRepository / UpdateRemoteMirror | — | — | P3 |
| Backup/Cleanup/ObjectPool/Conflicts/PraefectInfo/Partition | 备份、历史重写、对象池、冲突解决 | — | — | 挑做（§7） |

## 2. P1a：只读接入（建议首切片）

GitLab 把 banyan 当作一个存储后端消费：网页浏览、HTTP clone/pull、API 读。
数据进入 banyan 走 `banyan import`（c9a0af1）或 GitLab push 指向 banyan 的
git 端点（banyan 自有 smart HTTP 已可用——不经 Gitaly 面）。

**映射要点**（banyan 能力 → gitaly 语义，全部已有零件）：

- **InfoRefsUploadPack / PostUploadPack** → gitwire v0/v1/v2 广告与 fetch
  全阶梯（exact-pack 直传 → 流式编码器 → 阶梯）；gitaly 的流式响应与
  banyan 的窗口化服务一一对应，内存纪律不受接入层影响。
- **GetBlob/GetBlobs** → `Raw`（idx 单对象解压，O(对象)）；ListBlobs →
  TreeEntries 走查 + 批量取。
- **FindCommit/ListCommits/CommitsBetween** → commit 索引与 Log 走查；
  **GetTreeEntries/目录浏览** → 根视图织入（assembly-on-read）——注意
  gitaly 视角是**整仓**，织入的首访折叠成本由 prewarm 兜底（既有）。
- **DiffService** → P1a 以 `git diff-tree` 语义的树对差（两树走查）实现
  常用子集；FindChangedPaths（跨提交路径历史）挂 tree→commit 反向索引。
- **RepositoryExists/GetRepository** → 元数据面点查；CalculateChecksum
  （GitLab 完整性巡检）P1a 先返回 Unimplemented，观察容忍度（不影响读）。

**验收环境**：GitLab CE（docker `gitlab/gitlab-ce`）+ banyand（新增
`gitaly.listen` 配置节，缺省关）；e2e 场景 = 建仓（import 灌入）→ 网页
文件树/commit/blame 阅读 → HTTP clone → GitLab API 读对象。GitLab 镜像
重，e2e 用例 opt-in（`BANYAN_GITALY_E2E=1`），契约单测（对生成代码的
handler 表）默认跑。

## 3. P1b：完整 git 传输（push 进 GitLab UI 触达）

关键是 **Rails internal API 回调**：gitaly 在 receive 链路里回调 Rails
（`/api/v4/internal/pre_receive`、`post_receive`，secret token 鉴权），
GitLab 据此裁决保护分支/推送规则并更新自身状态（pipeline、webhook、统计）。
适配层的最小实现：

- 适配层内置 HTTP 客户端 POST 两回调（载荷：repo、identifier、gl_id、
  changes 列表）；pre-receive 返回拒绝 → 适配层不落 CAS（与 banyan 自己
  的 hook 机制正交，两者都过才落地）。
- 失败容忍度（回调超时/失败时 push 是否放行）必须 e2e 实测定档，默认
  fail-closed 与 banyan 自有 hook 一致（docs/04 §7）。
- `RepositoryService.CreateRepository` 需要就位（GitLab 建项目即建仓）；
  RemoveRepository → banyan 的删除语义（归档态，M6.3 议题）。

## 4. P2：网页写与 SSH

- **OperationService** 是最大块：UserCommitFiles/UserMergeBranch/
  UserCherryPick/UserRevert/UserSquash 各自是"在服务端执行 git 命令"的
  语义包装。banyan 的映射：CommitService.CreateCommit（changeset 组装）
  + internal/merge（blob/树三方合并）+ 收敛重放——零件都在，语义对齐
  工程量 L。UserMergeBranch 的 quorum 提交/ReferenceTransaction 投票
  **不做**（单节点形态下无意义）。
- **SSHService**：banyan 的 git-over-SSH（docs/04 §7b）平移为 gitaly 的
  SSH 帧格式（pkt 线协议相同，鉴权链换 gl_id）。
- **HookService 全量**（update-hook 逐 ref、reference-transaction）按
  GitLab 实际调用频率补齐。

## 5. 版本矩阵与漂移隔离

- **proto vendor**：锁定一个 GitLab minor（建议当前 CE 稳定版），proto
  文件与生成代码进 `internal/gitaly/`（或独立 module）——适配层是唯一
  import 点，核心代码不接触 gitaly 类型（与 enterprise/ 门控同款纪律）。
- **升级策略**：跟随 GitLab minor 重放 proto + 跑契约测试；RPC 增量按需
  补，删除项在适配层报 Unimplemented 而非编译期扩散。
- **测试金字塔**：handler 表契约测试（默认）→ 录制回放（GitLab 客户端
  流量样本）→ GitLab CE e2e（opt-in）。

## 6. 工程量粗估与排期钩子

| 切片 | 内容 | 量级 |
|---|---|---|
| G0 | grpc 依赖 + gitaly.listen 配置 + token 鉴权 + ServerService + 寻址转换（@hashed 同构） | S |
| P1a | 读面六服务 ~25 RPC + SmartHTTP 读侧 + GitLab CE e2e 环境 | M |
| P1b | PostReceivePack + internal API 回调最小实现 + Create/Remove | M |
| P2 | OperationService 子集 + SSHService + HookService 全量 | L |
| P3 | RemoteService（mirror）、Backup→bundle 映射、Optimize→repack | M |

G0+P1a 一个里程碑；P1b 独立（回调风险单独验证）；P2 按 GitLab 侧需求
拉动。NodeClient gRPC 化与 G0 同批。

## 7. 不做清单（诚实边界）

- **praefect / PraefectInfoService / RefTransaction**：GitLab 侧 HA 路由
  与投票——banyan 集群形态自带元数据面与字节面答案，两层叠加徒增复杂度。
- **ObjectPoolService**：跨仓对象池去重与 banyan 的多 pack/目录时间线
  模型冲突（池=全局对象可见性假设），不做；跨仓去重远期另议。
- **CleanupService.RewriteHistory（BFG）**：历史重写/合规删除属治理
  议题（enterprise 方向），不进开源 P 线。
- **PartitionService / SiteStatistics**：praefect 体系内部，随 praefect
  一并不做。
