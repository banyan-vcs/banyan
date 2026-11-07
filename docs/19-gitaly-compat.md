# 19 · Gitaly 协议兼容层（实现）

> 目标：以 gitaly 的 gRPC protocol 为契约实现全部接口，使本进程可**替代**
> gitaly 节点接入 GitLab（Rails/workhorse 只改 gitaly.yml 指向）。
> 研究稿（原 18 号文档）已并入本文 §0；本文是形态、覆盖矩阵与实现账本。

## 0. 背景：Gitaly 是什么，为什么能替换（原 18 号研究稿并入）

- **对象速写**：gitlab-rails ←gRPC→ gitaly 节点（中间可选 praefect）；
  git 数据 = 节点上的 bare 仓，gitaly 的本质是"把 git 操作变成 gRPC"。
  寻址是 Storage 级配置（名称 → address + token），每个请求携带
  `Repository{storage_name, relative_path}`；鉴权 = per-storage 静态令牌。
- **接入形态**：banyan 以**单个 gitaly 节点**身份注册进 GitLab
  （`gitlab.rb` 的 `gitaly[:storages]` 一条：名字 → banyand 的 gRPC 地址
  + token），Rails/workhorse 只改 gitaly.yml。GitLab 的 @hashed 寻址与
  banyan hashed 模式天然同构（§2），无翻译层。
- **漂移隔离纪律**：vendored proto 与生成代码只经 `internal/gitaly/`
  适配层 import，核心代码不接触 gitaly 类型（与 enterprise/ 门控同款）。
  升级 = 锁定 GitLab minor 重放 proto + 全量回归；RPC 增量按需补，
  删除项在适配层报 Unimplemented 而非编译期扩散。测试金字塔：handler
  表契约测试（默认）→ 传输模拟器/workhorse 形态对拍（T61/T62）→
  GitLab CE e2e（opt-in）。
- **演进注记**：研究稿曾按 P1a 只读 → P1b 传输 → P2 网页写分期，且
  "不做清单"排除 praefect / ObjectPool / 历史重写。实现超越了研究：
  163/163 全落地，praefect 族以 cluster-of-one 语义映射、对象池以
  alternates 行为等价、CleanupService 以快照重放实现（§4、§6a）——
  分期与不做清单仅存历史意义。

## 1. 形态

- **单 gitaly 节点形态**（praefect / RefTransaction / ObjectPool 集群面不在本层）。
- `internal/gitalypb/`：vendored 生成代码，**协议版本锁定 v19.5.2**
  （gitlab.com/gitlab-org/gitaly 上游 tag 验证为最新稳定版；MIT，附
  `LICENSE.gitaly`）。升级 = 整目录替换 + 全量回归。
- `internal/gitaly/`：兼容层。同一对象平面（MetaStore + ObjectService +
  service 视图），与 gitwire / JSON API 并列的第三个前端。
- 依赖：`google.golang.org/grpc v1.85.0`、`protobuf v1.36.12`（首批外部
  gRPC 依赖）。
- 生命周期：SIGTERM 时 gRPC 面 GracefulStop（宽限后降级 Stop），在途
  RPC 流跑完——与全进程 drain 同一条终止上下文（docs/14 生产主轴）。

## 2. 寻址：relative_path → banyan 仓库名

banyan 仓名不能含 `/`（`/r/<repo>/tree/` 的 URL 语法保留该字符），而
GitLab relative_path 是带斜杠的 `@hashed/<h2>/<h2>/<sha256(project id)>.git`。
映射（`gitaly.MapRelativePath`，单点）：

| relative_path | banyan 仓库名 |
|---|---|
| `@hashed/xx/yy/<64hex>.git` | `<64hex>`（GitLab 全局唯一，零歧义） |
| `@pools/xx/yy/<64hex>.git` | `pools-<64hex>`（ObjectPool 切片预留） |
| 其他（legacy 命名存储） | 去 `.git` 后可逆 tilde 转义（`~0`=~，`~1`=/） |

hashed 寻址模式下 banyan 由 storage_id 派生不透明目录键——与上游同构，
rename 永不搬数据。

## 3. 信任模型与配置

- 鉴权 = 共享 Bearer token（`authorization: Bearer <token>` 元数据）。
  两种受礼形态（§8 实测后补齐）：**v2 HMAC 签名**——Rails GitalyClient
  16.x 起每次调用现签 `v2.<HMAC-SHA256(secret, issued_at)>.<unix秒>`，
  ±30s 窗口（上游 auth/token.go）；以及裸 token 恒时比较（旧客户端与
  banyan 自家 Dial）。与上游一致：**token 之后全信**，细粒度授权在
  前端（Rails/console），不走 banyan auth 面（docs/07）。
- ServerSignature：进程内 ed25519（重启即换钥，Rails 会重取）。
- ServerInfo 上报：`server_version` = vendored 协议版本（19.5.2，可
  `reported_version` 覆盖）；`git_version` = 纯 Go 协议面的特性基线
  （2.45.2，常量声明）。
- 配置节（缺省整层关闭，仅 all-in-one/gateway）：

```yaml
gitaly:
  listen: "unix:/var/lib/banyan/gitaly.socket"  # 或 tcp
  token: "change-me"
  storage_name: "default"   # 须与 GitLab gitaly.yml 一致
```

## 4. 覆盖矩阵（163 RPC / 20 服务 — 全部落地 ✅）

图例：✅ 已实现并测试（163/163；服务态种类见 §6a——单节点诚实语义
映射取代了此前的定稿 Unimplemented）

### ServerService 4/4 ✅
ServerInfo ✅ · ReadinessCheck ✅ · ServerSignature ✅ · DiskStatistics ✅（字节数留待卷探测）

### RepositoryService 36/42
RepositoryExists ✅ · CreateRepository ✅（default_branch 落 repos 表，
SHA1；ObjectFormat 校验） · ObjectFormat ✅ · HasLocalBranches ✅ ·
RepositorySize/GetObjectDirectorySize ✅（= /repos/<repo>/size 同源） ·
WriteRef ✅ · FindMergeBase ✅（两 rev 单基形式：A 祖先集 + B 时间序走查
首命中） · **RemoveRepository ✅**（存储层删除路径：meta 六表事务删行 +
shard 目录回收，字节先行注册后删，幂等；hashed 寻址键不可变——旧键留
待 fleet GC） · **CalculateChecksum ✅**（上游同构：show-ref --head 行集
逐行 SHA-1 后 XOR，顺序无关；空仓=40×0） · **Fsck ✅**（全 ref 可达闭包
走查：commit 可解析、树可读、blob 可取，问题行进 response.Error——连通
性子集，无 dangling 面） · **GetArchive ✅**（tar 与 `git archive`
**逐字节相等**——pax_global_header comment、uname/gname=root、
tar_umask=002 的 mode、10240B 记录；tar.gz=gzip -n 包同一 payload；zip
条目级等价；path/elide_path/exclude + ArchivePathNotFound/
EmptyRootTree/ExcludePathNotFound 结构化 detail） · **RepositoryInfo ✅**
（单一对象平面映射：Size/LooseCount/pack 数；子集） · **ObjectsSize ✅**
（bidi；rev-list --objects 闭包求和——报告**未压缩**对象字节） ·
**GetRawChanges ✅**（git diff --raw 首父形态；A/D/M、blob OID/size） ·
**GetInfoAttributes ✅**（tip 树 .gitattributes；无则空流） ·
**GetConfig ✅**（恒空流——无 git config 面） · **OptimizeRepository ✅** /
**PruneUnreachableObjects ✅**（对象平面恒 compact——诚实空操作；
offloading 对与上游同形 Unimplemented） · **SearchFilesByName ✅**（pathspec glob+filter 正则+offset/limit） · **SearchFilesByContent ✅**（git-grep 形态块：NUL 分隔路径/: 与 - 的匹配上下文标记/±2 行/相邻融合/二进制跳过/不区分大小写） · **GetFileAttributes ✅**（根 .gitattributes：后行胜出/basename vs 路径模式/set-unset-value-unspecified；子集=仅根文件、负模式跳过） · **FindLicense ✅**（根 LICENSE*/COPYING* 候选 × 手工特征指纹=SPDX 库子集） · **CreateBundle ✅**（v2 头含尾部 HEAD 行——git bundle create --all 同形，clone 靠它猜默认分支；pack=闭包流式编码） · **CreateBundleFromRefList ✅**（bidi patterns glob 过滤） · **FetchBundle ✅**（头解析/prerequisite 校验/pack spool+整包摄入/refs force-set；流为全内存子集） · **CreateRepositoryFromBundle ✅**（建仓+种子+HEAD→默认分支+失败回滚） · **GetCustomHooks ✅ / SetCustomHooks ✅ / RestoreCustomHooks ✅**（tarball 经 meta RepoBlobStore 逐字往返——可执行钩子面仍是 HTTP 回调注册表，tarball 保备份移植性；无存档=空流） · **FetchSourceBranch ✅**（同对象平面仓对仓：源闭包逐对象拷贝+目标 ref CAS，expected-old 语义；新 ref 原样 OID） · **FetchRemote ✅**（go-git smart-HTTP 客户端——既有依赖零新增；一次 upload-pack 换包/整包摄入/refs force-set；Authorization 头原样注入；ssh 远端 Unimplemented） · **CreateRepositoryFromURL ✅**（建仓+拉取+失败回滚） · **CreateFork ✅**（全 ref 闭包拷贝；annotated tag 先落 tag 对象再 peel 走闭包） · **FastExport ✅**（fast-export 流子集：reset/commit/mark/data/M；blob 先于所属 commit（fast-import 拒绝交错）；rename/copy/tag/progress 不出——git fast-import 可完整往返，T70 实证） · **BackupCustomHooks ✅**（GetCustomHooks 的备份管道同体） · ⬜ 6 定稿（见 §6a：GetSnapshot/CreateRepositoryFromSnapshot/ReplicateRepository/GenerateBundleURI/BackupRepository/RestoreRepository）

### RefService 16/16 ✅
FindDefaultBranchName ✅ · FindBranch ✅ · RefExists ✅ · FindAllBranches ✅
（含 merged_only） · FindAllTags ✅ · FindTag ✅（含 annotated/lightweight
与 peel 语义） · FindAllRemoteBranches ✅（空集=诚实答案，banyan 无
remote-tracking refs） · ListRefs ✅（patterns glob/head/peel/pointing_at/
分页） · FindRefsByOID ✅（含 peel 匹配） · ListBranchNamesContainingCommit ✅
· ListTagNamesContainingCommit ✅ · UpdateReferences ✅（客户端流式批写；
Old OID 为咨询性——banyan ref 面写者串行化，竞争按最后写赢） · DeleteRefs ✅
· **FindLocalBranches ✅**（分页/exclude glob） · **GetTagSignatures ✅ / GetTagMessages ✅**（annotated tag 正文手解析；签名=尾部 PGP armor 块剥离）

### CommitService 22/22 ✅
FindCommit ✅（rev 缺失返回空 commit，同上游契约） · CommitIsAncestor ✅ ·
ListCommits ✅（^排除/first-parent/max_parents/after/before/author/分页） ·
ListAllCommits ✅ · CountCommits ✅ · GetTreeEntries ✅（recursive） ·
ListFiles ✅ · ListCommitsByOid ✅ · GetCommitMessages ✅ ·
CheckObjectsExist ✅（bidi） · **FindCommits ✅**（pathspec 过滤历史=pathTouched 对首父树比较） · **CommitStats ✅** · **TreeEntry ✅**（单条目+内容分块） · **CommitsByMessage ✅**（消息子串） · **LastCommitForPath ✅** · **ListLastCommitsForTree ✅**（树首层逐条目） · **CountDivergingCommits ✅**（对称差集双侧计数） · **CommitLanguages ✅**（扩展名+shebang 子集 + GitLab 徽章色） · **RawBlame ✅**（首父链行级归属：每步 diff 的 insert 行归子提交、幸存行继续上溯、root 收尾；porcelain 形态含 -L range） · **ListCommitsByRefName ✅**（批量 ref→tip） · **GetCommitSignatures ✅ / FilterShasWithSignatures ✅**（gpgsig 头剥离 + signed_text=去签名头的验签原文）

### BlobService 7/7 ✅
GetBlob ✅（oid+limit） · GetBlobs ✅（revision:path 批量，miss 答空消息） ·
ListBlobs ✅（**全历史**走查 rev-list --objects 语义：树 memo + blob OID
去重，tip 已删除文件仍应答；bytes_limit 内带内容） · ListAllBlobs ✅
（**全对象库**枚举 batch-all-objects 语义：ListPacks∪PackObjects，OID
序） · GetLFSPointers ✅（200B 门 + 上游同构正则解析；非 pointer 对象
静默过滤，缺对象报错） · ListLFSPointers ✅（历史走查 + limit） ·
ListAllLFSPointers ✅（全对象库）

### SmartHTTPService 5/5 ✅
InfoRefsUploadPack ✅ · InfoRefsReceivePack ✅ · PostReceivePack ✅
（bidi：body 流入 mux 桥、响应流回；工作方式 = 内建 gitwire mux +
recorder，一次 HTTP 交换一条 gRPC 消息流，stateless-rpc 契约） ·
**PostUploadPackWithSidechannel ✅（fetch 通路解锁）**——sidechannel 传输
（internal/gitaly/muxcreds.go + sidechannel.go）：连接首字节 "backchannel"
魔数 → yamux 会话（gRPC 走流 1）→ 服务端反向开流，魔数 "sidechannel"+BE
int64 id+"ok" 确认交换；**数据相位非对称帧**（客户端→服务端 pkt-line 分帧
≤65531B/帧、裸 "0000" 帧模拟半关闭；服务端→客户端裸字节）。同一监听器经
TransportCredentials 嗅探兼容普通 gRPC 客户端（listenmux 同构）。 ·
**AdvertiseBundleURI ✅**（无 bundle CDN 配置=空广告——上游未配置同形，客户端回落普通 fetch）

### SSHService 3/3 ✅
SSHReceivePack ✅（bidi：stdin 泵入会话、stdout 分块流回、exit-status
末条；复用 gitwire.ServeSSHSession——从 serveGitExec 抽出的 io 流会话核
心，docs/04 §7b 的无守护进程形态） · **SSHUploadPackWithSidechannel ✅**
（交互式 upload 会话整体跑在 sidechannel 上——广告/协商/包字节） ·
**SSHUploadArchive ✅**（git upload-archive wire 协议：argument pkt-line
流→ACK+flush→sideband 1 归档字节/3 错误→终 flush；载荷与 `git archive`
逐字节一致，T76；参数错=NACK 形态）

### DiffService 10/10 ✅
CommitDiff ✅ · CommitDelta ✅ · RawDiff ✅（**与 git diff 逐字节一致**，T63） · RawPatch ✅（同 RawDiff 体） · DiffStats ✅ · DiffBlobs ✅（字节对直评） · GetPatchID ✅（SHA-1 over patch，best-effort） · FindChangedPaths ✅（Tree/Commit 双形态） · **RangeDiff ✅**（两组范围按 patch-id 配对：相等=EQUAL、未配对邻位=NOT_EQUAL、单侧剩余=增删；RangePair/三点 RevisionRange（merge-base 展开）/BaseWithRevisions 三形态） · **RawRangeDiff ✅**（git range-diff 摘要行文本形态；代价型模糊配对与对间 diff 块为子集）。底层=treeDiff（树递归对比）+unifiedPatch（go-diff 行级 + git 格式头：diff --git/index 行/@@ 计数=1 省略/dev-null 语义）。

### OperationService 16/16 ✅
UserCreateBranch ✅（start point 解析+peel；已存在=FailedPrecondition） ·
UserDeleteBranch ✅（ExpectedOldOid CAS） · UserUpdateBranch ✅ ·
UserCreateTag ✅（annotated=自建 tag 对象经 PutObjects 落盘+peel 校验；
lightweight 直指 commit；已存在=AlreadyExists） · UserDeleteTag ✅ ·
UserFFBranch ✅（isAncestor 前置检查，非 FF=FailedPrecondition "not fast
forward"） · UserCommitFiles ✅（bidi：header+actions 流 → PutObjects +
service.CreateCommit 同一发布路径；CREATE/UPDATE/DELETE/MOVE（InferContent
内容拷贝）/CHMOD/CREATE_DIR(.gitkeep)；author=header、committer=user、
同一时间戳；start_branch 隐式建支） · **UserMergeBranch ✅**（两阶段 bidi：
apply 算合并提交答 OID → 客户端确认 CAS 上分支；冲突=FailedPrecondition+
UserMergeBranchError{MergeConflict} 结构化 detail；中止="merge aborted by
client"；CAS 竞态="unexpected old value"+ReferenceUpdateError detail） ·
**UserMergeToRef ✅**（refs/merge-requests/* 目标 ref 原样广告两父合并提交
——merge train 形态） · **UserSquash ✅**（start..end 折叠单亲提交，
不写 ref，答 SquashSha；ResolveRevision/RebaseConflict detail） ·
**UserCherryPick ✅ / UserRevert ✅**（单提交 3-way：pick 以被拾提交的
父为基、revert 反相（父为 theirs、被撤提交为基，根提交对空树）；
空结果=ChangesAlreadyApplied、分叉=TargetBranchDiverged/NotAncestor
detail；DryRun 不动 ref；pick 保留源作者、revert 以操作者为作者） ·
**UserRebaseToRef ✅**（SourceSha 重放到 FirstParentRef 上、落
refs/merge-requests/* 目标；冲突=FailedPrecondition） ·
**UserRebaseConfirmable ✅**（两阶段 bidi：header（BranchSha 被重放支+
RemoteBranch 落点，同仓限定）→ RebaseSha → apply 确认 CAS 上分支 /
中止；冲突 detail 同形） · **UserApplyPatch ✅**（bidi：header 后为
mbox 字节流；**纯 Go mbox 解析 + 统一 diff 应用器**（patchapply.go：
mails From-行切分/RFC-822 头/[PATCH] 前缀剥除；hunk 上下文匹配+前后
偏移搜索；二进制与 rename/copy 显式拒绝）；逐 mail 一提交（作者来自
邮件头、消息=subject+body）；上下文不匹配="Patch failed at %04d"
FailedPrecondition（git am 冲突同形）） · **UserUpdateSubmodule ✅**
（gitlink 改写走 CreateCommit（service 层 160000 免 blob 校验）；
already-at / 非子模块路径=InvalidArgument+PathError detail）

**rebase 引擎**（operations_rebase.go）：todo 列表=branch-only 提交
（可达闭包差集）、丢 merge、**patch 等价剔除**（--cherry-pick 语义：
对 treeDiff 变更集 (path,OID 对,mode) 做 SHA-1 签名，与 upstream-only
签名集比对——内容级等价的诚实近似）、最老优先重放；逐提交 3-way
（基=该提交首父）；变空丢弃/原本空保留（--empty=drop）；作者与消息
保留、committer=操作者。**根分支非快进 CAS 触发 ResetWoven**（casRef
内置：rebase 换链/落在广告孪生上的提交使织入 prevWoven 追加锚失效——
复用 force-push 的视图重建语义，docs/04 §6a）。

**合并引擎**：internal/merge 树条目 3-way + 内容 diff3（git merge-file
oracle 既测）经合成对象层驱动；applyMerge 单批（BeginSynthBatch→
FlushSynthBatch 持久化点在任何 ref CAS 前）；冲突标签 ours=HEAD/
theirs=短 OID；重命名不检测（git 关闭检测时的诚实子集）。

**双视图规则（gitaly 面统一约定）**：读与期望值说**广告视图 OID**
（织入视图——所有读面/广告的形态）；写翻译为**原始 timeline CAS**。
线性历史上两者相等；合并提交在广告侧被织入引擎线性化为**树全同的
孪生提交**（banyan 根视图线性模型）。期望值（expected_old_oid 等）
对广告 tip 或 raw head 任一匹配即过，CAS 恒对 raw head 执行；
cherry-pick/revert/commit 落在广告 tip 上则两视图收敛回同一 OID。

### ConflictsService 2/2 ✅
**ListConflictFiles ✅**（3-way 引擎冲突路径集 + 每路径三侧 blob 渲染 git 冲突标记体；SkipContent 只发头） · **ResolveConflicts ✅**（自动合并树 + 用户解覆写（content 逐字/sections 按标记块选 head|origin）+ 两亲 merge commit 落 target 分支；未解决路径答 ResolutionError 不 fail RPC）

### RemoteService 3/3 ✅
FindRemoteRepository ✅（广告非空=存在） · FindRemoteRootRef ✅（symref 能力+唯一 head 回退） · **UpdateRemoteMirror ✅**（go-git receive-pack 客户端：refspec 匹配分支、tip 闭包单包自含、分歧 force 覆盖或 keep_divergent_refs 跳过；T70 推 banyan 自身 smart-HTTP 面）

### SSHService 3/3 ✅
（见上文同节）SSHUploadArchive 的 wire 详注亦见上文。

### 其余 17 个服务（11 全勾，见上文各行）
**RepositoryService 42/42 ✅**（切片十一补齐：GetSnapshot/CreateRepositoryFromSnapshot/ReplicateRepository/BackupRepository/RestoreRepository/GenerateBundleURI——语义映射见 §6a）· **HookService 7/7 ✅** · **CleanupService 2/2 ✅** · **ObjectPoolService 6/6 ✅** · **BackupService 2/2 ✅** · **PartitionService 2/2 ✅** · **PraefectInfoService 6/6 ✅** · **InternalGitaly 5/5 ✅** · **AnalysisService 1/1 ✅** · **RefTransaction 2/2 ✅** · **ConflictsService 2/2 ✅**

**总账：163/163 全部落地。** vendored 协议 v19.5.2 的 20 服务 163 个
RPC（ServiceDesc Methods + Streams 合计口径）全部有实现；切片十一把
此前的 33 个定稿 Unimplemented + Repository 尾 6 + SSH 尾 1 全部按
§6a 的单节点诚实语义实现并配 T71–T76 集成面。

## 5. 切片路线（续）

1. ~~LFS pointer 面~~ ✅（切片五，随 BlobService 全绿）。
2. ~~合并三件套 + cherry-pick/revert~~ ✅（切片六）。
3. ~~rebase 系 + ApplyPatch + UpdateSubmodule~~ ✅（切片七——
   **OperationService 16/16 收满**）。
4. ~~Repo 维护面~~ ✅（切片八——删仓/校验和/fsck/archive/raw-changes/
   housekeeping，RepositoryService 19/42）。
5. ~~Import/Remote 面~~ ✅（切片九——检索/bundle/hooks/远端 14 RPC，
   RepositoryService 33/42；FastExport 留下片）。
6. ~~读面/Repo 尾/Conflicts/Remote 收满~~ ✅（切片十，123/156+33 定稿）。
7. ~~收尾 40 面：praefect 族/backup/快照/池/hook/清理/分析/archive-wire~~
   ✅（切片十一——**163/163 全落地**；33 个定稿 Unimplemented 改为
   §6a 的诚实语义实现）。

## 6. 已知语义差异（诚实清单）

- UpdateReferences 的 Old OID 不做强 CAS（banyan ref 面写者串行化；
  语义=最后写赢，见 docs/04 §6a 的 rollback guard）。
- DiskStatistics 字节数暂为 0（卷探测接运维面后补）。
- `git_version` 为常量声明（纯 Go 实现无 git 二进制可询）。
- CountCommits 的 path 过滤未实现（history simplification 需树 diff，
  后续接 view 引擎；当前按全历史计数 + 时间窗）。
- **hooks 是空操作**：OperationService 的 user 系 RPC 不执行 pre/post
  receive hooks（`PreReceiveError` 恒空）——banyan 的钩子面在网关
  （docs/04 §7），gitaly 面继承上游"token 后全信"模型，授权与钩子
  归前端。
- UserCommitFiles 的 `start_repository`（跨仓库 fork 编辑）不支持
  （InvalidArgument）；同仓 `start_branch_name` 完整支持（含隐式建支）。
- UserCommitFiles 空仓+零动作（空树提交）拒绝（InvalidArgument）；上游
  会写出空树提交——GitLab 不发送该形态。
- GetLFSPointers 缺失对象 = NotFound（上游经 cat-file 管道为 Internal
  系错误）；UserFFBranch 目标 commit 缺失 = NotFound（上游 Internal）。
  均为更精确的错误码偏移，消息可辨。
- UserUpdateBranch 的 newrev/oldrev 非全 hex 时按上游 Internal 报错。
- **合并提交的线性化孪生（双视图）**：UserMergeBranch 落地的两父合并
  提交在广告侧被织入引擎线性化为树全同的孪生提交（根视图线性模型，
  docs/02 §13）。BranchUpdate 答原始合并 OID（FindCommit 可读、MR
  merge_commit_sha 链接有效）；分支 tip 读取显示孪生（fetch 主分支
  看到线性历史）。**按合并 OID 的协议级 fetch** 不可用（孪生不可达
  该 OID）——GetBlob/FindCommit 等 RPC 直读不受影响。UserMergeToRef
  目标 ref 原样广告（aux ref 不织入），两父形态对 git 完整可见。
- 合并/cherry-pick/revert 的冲突语义为 internal/merge 的诚实子集
  （docs/15 §5 git_deviations：相邻行触碰融合等三形态与 git 有系统性
  偏差；重命名不检测）。T65 用 merge-tree/cherry-pick/revert 对拍覆盖
  干净与非重叠冲突形态。
- **rebase 的 patch 等价剔除为近似**：上游 rev-list --cherry-pick 用
  git patch-id（diff 文本规范化哈希）；banyan 对 treeDiff 变更集
  (path, OID 对, mode) 做 SHA-1——blob OID 即内容身份，语义等价
  （同改动同哈希/异改动异哈希），但对"同路径同内容、不同 hunk 边界"
  的极端形态不做 diff 文本级归一。
- **UserApplyPatch 的应用器为统一 diff 子集**：上下文匹配+偏移搜索；
  二进制补丁（GIT binary patch）与 rename/copy 显式拒绝（InvalidArgument）
  而非误用；quoted 路径与模式变更（old mode/new mode 无内容 hunks）未
  支持——GitLab web 流的 format-patch 形态（T66 以真 format-patch 产出
  对拍 git am）覆盖增/改/删。
- mbox 邮件解析为必要子集：From-行切分、From/Date/Subject 头、
  [PATCH n/m] 前缀剥除；Content-Transfer-Encoding 与内嵌 MIME 未处理
  （8-bit 干净补丁为 web 流常态）。
- **GetArchive 的 tar 与 `git archive` 逐字节相等**（T67：双提交+嵌套
  目录+可执行位夹具）；tar.gz 为 gzip -n 容器包**同一 payload**（deflate
  流是 zlib 与 Go 的实现差异，逐字节无意义）；zip 为**条目级**等价
  （名/大小/mtime/内容/目录条目；zip 结构头与 extra 字段随标准库）。
  tar.bz2 无标准库压缩器（Unimplemented）；include_lfs_blobs：GitLab 归档
  端点**无条件发 true**——对无 pointer 的树该旗标是空操作（§8 修齐：
  仅当树内实际存在 LFS pointer、smudge 会改变字节时才诚实拒绝）；
  放不进 ustar prefix 的超长路径显式拒绝（git 会落 pax path 记录）。
- ObjectsSize 报告**未压缩**对象字节和（banyan 对象在 pack 内，per-object
  盘上字节是布局细节）；上游 --disk-usage 是压缩后口径。
- RepositoryInfo 为单一对象平面的诚实映射（Size 与 RepositorySize 同源、
  pack 数真值、LooseCount=ref 数、后端恒 FILES；commit-graph/bitmap/
  alternates 字段不填）。
- GetConfig 恒空流、OptimizeRepository/PruneUnreachableObjects 恒成功空
  操作（对象平面无 loose 漂移与 unreachable 概念）；offloading 策略与
  上游同形 Unimplemented。
- Fsck 为连通性走查（ref→commit→tree→blob 可达即解析），非 git fsck 全
  面（无 dangling/unreachable 报告面——散存对象本身不存在）。
- CalculateChecksum 与上游算法同构（逐行 SHA-1 XOR）；banyan 无 packed
  refs，全 ref 视作 loose。
- **检索面**：SearchFilesByContent 的正则为 Go RE2（上游 --perl-regexp 的
  语法子集）；匹配块形态与 git grep 对齐（上下文/分隔符/行号标记）。
  GetFileAttributes 只读根 .gitattributes（子目录级与负模式 !pattern 不
  解析）。FindLicense 为手工特征指纹子集（上游嵌 SPDX 全库），命中报
  SPDX id、低置信形态答空。
- **bundle 面**：v2 bundle（v3 capabilities/filters 显式拒绝）；FetchBundle
  收流为全内存形态（GB 级 bundle 的流式化留后）；CreateBundle 的对象集=
  广告视图闭包（织入 OID 原样——克隆侧逐字节可用，T68 实证）。
- **custom hooks**：可执行钩子面=banyan HTTP 回调注册表（docs/04 §7），
  文件树钩子不执行；tarball 仅经 RepoBlobStore 往返保备份移植。
- **远端面**：FetchRemote/CreateRepositoryFromURL 仅 smart-HTTP（go-git
  transport，模块既有依赖）；ssh 远端（ssh_key/known_hosts）Unimplemented。
  force 覆盖已有分支 tip 触发织入重建（孪生树全同，OID 变化=双视图规则
  §6）；FetchSourceBranch 新建 ref 原样 OID、覆盖已有 ref 同孪生规则。
- **切片十一形态差**（详注 §6a）：备份 sink=本地目录（非 gocloud 云端，
  键位同形）；对象池=物化拷贝（非 alternates 读穿透，物理不省空间）；
  praefect 族=cluster-of-one 自洽回答（多数值 id 平面不存在→NotFound、
  generation=pack 计数）；RewriteHistory 旧对象保留待 GC（上游同义）；
  PackObjectsHookWithSidechannel=FailedPrecondition 门（无 git 进程回环
  方）；GenerateBundleURI 不做协议 v2 bundle-uri 能力广告（广告面仍由
  git config 驱动，上游同形）；CheckBlobsGenerated 依赖 go-enry（纯 Go
  新依赖，cgo 版 oniguruma 不编译进来）。

## 6a. 收尾 40 面的实现语义（切片十一，原定稿 Unimplemented 组）

此前 33 个定稿 Unimplemented + Repository 尾 6 + SSH 尾 1 在切片十一
全部落地。每组都给**单节点诚实语义**——行为可测、与上游同形处同形，
形态差异显式列出（§6 汇总）：

- **CleanupService 2（历史重写）**：纯 Go 重写引擎——按 OID 剥 blob、
  literal/regex redaction→`***REMOVED***`（git-filter-repo
  --replace-text 子集），逐提交重编码（树重写+父映射+tag 对象 object
  行改写），全部 ref 落新链。**disjoint 广告历史**：ref 重建走删时间线
  +重建+ResetWoven（织入视图从新源链重织，docs/04 §6a 的 force-push
  视图重建），旧提交不再可达——T71 clone 断言 `rev-list --all` 只含
  新链。旧对象留在存储（上游同语义：GC 另行）。ApplyBfgObjectMapStream
  按 old→new 映射删内部 ref（environments/keep-around/merge-requests/
  pipelines/remotes——上游 InternalRefPrefixes 同表）并流出 entry 批。
- **Backup 面 4**（BackupRepository/RestoreRepository + WriteBackupID/
  ReadLatestBackupID）：本地目录 sink，**键位与上游 gocloud 布局同形**
  （`<prefix>/<storage>/<path>/<id>/NNN.bundle|.refs|.custom_hooks.tar` +
  `manifests/.../<id>.toml` + `backup_ids/<id>` 标记）；manifest 是上游
  TOML 形状的读写子集；增量=steps 堆栈+对象差集 bundle（boundary 为
  prerequisite 行，ingestBundle 校验闭环）；refs 文件=show-ref 行、
  restore 权威重置（含多余 ref 删除）+HEAD 复原+hooks 回灌。未配置
  BackupDir=FailedPrecondition（上游未配置同形）。分区备份（§Praefect）
  复用快照 TAR，键名用 checksum 充当 LSN——未变更分区重备份=
  AlreadyExists（上游按 LSN 去重同义）。
- **快照/复制 3**（GetSnapshot/CreateRepositoryFromSnapshot/
  ReplicateRepository）：快照=**真 bare 仓 TAR**（HEAD symref/
  packed-refs 含 `^<oid>` 剥离行/pack+**手写 idx v2**（fanout/排序名/
  crc32/32 位偏移/pack 尾校验和命名/idx 自校验））——git 直接 clone 该
  TAR（T73 oracle，含 fsck --strict 全量 crc 验证）；FromSnapshot=
  HTTP 拉取（Authorization 头透传）+pack 摄入+refs/HEAD 重建，失败
  回滚；Replicate=目标删后全闭包+refs+HEAD+hooks 镜像。
- **ObjectPoolService 6 + InternalGitaly 5**：池=独立仓（@pools/
  <h2>/<h2>/<hex64> 映射成 pools-<hex64> 存储键，repo.go 的既有折叠）
  +repo_pools 注册表（storage 层新表，三方言+memory+remote-meta 端点）。
  **物化即 alternates**：link 把池对象拷入成员（banyan pack 每仓独立
  的诚实等价物——成员从空仓变可解析池对象，T74 行为断言）；物理去重
  不做。池元数据 4 RPC 与 WalkRepos 走同一注册表（pool_disk_path 记
  gitaly 原始路径）。
- **HookService 7**：桥接 docs/04 §7 的 HTTP 回调注册表（新导出
  gitwire.FireRepoHooks）。pre-receive/update **fail-closed**：回调
  拒绝=exit 1+首行错误上 stderr（T75 httptest 对拍）；post-receive/
  ReceiveReportHook best-effort 投递；ReferenceTransactionHook 恒 0
  （写者原子，无事务相事件）；**PackObjectsHookWithSidechannel/
  SignalPostReceiveReady 为 FailedPrecondition/确认门**——banyan 无
  git 进程，sidechannel 回环方不存在（与上游"备份未配置"同形的
  诚实拒绝，非静默）。
- **Praefect/Partition/RefTransaction 10**：**cluster-of-one**——本
  storage 即 primary 即唯一健康副本：RepositoryReplicas 答
  primary+checksum（XOR 校验和复用）；SetAuthoritativeStorage 只接受
  本 storage；SetReplicationFactor>1=InvalidArgument（storages=1）；
  MarkUnverified=0；Dataloss 空流；GetRepositoryMetadata 按 path 查询
  （generation=pack 计数；数值 id 平面不存在→NotFound）；分区=每仓一
  分区（ListPartitions 分页）；事务恒 COMMIT（单投票者）。
- **AnalysisService 1**：CheckBlobsGenerated=go-enry IsGenerated（纯
  Go）对每 blob 前 2KiB+路径判定；支持 `<rev>:<path>` 路径定界
  revision（上游 AllowPathScopedRevision 同义）；T76 断言
  package-lock.json=generated、main.go=not。
- **GenerateBundleURI**：向 sink 的 `<storage>/<path>/uri/default.bundle`
  写全量 bundle（上游 sink 键形；每次覆盖）——响应为空与上游一致，
  URI 由 sink/CDN 侧发现；v2 bundle-uri 能力广告仍不在 upload-pack 面
  （协议 v2 专属特性，§6）。

## 7. 测试

- 单测（internal/gitaly）：鉴权门/ServerInfo/生命周期（@hashed 名/
  AlreadyExists/存储名 404）/ref+commit+blob 面共 14 子测。
- T60（integration）：真 git 客户端 push 三提交+双形态 tag，gRPC 读回
  **逐项对拍 git 本地答案**（ls-remote/rev-parse/rev-list/ls-tree/
  cat-file），UpdateReferences 写回 git 可见；@hashed 建仓后经 gitwire
  push、gRPC 读回。oracle = git 本身。
- T61（integration）：**workhorse 模拟器**（HTTP 入 → gRPC 流 → gitwire
  mux）承载真 git push 全链路；推送内容经原生 /r/ 面 clone+fsck 对拍；
  InfoRefs 广告与原生端点逐字节相等；SSHReceivePack 广告会话 +
  exit-status；存储名错误在流上以 NotFound 冒出（服务端流式 RPC 的错误
  面契约）。
- T62（integration）：**sidechannel 全链路**——mini-workhorse（连接带
  "backchannel" 魔数 + yamux 客户端 + 回调注册表）上，真 git clone 经
  PostUploadPackWithSidechannel 完成（内容/fsck 与源仓对拍）；SSH 上传
  会话经 sidechannel 跑通（v0 最小 fetch：广告→want/flush/done→NAK+PACK）；
  同一监听器上普通 gRPC 客户端照常工作（嗅探透传），无 sidechannel 元数据
  的调用被拒绝。
- 单测（blob/repo 面新增）：GetBlobs 三形态（命中两枚逐字节/miss 空消息）、
  ListBlobs 预算内带内容、ListAllBlobs 全头并集、FindMergeBase、WriteRef
  写回 FindBranch 可见、RepositorySize>0。
- 单测（operations/LFS 面，operations_test.go）：LFS 三件套（历史内已删
  pointer 仍列出；混型对象静默过滤；空 BlobIds/Revisions=InvalidArgument）；
  分支生命周期（建/已存在/缺 start point/错旧值 CAS/删/再删）；annotated
  tag（对象可解析+peel 回目标）与 lightweight；FF（非 FF="not fast
  forward"）与 UserUpdateBranch 陈旧 oldrev 拒绝；UserCommitFiles 全动作
  流（CREATE 执行位/MOVE InferContent/DELETE/CREATE_DIR=.gitkeep）后逐
  字段核对提交对象（author/committer/时间戳/parent）。
- T64（integration）：真 git push 含 LFS pointer 与已删文件的仓库，
  gRPC 读写与 git oracle 对拍——ListLFSPointers 命中 pointer（FileOid/
  FileSize/Data 与 cat-file 一致）；分支/tag 经 gRPC 建删后 ls-remote
  可见性一致，annotated tag fetch 回源仓 `cat-file -p` 全字段核对；
  UserFFBranch 前后 ls-remote OID 对拍；UserCommitFiles 产物 git fetch
  回源仓，`git show` 核对 author/committer/消息/树内容（含 MOVE 保留
  pointer 原文与 DELETE 生效）。
- 单测（合并面，operations_merge_test.go）：干净合并（两亲/树内容/
  消息/作者）、冲突 detail（ConflictingFiles+ConflictingCommitIds）、
  客户端中止、MergeToRef（目标 ref 落位 + 非 merge-requests 前缀拒绝）、
  squash（单亲/尾换行/end 树等）、cherry-pick（作者保留 + 二次拾取
  ChangesAlreadyApplied）与 revert（作者=操作者 + 变更撤销）。
- T65（integration）：**git 自家合并机械作 oracle**——干净合并的树与
  `git merge-tree --write-tree` **逐字节相等**；冲突判定与 git 一致
  （同文件双改，ConflictingFiles=alpha.txt）；MergeToRef 的两父合并提交
  fetch 回源 `--format=%P` 验证；squash 树与 end 提交树等（CommitDelta
  空集）；cherry-pick 在干净克隆中与 `git cherry-pick` **树相等**且源
  作者保留、`git revert` 对拍树相等且作者=操作用户；合并后的 FF 收敛
  （ls-remote 主分支=目标 tip，fetch 复核）。
- 单测（rebase/patch/submodule 面，operations_rebase_test.go）：rebase
  链形状（重放坐在 main tip 上、消息保留、committer=操作者、目标 ref
  广告）；confirmable 两阶段（apply 移支 / 中止不动）；apply patch
  系列（多文件 hunk 应用、作者/消息来自邮件、内容逐字节）与上下文
  冲突（"Patch failed at 0001"）；submodule 指针（gitlink 模式保持、
  already-at / 非子模块路径拒绝）。
- T66（integration）：**git rebase / format-patch+am 作 oracle**——
  UserRebaseToRef 与 UserRebaseConfirmable 的重放树均与本地
  `git rebase` **逐字节相等**（log 逐条核对 subject+committer）；
  UserApplyPatch 吃真 `git format-patch` 产出的双补丁 mbox，树与
  本地 `git am` **逐字节相等**（作者/主题保留）；冲突补丁
  FailedPrecondition 干净失败；UserUpdateSubmodule 前后 `ls-tree`
  核对 gitlink 模式与目标 OID。
- 覆盖补测（2026-10 审计）：16 个已实现但无直接调用的 RPC 补齐
  （DiskStatistics/GetObjectDirectorySize/FindAllBranches/FindAllTags/
  FindAllRemoteBranches/List{Branch,Tag}NamesContainingCommit/
  ListAllCommits/ListFiles/ListCommitsByOid/GetCommitMessages/
  CheckObjectsExist/ListLastCommitsForTree/CommitDiff/RawPatch/DiffBlobs）；
  审计击落三枚实现 bug：patchStats 无尾换行漏计（git --numstat 语义）、
  revision `~N` 后缀把 "~1" 读成 11 跳、RenameRepo 丢 hooks 行。
- 单测（维护面，repository_maintenance_test.go）：checksum 手工 XOR 复算
  与空仓 40×0；fsck 健康仓净；repo info 字段映射；housekeeping 空操作与
  offloading 拒；ObjectsSize bidi（后续带 repository 拒/缺 revision 拒）；
  raw changes A/D/M 形状；archive tar 结构（pax_global_header/mode/
  typeflag/10240 对齐）+ tar.gz 同 payload + zip 条目 + path/elide +
  bz2/LFS 显式拒。
- T67（integration）：**git archive / show-ref / diff --raw 作 oracle**——
  GetArchive 的 tar 输出与 `git archive --format=tar --prefix=…` **逐字节
  相等**（含 exec 位与嵌套目录）；tar.gz 解包 payload 与 git tar 相等
  （gzip 头无 name/mtime）；zip 条目逐项对拍；CalculateChecksum 从
  `git show-ref --head` 行集复算相等；Fsck 净；GetRawChanges 的 blob
  OID/size/mode 与 `git diff --raw` + rev-parse 一致；RemoveRepository
  后 ls-remote 失败且幂等。
- 单测（检索面，repository_search_test.go）：pathspec+filter+分页；grep 块形态
  （NUL 前缀/行号/上下文/EndOfMatch）；attributes 四态与 basename/路径模式；
  MIT 指纹与无候选空答；校验错误在 Recv 冒出（流式 RPC 契约）。
- 单测（custom hooks 往返 + meta conformance repo_blobs 段）：tarball 分块
  流入/流出逐字节、upsert 覆盖、missing 答空、rename 携带/delete 清除。
- T68（integration）：**git bundle 双向 oracle**——banyan 产的 bundle 过
  `git bundle verify` 且 clone 内容逐字节；`git bundle create --all` 产物
  经 FetchBundle/CreateRepositoryFromBundle 摄入 banyan，clone 对拍。
- T69（integration）：**FetchRemote 以 go-git 客户端拉 banyan 自身
  smart-HTTP 面**（跨实现互操作）；FetchSourceBranch 仓对仓 raw OID 原样；
  force 覆盖分支断言内容级（双视图孪生）。
- T70（integration）：**收尾三面 oracle**——ListConflictFiles 的标记体含
  <<<<<<</两侧内容/>>>>>>>；ResolveConflicts 落 merged 分支 clone 核对
  解决内容+theirs 侧自动合并；**FastExport 流经 git fast-import 完整往返**
  （blob 先于所属 commit 的顺序是 fast-import 硬约束——首版交错被击落）；
  UpdateRemoteMirror 推 banyan 自身面后 ls-remote 对拍；CreateFork 全 ref
  （含 annotated tag peel）clone 对拍。
- 单测（清理/praefect 面）：RewriteHistory 内容级断言（strip/redact/
  副本 ref 全部落新链/历史可走）+ 校验面（空规则/坏 OID/换行 pattern/
  二发 repository 拒绝）；ApplyBfgObjectMapStream 内部 ref 删除+entry
  流；praefect 族 cluster-of-one 全断言（replicas/authoritative/rf
  边界/metadata/分区/事务 COMMIT）。
- T71（integration）：**历史重写 oracle=clone**——strip+redact 后 clone
  断言 `rev-list --all` 只含新链（disjoint：旧链不可达——首版仅 force
  改写会织入旧链为祖先，删时间线+ResetWoven 后击落）、被剥路径全历史
  消失、secret 全历史→`***REMOVED***`；BFG map 删 keep-around ref。
- T72（integration）：**备份/恢复 oracle=clone**——sink 键位核对（bundle/
  refs/hooks/manifest 上游同形）；删仓→恢复→clone 与源仓逐 OID 对拍
  （含 annotated tag subject）；hooks tarball 经 sink 往返；增量 run-2
  manifest 堆叠 002 步+previous_ref_path、恢复重放全 steps；ID 注册表
  latest 按标记 mtime 排序；未配置 sink=FailedPrecondition。
- T73（integration）：**快照 oracle=git clone 裸目录**——手写 idx v2 经
  git clone+fsck --strict 全量验证（首版三败击落：pack 名须取尾部
  checksum 而非全文件 SHA、idx 校验和字段同源、packed-refs 剥离行是
  `^<oid>` 独立行、bare 骨架需 refs/ 目录条目）；FromSnapshot 经 HTTP
  往返（Authorization 断言）；Replicate 到已有仓替换后 clone 对拍；
  GenerateBundleURI 的 bundle 过 `git bundle verify`。
- T74（integration）：**对象池行为面**——建池/二次建=FailedPrecondition/
  非 @pools 路径拒绝；空 fork link 后可解析池对象（物化=alternates 的
  行为等价）；registry 全 RPC（GetObjectPool/Scan/List/Upstreams/
  WalkRepos）对拍；FetchIntoObjectPool 扩池；断开后物化对象仍在；
  DeletePool 连带成员解链。
- T75（integration）：**hook 回调对拍**——httptest 回调 allow/decline 两
  态：pre-receive exit 0/1+stderr 首行、update 同 fail-closed、
  post-receive/ReceiveReport best-effort 投递计数、事务 hook 恒 0、
  sidechannel 面 FailedPrecondition。
- T76（integration）：**收尾双面 oracle**——CheckBlobsGenerated（go-enry：
  package-lock.json=generated、main.go=not，`HEAD:<path>` 定界）；SSH
  upload-archive wire：band-1 重组载荷与 `git archive --format=tar HEAD`
  **逐字节相等**，坏参数=NACK+exit 1（首版 ACK+sideband 混帧被 oracle
  击落）。

## 8. GitLab-CE 活体实测（2026-11-07，T83）

> 接入操作手册（怎么接、怎么验、坏了怎么查）见
> [docs/20-gitlab-integration.md](20-gitlab-integration.md)；本节是验收
> 记录与实现侧账本。

**一键演示**（`demo/gitlab/`）：`docker compose up -d` 拉起真 GitLab-CE
（19.5.3-ce.0，与 vendored 协议同 minor）+ banyan；`provision.sh` 走查全
链路——种子 root、铸 PAT、建项目（CreateRepository）、HTTP push
（workhorse→PostReceivePack）、API 读面（默认支解析/树/提交/裸 blob/
归档），末尾以 banyan 自有面核对 @hashed 落库。GitLab 侧接线：
`gitlab_rails['repositories_storages']`（18.0 移除了 `git_data_dirs`），
token/存储名与 banyan 配置一致。T83（integration，opt-in：
`BANYAN_GITLAB_URL`+`BANYAN_GITLAB_TOKEN`）是同一循环的 Go 形态，无
demo 环境即跳过。

**实测揪出并修齐的六处协议缝**（模拟器全绿≠活体绿——每个都有回归
测试钉住）：

1. **v2 HMAC bearer**：Rails 16.x 起每次调用现签
   `v2.<HMAC>.<issued_at>`，服务端只做裸比较必拒（首发现象：建项目
   400 "invalid bearer token"）。修：±30s 窗口验签，裸 token 并存
   （上游 auth/token.go 语义）。
2. **FindDefaultBranchName 须答全限定名**：Rails `Gitlab::Git.branch_name`
   对无 `refs/heads/` 前缀的答案直接 nil——root_ref nil、整站读面视仓
   为空。修：按上游测试表语义（默认支存在→全名；不存在→首支；无支
   →空；head_only→恒答 symref 目标含悬空）。
3. **ListRefs pattern 前缀语义**：Rails 发字面量 `refs/heads/`（尾斜杠
   非通配），for-each-ref 的 match_name_as_path 三分支命中（前缀止于
   边界/pattern 自身带尾斜杠/wildmatch）——banyan 只做 glob 必空。
4. **GetTreeEntries 的 path "."**：Rails 列根目录发 `.`，逐段查找按
   字面量 miss（现象：tree API NoRepository）。修：splitPath 归一。
5. **FindTag 缺失须带结构化错**：上游 NotFound 附
   `FindTagError{TagNotFound}` detail，Rails 映射为可救的
   ReferenceNotFoundError；裸 NotFound→NoRepository→归档等调用方 500。
6. **GetArchive 的 include_lfs_blobs**：GitLab 恒发 true，原实现无条件
   拒。修：树内实有 LFS pointer（smudge 会变字节）才拒，无 pointer
   即普通归档——含 pointer 的拒绝与不含的放行都有测试钉住。
7. **TreeEntry 缺失路径的应答形态**（2026-11-07 晚，**UI 浏览面**击出：
   API 走查测不到——真项目页恒探测 `.git-blame-ignore-revs`）：上游对
   缺失答 **NotFound 错误**（Rails blob finder `rescue GRPC::NotFound→nil`）；
   banyan 原答零消息空流——Ruby 客户端 `entry.data=` 落 nil 直接 500 整个
   项目页。修：缺失答 NotFound（T63 钉住，注意流式 RPC 错误在 Recv 冒出）。

**已知开放缺口（登记待建）**：post-receive → Rails internal API 回调链
（活动事件/pipeline/MR-from-push/缓存失效）。walkthrough 以显式
`expire_all_method_caches` 替身绕过；正解是 banyan 接收路径向 GitLab
internal API（gitlab-shell 协议 `/api/v4/internal/post_receive`）的适配
器，形态见 docs/14 M5。

**子树投影 ref（T84，docs/20 §7）**：gitaly 面的广告桥接到内建
gitwire（SmartHTTP SSH 同源），子树投影 `refs/banyan/tree/<dir>` 因此
对 GitLab 客户端原样可见——ls-remote/fetch/推回全走 GitLab URL 与
凭证（活体验收）；gitaly 的 listRefs 与 gitwire 广告保持同集（T60
钉住），投影 ref 的接收在 gitwire 命名空间路由成子树推送。

**演示工程杂记**（复用时省时）：omnibus nginx 监听 external_url 端口
（容器内 8180，映射须同端口）；`GITLAB_OMNIBUS_CONFIG` 仅首 boot 生成
`/etc/gitlab/gitlab.rb`；root 种子读 `ENV['GITLAB_ROOT_PASSWORD']` 且口
令过 GitLab 黑名单（"demo"/"pass"/"gitlab" 字样皆拒）；rails runner 须
`-u git`（postgres peer 认证）；PAT 铸造 `set_token(明文)`（19.x 起带
参）。
