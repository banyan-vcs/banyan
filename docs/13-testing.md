# 13 · 集成测试

六轮手工测试（MVP → 统一 DAG → 轮询协商 → 交叉压测 → 多人协作）的
全部验证点已固化为**可自动回归的集成测试**（`integration/`，断言用
testify，文件按测试目标拆分）：

| 文件 | 测试目标 |
|---|---|
| `helpers_test.go` | 共享基建：进程内服务端装配、git CLI 封装、种子/克隆/断言辅助 |
| `transport_basic_test.go` | 基础传输：根/子树克隆往返、引用操作、增量推送拉取（T1–T4） |
| `subtree_collaboration_test.go` | 子树协作语义：线性织入、连续推送、非干扰/路径内互斥、多用户同文件、多作用域稳定性（T5/T7/T8/T10/T18） |
| `push_guards_test.go` | 服务端守卫与协商：v1 have 协商终态 ACK、竞态窗口 sameXView 兜底（T8b/T9） |
| `protocol_compat_test.go` | git 协议兼容矩阵：版本降级、partial clone、浅化 deepen/unshallow（T13–T16） |
| `persistence_test.go` | SQLite 重启持久化（T17） |
| `rpc_log_test.go` | /api/v1/log 路径过滤（T11） |
| `packing_efficiency_test.go` | 索引与打包效率：have 减除增量包精确构成（T19）、serve 路径 delta 化（T29） |
| `refs_and_repack_test.go` | tags/自定义命名空间端到端 + repack 碎片合并/delta/重启（T20/T21/T21b） |
| `cli_workspace_test.go` | 薄工作区 CLI：最小化 clone/status/commit/pull/log/cat + 冲突恢复 + 子树工作区（T22/T22b） |
| `ha_cluster_test.go` | HA 集群（docs/10）：进程内 refs+storage×4+gateway，散布放置/节点宕恢复/动态副本/refs 重启（T23-T26） |
| `protocol_v2_test.go` | protocol v2 命令面（docs/04 §9b）：裸 pkt 断言（广告/ls-refs/fetch 段组帧/协商轮/可达性门）+ 真实 git 全链路（clone/fetch/浅化/partial/子树，GIT_TRACE_PACKET 证明 v2 生效） |

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
| T13 | `TestProtocolVersionMatrix` | 协议版本矩阵：v0 显式 / v2 强制（降级走 v1）clone + 增量 fetch + 子树 URL（docs/04 §9） |
| T14 | `TestPartialCloneFilter` | **partial clone 全链路**（docs/04 §4）：`blob:none`/`tree:0` 初始包裁剪、promisor 惰性取回（want=缺失 OID，经 allow-reachable-sha1-in-want 可达性校验）、后续 fetch 带 filter 行不破坏、不支持的 spec ERR 拒绝 |
| T15 | `TestShallowClone` | **浅化协议**：depth 1/2/超深（空边界=裸 flush）+ 子树投影空间浅化/unshallow（docs/04 §3a 参考流对照） |
| T16 | `TestShallowFetchDeepen` | 浅仓增量：普通 fetch（上报边界裁剪、浅文件不变）/ 加深（边界下移+unshallow 回显）/ unshallow 清除 |
| T17 | `TestSQLiteMetaRestartPersistence` | **M2 持久化**：整进程重启后仓库/pack/对象索引/时间线头全存活；冷启动 nil-index 崩溃回归；重启后可继续 push |
| T18 | `TestSequentialMultiScopeStability` | 顺序多作用域到达：零重复折叠、tip 不漂移、X 外变更不阻塞；含冲突恢复后继续推送（投影守卫 TreeKnown） |
| T19 | `TestIncrementalFetchHaveSubtraction` | **have 减除**（docs/09 §3.1）：增量包=精确 delta（1 提交/2 树/1 blob），未触碰对象不入包；子树 URL 同理；真实客户端端到端 |
| T29 | `TestServePathDelta` | **serve 路径 delta 化**（docs/11 P1-4）：跨包闭包在线编码过阈值后 OFS_DELTA 化（字节显著收缩）、真实客户端解包 delta 包（clone+fsck） |
| T30 | `TestV2*`（5 用例） | **protocol v2 命令面**（docs/04 §9b）：广告格式（无 service 前导）、ls-refs（ref-prefix/peel/symrefs/unborn、空格分隔属性）、fetch 段组帧（done 省 acks 段、sideband pack 帧、ready 终态轮）、真实 git clone/fetch/ls-remote/浅化/unshallow/partial/子树/push 恒 v0；两处被真实客户端钉住的 wire 细节（TAB 分隔符、done 省 acks）记录于 docs/04 §9b |
| T31 | `TestSynthBatchSinglePack` + storage `TestSynthBatchPacksOnce`/`TestLocationCacheHitsAndInvalidation` | **P0 落地**（docs/11）：B3 一次织入物化=一个合成 pack（含 pre-CAS flush 持久化点）；B1 定位 LRU（重复读零定位、Prefetch 批量、Known 批量存在性、repack 失效+重定位） |
| T34 | `TestPushWarmView` | **push 后预热织入**（docs/11 P2-3）：无需任何读请求 /.woven 推进到新头（根推送=原样链头；子树推送=根织入推进） |
| T35 | `TestClosureResponseCache` | **闭包响应包缓存**（docs/11 P2-2）：clone 形态纯函数缓存（首存/复用命中/tips 换键/ExactPackHit 不污染/haves 不入缓存） |
| T36 | `TestHAGatewayDiskCache` | **网关磁盘 pack cache**（docs/11 P2-1）：节点请求计数器下，缓存填充后整网关重启、克隆零节点请求（读放大翻转 + CheapPack 路径） |
| T37 | `TestThinPackFetch` + storage delta/thin 单测 | **真 thin pack**（docs/11 P2-6）：REF_DELTA 条目 + 字节收缩 + 未请求不发；真实客户端 v0/v2 双协议消费；delta 往返（增/删/中改/块拷贝/不可压）与 thin 写出器对 fix-thin 式解码器往返 |
| T38 | `TestAuthorizationWire` | **授权 wire 全矩阵**（docs/07）：匿名 401/错密匿名、个人分支推/看归属（本人+admin）、跨命名空间 push 拒绝、保护 main 开发者拒/admin 过、裸 OID 取隐藏分支拒（可见 tips 覆盖）、受限目录 403、v2 ls-refs 同过滤；含 credential.helper 复位（否则开发机管理器会替匿名请求补凭证） |
| T38b | `TestAuthorizationRPC` | **RPC 认证面**（docs/07 §5）：匿名 401/认证 200、login 对错密码、建仓端点 + admin 播种 + 开发者克隆 |
| T39 | `TestGitSSH` | **git-over-SSH 传输**（docs/04 §7b，docs/07 §7）：公钥认证（IdentifyKey）→ 同一授权矩阵（个人分支可见性/跨命名空间拒/保护分支/受限目录/未授权密钥拒）；v0 克隆 + 51 提交增量 fetch（**多轮 have 协商**：客户端第二批 have 后阻塞读，服务端必须逐 flush 应答——终态 ACK 模型）；v2 命令会话（ls-refs/clone）；push + 自动建仓 |
| T40 | `TestStorageModes` | **存储寻址双模式**（docs/03 §6）：named——磁盘有可读仓库名目录、rename 移动全部持有节点目录且持有集不变、旧名消失新名可克隆；hashed——任何节点磁盘无仓库名、键=storageID 的 @hashed fanout、rename 前后各节点文件树逐项相同（零存储动作）、rename 后 push 落同一键、新旧名克隆均正确 |
| T41 | `TestWorkflowBurst` | **并发突发全落地**（docs/15）：8 真实 git 客户端并发 push 同目录（各加新文件），merge train 下零拒绝全落地、内容齐备、既有内容不回退——无收敛时 7 个会收 non-FF |
| T42 | `TestWorkflowConvergence` + internal/merge 单测（含 **git merge-file oracle**） | **收敛语义**（docs/15 §2.2，rawPush 直钉 wire——真 git 客户端顺序性非 FF 在发送前自拒，收敛面=并发窗口）：不同文件分歧自动合并（两改俱在线性视图）；同文件同行冲突 → ng conflict 且票隔离（后续票继续）；冲突客户端 pull --rebase 停于真冲突、人工解决后续推（T18 路径在收敛链上成立）；blob diff3 与 git merge-file 在**重叠冲突形态**逐字节一致（含"同点双插=冲突"）；已知偏差钉在 T42b |
| T42b | `TestDeviationAdjacentEditsMergeCleanly` / `TestDeviationConflictSpansRenderWhole` / `TestBinaryIdentityShortcuts` | **merge 与 git 的已声明偏差**（对照 go-git merge-ort 移植的 xmerge 审计发现，git merge-file 现场验证双前提）：相邻行改动 banyan 干净叠加（git touch-fuse 判冲突——收敛引擎的有意宽姿势）；冲突块整段渲染（git ZEALOUS 精化缩小共同段>3 出 marker、≤3 行间隔融合成块——只影响 ng 诊断形态）；blob 同一性判定在 binary 检查**之前**（双侧相同二进制必须干净——回归修复，原实现误开冲突票） |
| T45（storage 单测） | `TestIdxLargeOffsetRoundTrip` / `TestGeometricGroups` / `TestIngestPackFileEquivalence` | **大仓三项**（docs/03 §4/§5）：idx 64 位偏移编解码往返（大偏移表，单 pack 无 4GiB 限制）；几何分组表驱动（相邻比判据、大 pack 不入组、多组划分）；流式摄入等价（与内存路径同对象集/同可读性/idx 落盘/整包缓存保持冷）——含空 pack 合法形态（集成 T20/T43 的"推已有对象"路径回归覆盖） |
| T46 | `TestMemoryChainSmallBudget` + storage `TestSpoolVerifiedCopy`/`TestReceivePackRoundtrip`/`TestDiskCacheStreaming` | **小内存全链路**（docs/03 §3.1/§4）：64MiB 软内存限制 + 堆峰值采样下，150MiB 不可压缩仓经 git→网关→3 存储节点 push（×3 副本）与 clone（v2+v0）全链路可用——峰值 13.5/12.4/9.9 MiB（修复前 783/611/512：节点 PUT 整包 ReadAll ×3、单对象提取整包 ReadFile、网关整包拉取 + closure 全图）；正确性双钉（head/对象数/工作树字节相等 + 服务端落盘 450MiB 门槛）；单测钉住滑动窗校验（sha1/sha256/坏 trailer/过短）、节点流式落盘（OID 同一性/短拷/坏包拒绝/临时文件无泄漏）、磁盘缓存流式写（abort 不留痕/complete 注册/GetCapped 闸门） |
| T46b（storage 单测） | `TestMemoizeBudgetEviction` / `TestRepackByteCapSplitsGroups` + HA T24-T26 回归 | **内存二轮审查**（docs/03 §3.1/§5）：全库同款模式扫描后修五处——reconciler 副本修复流式化（源 VerifyPack 滑动窗 + 目标 PutPackStream 各自流，节点落地二次校验；HA 修复用例回归覆盖）、decoded-pack memoize 总量 LRU（超预算逐出最旧、packOrder 元数据不随逐出丢失、逐出包重读自愈）、repack 合并组字节上限（1 字节上限拆组零合并/默认正常合并双钉）+ spool 编码入册（消灭整包输出缓冲与二次解码）+ DeletePack 存储键修正（hashed 寻址旧分片删不到）、响应缓存超限免缓冲直发、非 clone 形态 verbatim 精确命中流式化（首字节前失败回退 ladder、中断即终态不追加第二个 pack） |
| T47 | `TestHooksReceiveLifecycle` / `TestHooksDecline` / `TestHooksDirectoryScope` / `TestHooksTimeoutAndSecret` + gitwire `TestMatchHooks`/`TestHookOIDZeroRendering` + storage `TestHookRegistrySQLite` | **服务端 hook（注册表 + HTTP 回调，docs/04 §7）**：注册走 `/repos/<repo>/hooks`（GET/PUT/DELETE 往返 + 注销后零回调）；pre-receive 拒绝整 push 且响应体首行回传客户端、update 只拒点名分支（同推其余落地）、post-receive 只收 applied 集；payload 钉 repo/directory/user/repo_bytes（实时）与零 OID 全零 hex；**目录作用域**：lib 级钩子拒 lib 子树推送且只见 lib 回调、doc 推送走仓库级放行（directory=doc）；超时 fail-closed（timed out 回传）；Secret 头送达。单测：匹配（最具体优先/全祖先/根只仓库级/事件过滤）与 sqlite 注册表（重启往返/upsert 覆盖/幂等删除/校验拒绝） |
| T48 | `TestFullCloneBudget` | **规模统计 + 全 clone 预算**（docs/11 §6）：RPC `/size` 反映真实包字节（真随机内容——稀疏模式会 zlib 压穿）；预算武装后全 clone 拒（消息含 --depth 1 替代；v2 ERR 在 packfile 段头前——曾出 "bad band #69"）、`--depth 1` 过、既有克隆增量 pull 过、裸 OID lazy fetch 过；T22c `TestCLIWorkspaceDiscovery`：任意子目录向上发现工作区（status/commit 全工作区语义）、区外明确报错 exit 2 |
| T49（console/auth 单测） | console `TestChainResolution`/`TestEffectiveRoleInheritance`/`TestRenameStability`/`TestArchive`/`TestAccessGates`/`TestTokens`/`TestBuildSnapshot` + auth `TestSignedIdentity`/`TestUpstreamPolicyDecisions`/`TestUpstreamIdentify` | **产品控制面模型**（docs/17 §3-§5）：前缀链解析（未注册中间段不贡献、深度空档不截断）、additive-max 继承（父 developer+子 maintainer=子锚点 maintainer、兄弟子树不受污染、子级授权不上溯）、rename 保 repo_uid、归档隐藏路由、四门（fetch/push/管成员/管项目）；签名身份头（篡改/错钥/±30s 窗外全拒）；UpstreamPolicy 决策（子树独占授权可锚点 fetch 根拒、protected 需 owner、个人分支角色买不到他人命名空间、公开仓匿名、未知仓 fail-closed） |
| T50 | `TestConsoleE2E` / `TestConsoleMemoryChain` | **console 端到端**（docs/17 §6）：banyans+banyand 双进程拓扑（in-process 等价）真实 git 全链路——逻辑 URL 根/子树克隆推送、建仓经签名内部身份落到网关注册表、开发者推保护 main 拒+解保护过+复保护再拒（PATCH 即时生效=console 踢网关刷新快照）、reporter 子树推拒、锚点级 maintainer 授予后过且**根仓库对该用户仍 404**（子树独占=可执行方向）、public 匿名克隆、PAT 吊销立即失效、`/repos/{g}/{p}/size` 透传重映射；`TestConsoleMemoryChain`：64MiB 软限制下经 console 的 20MiB 不可压缩仓 push→repack→clone 全链路存活（代理贡献经直连网关隔离实验证零；残余增量为存储面 all-in-one 形态的服务路径分配，见 docs/17 §7 注） |
| T51 | `TestReviewSubprojectIsolation` + service `TestDiffTwoRefs` + console `TestMyers*` | **轻量级代码评审（锚点作用域）**（docs/17 §9）：双子项目并行评审互不干扰——真实 git 客户端克隆各自子树视图→推提案（锚点 main，基线=冻结的根分支指针投影）→开评审；评审者各只见本锚点评审（对方详情/评论/批准全 404）、作者不可自批、评论+批准并行落地互不污染、根成员聚见两条、diff 含正确文件与 hunk 渲染（D1 修复前为结构性断言）；service 侧钉住路径级 diff（增/删/改/锚点相对路径/同 refs 空 diff/未出生 ref 零头信号）；console 侧钉住 Myers（roundtrip 应用重建/上下文 hunk 形状/远距变更多 hunk/二进制与上限降级） |
| D1（开放缺陷） | `TestSubtreeViewCrossScopeCorruptionD1`（skip，修复后去 skip 即守卫） | **own-scope 推送后的跨作用域视图污染**（2026-10-26 由 T51 的锚点 diff 暴露）：根 main 含两个子树路径时，子树 A 推送 + A 视图重物化后，B 的 SubtreeHead 归零（push 报 "subtree view unavailable"）、A 自身视图随后双重嵌套（X 内容按自身相对路径再嫁接：token.go 与 services/auth/token.go 同检出）。既有 T5/T7/T8 绿是因为从不在 own-scope 推送后重物化兄弟视图。修复应落在 scoped weave 的 marker/advance 状态机 |

| T43 | `TestForcePushRevivalRejected` / `…Recovery` / `TestRootForcePushViewMonotonic` | **子树 force push 回滚守卫**（docs/04 §6a）：wire 无 force 信号、old 恒=广告值——回滚/重写丢他人内容的 force push 以连续 push 形态到达；四种形态（adopt/收敛 × 纯回滚/回滚+己方提交）真实 git 客户端全拒绝（non-fast-forward: would rewind，旧代码上实证全部 forced-update 落地）；拒后 pull --rebase 恢复路线畅通；根仓库 force push 放行（git 原生 verbatim）但织入视图单调——root-local 内容与目录时间线内容均不回魂 |
| T44 | `TestForcePushOptIn` | **仓库级 force push 开关 + 织入重建**（docs/04 §6a）：PUT /api/v1/repos/&lt;repo&gt;/force-push 运行时生效——关（默认）回滚仍拒（T43 语义不回归）；开后回滚链落地（forced update）且 `/.woven` 家族重置全量重织：lib 时间线回滚（a.txt 回退）而**下层 lib/nested 独立时间线内容保留**（时间线边界=回滚边界）、根视图历史**重建**（被回滚提交的织入体从 log 消失、nested 织入保留）、回滚后正常 push 即刻恢复；开关再关守卫重新武装 |
| T33 | `TestFetchTreesBinary` | **fetch-trees 二进制帧**（docs/05 §3.3、docs/11 P1-2）：bin/JSON 语义全等（树与 blob 集合逐字节一致）、二进制小于 JSON、真实 CLI clone 走 bin 路径物化 |
| T32 | storage `TestExtractObjectRoundTrip`/`TestLocalGetObjectViaDisk` + T23-T26 回归 | **P1-1 单对象解压**（docs/11）：idx 偏移 + delta 基链递归解压（含 ofs/ref 两型）；集群 wire 从整包降为单对象（/internal/object 端点）；修复 idx oid→offset 在 delta pack 上的按位配对错位（Parser Observer 为权威源），idx crc 段补真值 |
| T20 | `TestTagsAndAuxRefs` | **refs 命名空间泛化**：annotated/lightweight tag、tag 对象闭包、自定义 refs/<ns>、tag 删除与非 FF 拒绝、浅克隆 tag 剥离 |
| T21 | `TestRepackFragmentMerge` / `TestRepackRestartSurvival` | **repack**：碎片合并+delta（字节收缩、对象保全、ExactPackHit 恢复）、SQLite 重启后存活（UPSERT 重指向回归） |
| T22 | `TestCLIThinWorkspace` / `TestCLISubtreeWorkspace` | **薄工作区 CLI**（docs/06/10）：最小化 clone（无 .git、无提交闭包）、离线 status、服务端 changeset commit 经**真 git 客户端交叉验证**（fresh clone + fsck + subject）、log/cat 零下载、pull=服务端 tree-diff、base-moved 冲突退出码 1→pull→重提、子树工作区 commit 织入根视图 |
| T23 | `TestHAClusterScatteredPlacement` | **散布放置**（docs/10 §3）：每 pack 持有集=3 个不同节点、任一持有者可服务且校验和通过、登记即事实（读不依赖放置函数）、增量 push 新随机放置、cluster/nodes 端点 |
| T24 | `TestHANodeFailureAndRecovery` | **节点宕故障转移与恢复**（docs/10 §8）：kill 后新写/克隆照常（W=2 掩蔽死节点）、健康表 dead 判定、reconciler 补齐回 3 副本且 dead 节点移出持有集、恢复后克隆仍正确 |
| T25 | `TestHAPlacementDynamic` | **动态副本**（docs/10 §6）：R 3→2 触发剪枝（先登记后删除）、2→3 触发补齐；剪枝后读正常 |
| T26 | `TestHARefsRestart` | **refs 重启 RPO=0**（docs/10 §4）：SQLite 句柄释放+同址重启、心跳重新注册、重启前提交零丢失、重启后继续 push/clone |

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
