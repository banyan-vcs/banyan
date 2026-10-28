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

### RepositoryService 4/42
RepositoryExists ✅ · CreateRepository ✅（default_branch 落 repos 表，
SHA1；ObjectFormat 校验） · ObjectFormat ✅ · HasLocalBranches ✅ · ⬜ 其余
（Delete/Rename/Size/Bundle/Fork/Repack/Fsck/…维护与导入面，后切片）

### RefService 13/16
FindDefaultBranchName ✅ · FindBranch ✅ · RefExists ✅ · FindAllBranches ✅
（含 merged_only） · FindAllTags ✅ · FindTag ✅（含 annotated/lightweight
与 peel 语义） · FindAllRemoteBranches ✅（空集=诚实答案，banyan 无
remote-tracking refs） · ListRefs ✅（patterns glob/head/peel/pointing_at/
分页） · FindRefsByOID ✅（含 peel 匹配） · ListBranchNamesContainingCommit ✅
· ListTagNamesContainingCommit ✅ · UpdateReferences ✅（客户端流式批写；
Old OID 为咨询性——banyan ref 面写者串行化，竞争按最后写赢） · DeleteRefs ✅
· ⬜ GetTagSignatures / GetTagMessages（tag 正文解析扩展后补）

### CommitService 10/22
FindCommit ✅（rev 缺失返回空 commit，同上游契约） · CommitIsAncestor ✅ ·
ListCommits ✅（^排除/first-parent/max_parents/after/before/author/分页） ·
ListAllCommits ✅ · CountCommits ✅ · GetTreeEntries ✅（recursive） ·
ListFiles ✅ · ListCommitsByOid ✅ · GetCommitMessages ✅ ·
CheckObjectsExist ✅（bidi） · ⬜ 12 个（diff/blame/签名/语言/LastCommitForPath/
ListCommitsByRefName/FindCommits 等）

### BlobService 1/7
GetBlob ✅（oid+limit，1MiB 分块） · ⬜ GetBlobs/ListBlobs/ListAllBlobs/
LFSPointer 系

### 其余服务 ⬜（全部已注册）
DiffService(10) · SmartHTTPService(5) · SSHService(3) · OperationService(16)
· HookService(7) · ConflictsService(2) · RemoteService(3) · CleanupService(2)
· ObjectPoolService(6) · InternalGitaly(5) · BackupService(2) ·
PartitionService(2) · AnalysisService(1) · PraefectInfoService(6) ·
RefTransaction(2)

**小计：32/163。**

## 5. 切片路线（续）

1. **传输面**（P0）：SmartHTTPService + SSHService——gitwire 的
   upload-pack/receive-pack 直接挂接，GitLab 走 workhorse 的 git HTTP/SSH。
2. **Diff + Commit 补全**：CommitDiff/DiffStats/LastCommitForPath/
   ListLastCommitsForTree/FindCommits（范围 rev-list）。
3. **Blob 补全**：GetBlobs/ListBlobs/LFS pointers。
4. **OperationService**：user 系 merge/rebase/revert/squash/cherry-pick
   ——复用 merge-ort 子集与 service.CreateCommit 服务端提交路径。
5. **Repo 维护面**：DeleteRepository（需散存 pack GC 设计）/
   RepositorySize/GetArchive/Fsck。
6. **Import/Remote 面**：CreateRepositoryFromURL/Bundle（import 工具编排）。
7. **Praefect 面**（可选）：PraefectInfoService/RefTransaction——仅当
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
