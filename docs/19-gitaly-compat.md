# 19 · Gitaly 协议兼容层（实现）

> 目标：以 gitaly 的 gRPC protocol 为契约实现全部接口，使本进程可**替代**
> gitaly 节点接入 GitLab（Rails/workhorse 只改 gitaly.yml 指向）。
> 方案与研究记录见 [18-gitaly](18-gitaly.md)；本文是实现账本与覆盖矩阵。

## 1. 形态

- **单 gitaly 节点形态**（praefect / RefTransaction / ObjectPool 集群面不在本层）。
- `internal/gitalypb/`：vendored 生成代码，**协议版本锁定 v19.5.2**
  （gitlab.com/gitlab-org/gitaly 上游 tag 验证为最新稳定版；MIT，附
  `LICENSE.gitaly`）。升级 = 整目录替换 + 全量回归。
- `internal/gitaly/`：兼容层。同一对象平面（MetaStore + ObjectService +
  service 视图），与 gitwire / JSON API 并列的第三个前端。
- 依赖：`google.golang.org/grpc v1.85.0`、`protobuf v1.36.12`（首批外部
  gRPC 依赖）。

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

- 鉴权 = 共享 Bearer token（`authorization: Bearer <token>` 元数据，
  恒时比较）；与上游一致：**token 之后全信**，细粒度授权在前端
  （Rails/console），不走 banyan auth 面（docs/07）。
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

## 4. 覆盖矩阵（163 RPC / 20 服务）

图例：✅ 本切片已实现并测试 · ⬜ 未实现（返回 `codes.Unimplemented`，
注册即生效——前端看到的是稳定契约而非连接失败）

### ServerService 4/4 ✅
ServerInfo ✅ · ReadinessCheck ✅ · ServerSignature ✅ · DiskStatistics ✅（字节数留待卷探测）

### RepositoryService 8/42
RepositoryExists ✅ · CreateRepository ✅（default_branch 落 repos 表，
SHA1；ObjectFormat 校验） · ObjectFormat ✅ · HasLocalBranches ✅ ·
RepositorySize/GetObjectDirectorySize ✅（= /repos/<repo>/size 同源） ·
WriteRef ✅ · FindMergeBase ✅（两 rev 单基形式：A 祖先集 + B 时间序走查
首命中） · ⬜ 其余（Delete/Rename/Bundle/Fork/Repack/Fsck/Checksum/…
维护与导入面，后切片）

### RefService 13/16
FindDefaultBranchName ✅ · FindBranch ✅ · RefExists ✅ · FindAllBranches ✅
（含 merged_only） · FindAllTags ✅ · FindTag ✅（含 annotated/lightweight
与 peel 语义） · FindAllRemoteBranches ✅（空集=诚实答案，banyan 无
remote-tracking refs） · ListRefs ✅（patterns glob/head/peel/pointing_at/
分页） · FindRefsByOID ✅（含 peel 匹配） · ListBranchNamesContainingCommit ✅
· ListTagNamesContainingCommit ✅ · UpdateReferences ✅（客户端流式批写；
Old OID 为咨询性——banyan ref 面写者串行化，竞争按最后写赢） · DeleteRefs ✅
· ⬜ GetTagSignatures / GetTagMessages（tag 正文解析扩展后补）

### CommitService 16/22
FindCommit ✅（rev 缺失返回空 commit，同上游契约） · CommitIsAncestor ✅ ·
ListCommits ✅（^排除/first-parent/max_parents/after/before/author/分页） ·
ListAllCommits ✅ · CountCommits ✅ · GetTreeEntries ✅（recursive） ·
ListFiles ✅ · ListCommitsByOid ✅ · GetCommitMessages ✅ ·
CheckObjectsExist ✅（bidi） · **FindCommits ✅**（pathspec 过滤历史=pathTouched 对首父树比较） · **CommitStats ✅** · **TreeEntry ✅**（单条目+内容分块） · **CommitsByMessage ✅**（消息子串） · **LastCommitForPath ✅** · **ListLastCommitsForTree ✅**（树首层逐条目） · ⬜ 6 个（RawBlame/CommitLanguages/签名×2/ListCommitsByRefName/TreeEntry 特性位）

### BlobService 4/7
GetBlob ✅（oid+limit） · GetBlobs ✅（revision:path 批量，miss 答空消息）
· ListBlobs ✅（revisions 树走查按 OID 去重，bytes_limit 内带内容） ·
ListAllBlobs ✅（全分支头并集） · ⬜ GetLFSPointers/ListLFSPointers/
ListAllLFSPointers（指针解析后挂同一枚举）

### SmartHTTPService 4/5
InfoRefsUploadPack ✅ · InfoRefsReceivePack ✅ · PostReceivePack ✅
（bidi：body 流入 mux 桥、响应流回；工作方式 = 内建 gitwire mux +
recorder，一次 HTTP 交换一条 gRPC 消息流，stateless-rpc 契约） ·
**PostUploadPackWithSidechannel ✅（fetch 通路解锁）**——sidechannel 传输
（internal/gitaly/muxcreds.go + sidechannel.go）：连接首字节 "backchannel"
魔数 → yamux 会话（gRPC 走流 1）→ 服务端反向开流，魔数 "sidechannel"+BE
int64 id+"ok" 确认交换；**数据相位非对称帧**（客户端→服务端 pkt-line 分帧
≤65531B/帧、裸 "0000" 帧模拟半关闭；服务端→客户端裸字节）。同一监听器经
TransportCredentials 嗅探兼容普通 gRPC 客户端（listenmux 同构）。 ·
⬜ AdvertiseBundleURI

### SSHService 2/3
SSHReceivePack ✅（bidi：stdin 泵入会话、stdout 分块流回、exit-status
末条；复用 gitwire.ServeSSHSession——从 serveGitExec 抽出的 io 流会话核
心，docs/04 §7b 的无守护进程形态） · **SSHUploadPackWithSidechannel ✅**
（交互式 upload 会话整体跑在 sidechannel 上——广告/协商/包字节） ·
⬜ SSHUploadArchive

### DiffService 8/10
CommitDiff ✅ · CommitDelta ✅ · RawDiff ✅（**与 git diff 逐字节一致**，T63） · RawPatch ✅（同 RawDiff 体） · DiffStats ✅ · DiffBlobs ✅（字节对直评） · GetPatchID ✅（SHA-1 over patch，best-effort） · FindChangedPaths ✅（Tree/Commit 双形态） · ⬜ RangeDiff/RawRangeDiff（范围互比，后切片）。底层=treeDiff（树递归对比）+unifiedPatch（go-diff 行级 + git 格式头：diff --git/index 行/@@ 计数=1 省略/dev-null 语义）。

### 其余服务 ⬜（全部已注册）
OperationService(16) · HookService(7) · ConflictsService(2)
· RemoteService(3) · CleanupService(2) · ObjectPoolService(6) ·
InternalGitaly(5) · BackupService(2) · PartitionService(2) ·
AnalysisService(1) · PraefectInfoService(6) · RefTransaction(2)

**小计：59/163。**

## 5. 切片路线（续）

1. **LFS pointer 面**：解析 pointer blob（文本头解析）挂 GetBlobs 枚举。
2. **OperationService**：user 系 merge/rebase/revert/squash/cherry-pick
   ——复用 merge-ort 子集与 service.CreateCommit 服务端提交路径。
4. **Repo 维护面**：DeleteRepository（需散存 pack GC 设计）/
   GetArchive/Fsck/CalculateChecksum。
5. **Import/Remote 面**：CreateRepositoryFromURL/Bundle（import 工具编排）。
6. **Praefect 面**（可选）：PraefectInfoService/RefTransaction——仅当
   目标拓扑含 praefect；单节点形态明确不做。

## 6. 已知语义差异（诚实清单）

- UpdateReferences 的 Old OID 不做强 CAS（banyan ref 面写者串行化；
  语义=最后写赢，见 docs/04 §6a 的 rollback guard）。
- DiskStatistics 字节数暂为 0（卷探测接运维面后补）。
- `git_version` 为常量声明（纯 Go 实现无 git 二进制可询）。
- CountCommits 的 path 过滤未实现（history simplification 需树 diff，
  后续接 view 引擎；当前按全历史计数 + 时间窗）。

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
