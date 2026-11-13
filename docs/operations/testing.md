# 集成测试

六轮手工测试（MVP → 统一 DAG → 轮询协商 → 交叉压测 → 多人协作）的
全部验证点已固化为**可自动回归的集成测试**（断言用 testify）。2026-11-09
起按功能域拆为**子包**（每包=独立测试进程，go test 并行调度；跨包共享
基建沉淀在 `integration/support`）：

| 子包 | 文件（节选） | 功能域 |
|---|---|---|
| `support/` | support.go | 共享基建：进程内服务端装配、git CLI 封装、种子/克隆/断言辅助 + 跨包助手（GitDated/GitV2/NewStack） |
| `basic/` | transport_basic、protocol_compat | 基础传输（T1–T4）与协议兼容矩阵（T13–T16） |
| `streaming/` | streaming_{clone,zoo,deepen}、gzbody、fullclone、import、clone_serving | 流式服务与内存链（T48/T53/T55/T57/T58/T59/T82） |
| `protocol/` | push_guards、force_push、workflow、protocol_v2、packing_efficiency、hooks | 写路径协议面：守卫/收敛/force-push、v2 命令面、打包效率、服务端 hook（T8b/T9/T19/T29/T30/T34/T35/T37/T41/T42/T43/T44/T47） |
| `subtree/` | subtree_collaboration、projection_refs、subtrees_api、weave_{consistency,provenance} | 子树协作语义与织入一致性（T5/T7/T8/T10/T18/T52/T78/T84/T88/T89/T90） |
| `ha/` | ha_cluster、refs_and_repack、persistence、storage_modes、memory_chain | HA 集群/存储模式/持久化/小内存链（T17/T20/T21/T23–T26/T31/T36/T40/T46） |
| `authz/` | auth、auth_tokens、ssh | 授权矩阵、token 细粒度、SSH 传输（T38/T38b/T39/T79–T81） |
| `cli/` | cli_workspace、cli_snapshot、cli_stack、sync | 薄工作区 CLI 全家（T22*/T91/T92/T93） |
| `gitalyrpc/` | 17 个 gitaly_*_test.go | gitaly gRPC 兼容面（T60–T76） |
| `gitlab/` | gitlab_demo、gitlab_hook、gitlab_pushopts | GitLab 活体集成（T83/T85/T86/T87） |
| `ops/` | rpc_log、console、ops_plane、metrics | RPC 读面/控制面/可观测（T11/T50/T54 + metrics） |

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
| T13 | `TestProtocolVersionMatrix` | 协议版本矩阵：v0 显式 / v2 强制（降级走 v1）clone + 增量 fetch + 子树 URL（docs/gateway/git-compat.md §9） |
| T14 | `TestPartialCloneFilter` | **partial clone 全链路**（docs/gateway/git-compat.md §4）：`blob:none`/`tree:0` 初始包裁剪、promisor 惰性取回（want=缺失 OID，经 allow-reachable-sha1-in-want 可达性校验）、后续 fetch 带 filter 行不破坏、不支持的 spec ERR 拒绝 |
| T15 | `TestShallowClone` | **浅化协议**：depth 1/2/超深（空边界=裸 flush）+ 子树投影空间浅化/unshallow（docs/gateway/git-compat.md §3a 参考流对照） |
| T16 | `TestShallowFetchDeepen` | 浅仓增量：普通 fetch（上报边界裁剪、浅文件不变）/ 加深（边界下移+unshallow 回显）/ unshallow 清除 |
| T17 | `TestSQLiteMetaRestartPersistence` | **M2 持久化**：整进程重启后仓库/pack/对象索引/时间线头全存活；冷启动 nil-index 崩溃回归；重启后可继续 push |
| T18 | `TestSequentialMultiScopeStability` | 顺序多作用域到达：零重复折叠、tip 不漂移、X 外变更不阻塞；含冲突恢复后继续推送（投影守卫 TreeKnown） |
| T19 | `TestIncrementalFetchHaveSubtraction` | **have 减除**（docs/storage/indexing-packing.md §3.1）：增量包=精确 delta（1 提交/2 树/1 blob），未触碰对象不入包；子树 URL 同理；真实客户端端到端 |
| T29 | `TestServePathDelta` | **serve 路径 delta 化**（docs/operations/performance.md P1-4）：跨包闭包在线编码过阈值后 OFS_DELTA 化（字节显著收缩）、真实客户端解包 delta 包（clone+fsck） |
| T30 | `TestV2*`（5 用例） | **protocol v2 命令面**（docs/gateway/git-compat.md §9b）：广告格式（无 service 前导）、ls-refs（ref-prefix/peel/symrefs/unborn、空格分隔属性）、fetch 段组帧（done 省 acks 段、sideband pack 帧、ready 终态轮）、真实 git clone/fetch/ls-remote/浅化/unshallow/partial/子树/push 恒 v0；两处被真实客户端钉住的 wire 细节（TAB 分隔符、done 省 acks）记录于 docs/gateway/git-compat.md §9b |
| T31 | `TestSynthBatchSinglePack` + storage `TestSynthBatchPacksOnce`/`TestLocationCacheHitsAndInvalidation` | **P0 落地**（docs/operations/performance.md）：B3 一次织入物化=一个合成 pack（含 pre-CAS flush 持久化点）；B1 定位 LRU（重复读零定位、Prefetch 批量、Known 批量存在性、repack 失效+重定位） |
| T34 | `TestPushWarmView` | **push 后预热织入**（docs/operations/performance.md P2-3）：无需任何读请求 /.woven 推进到新头（根推送=原样链头；子树推送=根织入推进） |
| T35 | `TestClosureResponseCache` | **闭包响应包缓存**（docs/operations/performance.md P2-2）：clone 形态纯函数缓存（首存/复用命中/tips 换键/ExactPackHit 不污染/haves 不入缓存） |
| T36 | `TestHAGatewayDiskCache` | **网关磁盘 pack cache**（docs/operations/performance.md P2-1）：节点请求计数器下，缓存填充后整网关重启、克隆零节点请求（读放大翻转 + CheapPack 路径） |
| T37 | `TestThinPackFetch` + storage delta/thin 单测 | **真 thin pack**（docs/operations/performance.md P2-6）：REF_DELTA 条目 + 字节收缩 + 未请求不发；真实客户端 v0/v2 双协议消费；delta 往返（增/删/中改/块拷贝/不可压）与 thin 写出器对 fix-thin 式解码器往返 |
| T38 | `TestAuthorizationWire` | **授权 wire 全矩阵**（docs/gateway/authorization.md）：匿名 401/错密匿名、个人分支推/看归属（本人+admin）、跨命名空间 push 拒绝、保护 main 开发者拒/admin 过、裸 OID 取隐藏分支拒（可见 tips 覆盖）、受限目录 403、v2 ls-refs 同过滤；含 credential.helper 复位（否则开发机管理器会替匿名请求补凭证） |
| T38b | `TestAuthorizationRPC` | **RPC 认证面**（docs/gateway/authorization.md §5）：匿名 401/认证 200、login 对错密码、建仓端点 + admin 播种 + 开发者克隆 |
| T39 | `TestGitSSH` | **git-over-SSH 传输**（docs/gateway/git-compat.md §7b，docs/gateway/authorization.md §7）：公钥认证（IdentifyKey）→ 同一授权矩阵（个人分支可见性/跨命名空间拒/保护分支/受限目录/未授权密钥拒）；v0 克隆 + 51 提交增量 fetch（**多轮 have 协商**：客户端第二批 have 后阻塞读，服务端必须逐 flush 应答——终态 ACK 模型）；v2 命令会话（ls-refs/clone）；push + 自动建仓 |
| T40 | `TestStorageModes` | **存储寻址双模式**（docs/storage/storage.md §6）：named——磁盘有可读仓库名目录、rename 移动全部持有节点目录且持有集不变、旧名消失新名可克隆；hashed——任何节点磁盘无仓库名、键=storageID 的 @hashed fanout、rename 前后各节点文件树逐项相同（零存储动作）、rename 后 push 落同一键、新旧名克隆均正确 |
| T41 | `TestWorkflowBurst` | **并发突发全落地**（docs/design/workflow.md）：8 真实 git 客户端并发 push 同目录（各加新文件），merge train 下零拒绝全落地、内容齐备、既有内容不回退——无收敛时 7 个会收 non-FF |
| T42 | `TestWorkflowConvergence` + internal/merge 单测（含 **git merge-file oracle**） | **收敛语义**（docs/design/workflow.md §2.2，rawPush 直钉 wire——真 git 客户端顺序性非 FF 在发送前自拒，收敛面=并发窗口）：不同文件分歧自动合并（两改俱在线性视图）；同文件同行冲突 → ng conflict 且票隔离（后续票继续）；冲突客户端 pull --rebase 停于真冲突、人工解决后续推（T18 路径在收敛链上成立）；blob diff3 与 git merge-file 在**重叠冲突形态**逐字节一致（含"同点双插=冲突"）；已知偏差钉在 T42b |
| T42b | `TestDeviationAdjacentEditsMergeCleanly` / `TestDeviationConflictSpansRenderWhole` / `TestBinaryIdentityShortcuts` | **merge 与 git 的已声明偏差**（对照 go-git merge-ort 移植的 xmerge 审计发现，git merge-file 现场验证双前提）：相邻行改动 banyan 干净叠加（git touch-fuse 判冲突——收敛引擎的有意宽姿势）；冲突块整段渲染（git ZEALOUS 精化缩小共同段>3 出 marker、≤3 行间隔融合成块——只影响 ng 诊断形态）；blob 同一性判定在 binary 检查**之前**（双侧相同二进制必须干净——回归修复，原实现误开冲突票） |
| T45（storage 单测） | `TestIdxLargeOffsetRoundTrip` / `TestGeometricGroups` / `TestIngestPackFileEquivalence` | **大仓三项**（docs/storage/storage.md §4/§5）：idx 64 位偏移编解码往返（大偏移表，单 pack 无 4GiB 限制）；几何分组表驱动（相邻比判据、大 pack 不入组、多组划分）；流式摄入等价（与内存路径同对象集/同可读性/idx 落盘/整包缓存保持冷）——含空 pack 合法形态（集成 T20/T43 的"推已有对象"路径回归覆盖） |
| T46 | `TestMemoryChainSmallBudget` + storage `TestSpoolVerifiedCopy`/`TestReceivePackRoundtrip`/`TestDiskCacheStreaming` | **小内存全链路**（docs/storage/storage.md §3.1/§4）：64MiB 软内存限制 + 堆峰值采样下，150MiB 不可压缩仓经 git→网关→3 存储节点 push（×3 副本）与 clone（v2+v0）全链路可用——峰值 13.5/12.4/9.9 MiB（修复前 783/611/512：节点 PUT 整包 ReadAll ×3、单对象提取整包 ReadFile、网关整包拉取 + closure 全图）；正确性双钉（head/对象数/工作树字节相等 + 服务端落盘 450MiB 门槛）；单测钉住滑动窗校验（sha1/sha256/坏 trailer/过短）、节点流式落盘（OID 同一性/短拷/坏包拒绝/临时文件无泄漏）、磁盘缓存流式写（abort 不留痕/complete 注册/GetCapped 闸门） |
| T46b（storage 单测） | `TestMemoizeBudgetEviction` / `TestRepackByteCapSplitsGroups` + HA T24-T26 回归 | **内存二轮审查**（docs/storage/storage.md §3.1/§5）：全库同款模式扫描后修五处——reconciler 副本修复流式化（源 VerifyPack 滑动窗 + 目标 PutPackStream 各自流，节点落地二次校验；HA 修复用例回归覆盖）、decoded-pack memoize 总量 LRU（超预算逐出最旧、packOrder 元数据不随逐出丢失、逐出包重读自愈）、repack 合并组字节上限（1 字节上限拆组零合并/默认正常合并双钉）+ spool 编码入册（消灭整包输出缓冲与二次解码）+ DeletePack 存储键修正（hashed 寻址旧分片删不到）、响应缓存超限免缓冲直发、非 clone 形态 verbatim 精确命中流式化（首字节前失败回退 ladder、中断即终态不追加第二个 pack） |
| T47 | `TestHooksReceiveLifecycle` / `TestHooksDecline` / `TestHooksDirectoryScope` / `TestHooksTimeoutAndSecret` + gitwire `TestMatchHooks`/`TestHookOIDZeroRendering` + storage `TestHookRegistrySQLite` | **服务端 hook（注册表 + HTTP 回调，docs/gateway/git-compat.md §7）**：注册走 `/repos/<repo>/hooks`（GET/PUT/DELETE 往返 + 注销后零回调）；pre-receive 拒绝整 push 且响应体首行回传客户端、update 只拒点名分支（同推其余落地）、post-receive 只收 applied 集；payload 钉 repo/directory/user/repo_bytes（实时）与零 OID 全零 hex；**目录作用域**：lib 级钩子拒 lib 子树推送且只见 lib 回调、doc 推送走仓库级放行（directory=doc）；超时 fail-closed（timed out 回传）；Secret 头送达。单测：匹配（最具体优先/全祖先/根只仓库级/事件过滤）与 sqlite 注册表（重启往返/upsert 覆盖/幂等删除/校验拒绝） |
| T48 | `TestFullCloneBudget` | **规模统计 + 全 clone 预算**（docs/operations/performance.md §6）：RPC `/size` 反映真实包字节（真随机内容——稀疏模式会 zlib 压穿）；预算武装后全 clone 拒（消息含 --depth 1 替代；v2 ERR 在 packfile 段头前——曾出 "bad band #69"）、`--depth 1` 过、既有克隆增量 pull 过、裸 OID lazy fetch 过；T22c `TestCLIWorkspaceDiscovery`：任意子目录向上发现工作区（status/commit 全工作区语义）、区外明确报错 exit 2 |
| T49（console/auth 单测） | console `TestChainResolution`/`TestEffectiveRoleInheritance`/`TestRenameStability`/`TestArchive`/`TestAccessGates`/`TestTokens`/`TestBuildSnapshot` + auth `TestSignedIdentity`/`TestUpstreamPolicyDecisions`/`TestUpstreamIdentify` | **产品控制面模型**（docs/web/console.md §3-§5）：前缀链解析（未注册中间段不贡献、深度空档不截断）、additive-max 继承（父 developer+子 maintainer=子锚点 maintainer、兄弟子树不受污染、子级授权不上溯）、rename 保 repo_uid、归档隐藏路由、四门（fetch/push/管成员/管项目）；签名身份头（篡改/错钥/±30s 窗外全拒）；UpstreamPolicy 决策（子树独占授权可锚点 fetch 根拒、protected 需 owner、个人分支角色买不到他人命名空间、公开仓匿名、未知仓 fail-closed） |
| T50 | `TestConsoleE2E` / `TestConsoleMemoryChain` | **console 端到端**（docs/web/console.md §6）：console+banyand 双进程拓扑（in-process 等价）真实 git 全链路——逻辑 URL 根/子树克隆推送、建仓经签名内部身份落到网关注册表、开发者推保护 main 拒+解保护过+复保护再拒（PATCH 即时生效=console 踢网关刷新快照）、reporter 子树推拒、锚点级 maintainer 授予后过且**根仓库对该用户仍 404**（子树独占=可执行方向）、public 匿名克隆、PAT 吊销立即失效、`/repos/{g}/{p}/size` 透传重映射；`TestConsoleMemoryChain`：64MiB 软限制下经 console 的 20MiB 不可压缩仓 push→repack→clone 全链路存活（代理贡献经直连网关隔离实验证零；残余增量为存储面 all-in-one 形态的服务路径分配，见 docs/web/console.md §7 注） |
| T94 | `TestT94ImportFromSiblingServer` / `TestT94ImportRejectsSSHRemotes` | **导入引擎自举**（internal/importer，docs/web/web-layer.md §4）：双栈自举——A 侧非 main 默认分支 + tag 的种子仓，B 侧直调 `importer.Fetch` 指向 A 的 smart-HTTP 面：refs 计数、symref 默认分支探测、pack **流式落盘**往返（clone 回比对 + fsck）；ssh remote 拒绝为哨兵错误 |
| T95 | `TestT95ImportAPI` | **导入 API 契约**（`POST /api/v1/imports`，docs/gateway/authorization.md §8d）：匿名 401、URL 末段推导目标名、显式 name、采用远端 HEAD、clone 回比对；二次导入 409；ssh remote 为 invalid argument（非上游失败）；不可达源 502 且**回滚后名字可复用** |
| T52 | `TestSubtreeViewCrossScopeIsolation` | **跨作用域隔离契约 + 混合作用域语义**（feat/review 上 D1 误诊复盘的沉淀）：A 推送→A 重物化→B 推送→B 重物化→A 不变——T5/T7/T8 从不覆盖"own-scope 推送后重物化兄弟视图"时序；断言用**精确文件名/内容**（计数断言无法区分 `token.go` 与 `services/auth/token.go`，正是 D1 漏网原因）；根视图每子树恰一次于自身路径；**混合作用域推送钉为内容**（客户端在作用域化克隆里创建锚点同名目录——xviewTree 的 mixed-scope 规则）：子树视图原样服务（`b/y.txt`+`y.txt` 同检出）、根织入嵌套于 `b/b/y.txt`、兄弟作用域零泄漏。D1 本体（"视图污染"）经 feat/review 2cea648 定位为评审测试 helper 的嵌套写入路径，引擎无缺陷 |
| T53 | `TestCloneShapedServing` | **clone 形态服务契约**（T50 复盘，docs/storage/storage.md §3 ⓪/④、docs/web/console.md §7）：① **gitlink 安全**——树条目 mode 160000 指向外部仓提交（本仓 pack 永不包含），类型导向枚举跳过之；旧探针式遍历对缺失对象 Raw 报错使**含子模块的仓库 clone 直接失败**（`remote did not send all necessary objects`，stash 对照实证）；② **零整包读**——计数后端（包装 LocalBackend）断言 exact-pack 命中的 clone 服务全程 `GetPack` 调用数为 0：枚举类型导向（blob 字节不触碰）、翻转仅集群线（`PrefersWholePack`），all-in-one 形态 20MiB 仓 clone 峰值 52.0MiB → 3.9MiB（T50 重测） |
| T54（ops 单测） | debug `TestOpsPlaneEndpoints` + config `TestDebugSection`/`TestDebugRatesRequireAddr` | **运维面端点与配置**（docs/gateway/service-api.md §5a、docs/operations/performance.md §7）：pprof 索引全家在列、heap profile 以 gzip'd protobuf 到达（magic 字节端到端）、`/debug/stats` JSON 携带 go_version/GOMEMLIMIT 读数、`/debug/vars` 应答；config——debug 节解析 + Describe 透出 + 采样率无 pprof_addr 严格拒绝 |
| T54 | `TestOpsPlaneObservesServing` | **服务路径计数器端到端**（docs/gateway/service-api.md §5a）：真 git 客户端驱动 fetch 阶梯——单 pack clone → `banyan_fetch_exact_pack_stream`、二次 push 碎片化后 clone → `banyan_fetch_encoded`（小仓回退 closure 阶梯，docs/storage/storage.md §3 ⓪' 阈值）、同 tip 重复 clone → `banyan_fetch_closure_cache`、repack 后 clone → 快路径回归、push → `banyan_push_accepted`；计数器均经 `/debug/vars` JSON 读取（expvar 进程级共享，包内测试串行故增量确定）；同平面 heap profile gzip 可消费 |
| T55 | `TestStreamingCloneMultiPack` | **多 pack clone 流式编码**（docs/storage/storage.md §3a，"进得去、出不来"关闭）：3 次 push × 12 个 2MiB 不可压缩 blob = 72MiB 跨 3 pack（不 repack 保碎片，精确命中必 miss）；64MiB 软限制下完整 clone **完成本身就是证明**（closure 图 ≥72MiB 常驻在 64MiB 限制下不可能）——峰值堆 12.3MiB（0.17×仓）；`banyan_fetch_streamed` 计数器断言走流式 rung；36 文件全在 + 每轮首 blob 与源逐字节一致；v0 线（`protocol.version=0` clone）同走流式 rung——finishFetch 的 v1 接线与 v2 等价 |
| T56（streaming 单测） | storage `TestStreamEncodePack*` / `TestPlacementsAndPackSizes` / `TestPrefersWholePackCapabilityBits` / `TestNodeClient*` + gitwire `TestEnumerateClosureTypeZoo` / `TestEnumerateClosureDepth` / `TestEnumerateClosureEquivalence` / `TestCountServeRungClassification` / `TestStreamingServe*` | **流式原语单测**（补齐 T50/T55 交付的单测空白；2026-10-27 二轮增补 deepen/超时）：storage——StreamEncodePack 往返（DecodePack + git index-pack/show-index oracle；空包 32 字节、负/越界计数拒绝、不支持类型拒绝、next 错误传播）、Placements 按 pack 分组 + 未知对象零 OID、CachedPackSize 空 OID 答 -1 不触后端（合成组 panic 回归）、PrefersWholePack 能力位（本地 false / 集群 true）、NodeClient 超时分层三契约（迟滞响应头在传输预算内失败/迟滞体随 ctx/持续流动的体远超头预算仍完整）；gitwire——enumerateClosure 以真 git 源仓做类型动物园（**tag 对象入服务集**且剥皮、**gitlink 排除**、恰 10 对象）、**深度语义**（depth=1 恰为完整 tip 快照、边界提交含树父链止步、unshallow=want 重发+嫁接下方祖先）、**8 组合与 closure() 集合等价**（clone 形态/各深度/浅客户端普通 fetch/deepen-with-haves/unshallow——等价性测试击落 ladder closure() 不跳 gitlink 的存量缺陷，与 T53 快路径对齐）、countServeRung 计数器映射（未知 rung 计 encoded）、tryStreamingServe 小仓回退（阈值判定前零字节）与响应缓存前置（want 集是缓存键） |
| T57 | `TestStreamingCloneTypeZoo` | **类型动物园流式 clone**（T55 的正确性对面）：2 push × 9 × 2MiB = 36MiB 跨 2 pack（超 32MiB 阈值必走流式），第二轮植入 exec/symlink（cacheinfo 120000）/gitlink（外部仓提交）+ annotated tag；clone 后 **tag 对象到达**（`cat-file -t v1` = tag、剥皮到源 tip）、`ls-files -s` 三种 mode（100755/120000/160000）完整、每轮首 blob 与源逐字节一致——类型导向枚举 + 流式编码器对 git 全部树条目形态的端到端契约 |
| T58 | `TestStreamingUnshallowMultiPack` | **deepen 形态流式 rung**（docs/storage/storage.md §3 ⓪''，"非 clone 形态 fetch 的 O(闭包)"收窄关闭）：3 push × 9 × 2MiB = 54MiB/3 pack（树随轮累积文件，tip 快照即跨全 3 pack）；depth=1 clone 流式（29 对象 = 单提交完整快照，深度语义正确）→ `--unshallow` 在 64MiB 软限制下完成（5 对象：want 重发 + 嫁接下方 2 提交 2 树，排除集=客户端已持有闭包）——**峰值堆 12.2MiB**（ladder 的驻留对象字节下限即超 32MiB）；rev-list 计数 3、fsck 干净、每轮首 blob 逐字节一致；`banyan_fetch_streamed` 两阶段各 +1（经 /debug/vars） |
| T59 | `TestImportSegmented` | **分批导入编排**（docs/storage/storage.md §4/§5、docs/client/cli.md §3.6，"进得去"闭环）：6 提交 × 40KiB 不可压缩 blob、`--segment-bytes 60KiB` 强制多段——import 退出 0、计划行与 `segment 2/` 断言；clone 后 rev-list 6、fsck 干净、6 blob 逐字节一致；`/repos/t59/size` 断言 **packs ≥ 2**（段=真实服务端碎片）；重跑=断点续传 no-op（"up to date"）；`--dry-run` 出计划不建仓（size 404）。实现坑由用例钉住：rev-parse 输出尾随换行、ls-remote 对不存在仓=全新导入、cat-file batch-check 整行即对象名（树/blob 行裁首字段） |
| T60 | `TestT60GitalyCompatSurface`（+ internal/gitaly 14 子测） | **gitaly 协议兼容面**（docs/integration/gitaly-compat.md 首切片 32/163 RPC）：真 git push 3 提交 + annotated/lightweight tag → gRPC 读回**逐项对拍 git 本地答案**——ListRefs ≡ ls-remote（HEAD+heads+tags，含 tag 对象 OID）、FindCommit ≡ rev-parse/log（subject/作者）、CommitIsAncestor 双向、ListCommits ≡ rev-list 全序、CountCommits ≡ rev-list --count、GetTreeEntries ≡ ls-tree 类型、GetBlob ≡ cat-file 逐字节、FindTag 双形态剥皮 ≡ ^{commit}；UpdateReferences 写 refs/heads/deploy 后 ls-remote 可见、DeleteRefs 后消失；**@hashed 寻址闭环**：CreateRepository(@hashed/6b/86/&lt;sha256&gt;.git) → 仓名=尾 oid → 经 gitwire push master → gRPC FindBranch 读回——三前端（gitwire/JSON/gRPC）同一对象平面；鉴权门（错/空 token → PermissionDenied）与 Unimplemented 契约（未实现 RPC 返回标准码而非连接失败）由单测钉住 |
| T61 | `TestT61GitalyTransportSurface` | **gitaly 传输面**（docs/integration/gitaly-compat.md 切片二，SmartHTTP 3/5 + SSH 1/3）：本地 **workhorse 模拟器**（git-HTTP 前门 → gRPC InfoRefsReceivePack/PostReceivePack 流 → 内建 gitwire mux 桥）承载**真 git push 全链路**——经 gRPC 桥推入的内容从原生 /r/ 面 clone+fsck 对拍（head 相等、文件逐字节、commit 计数）；InfoRefsUploadPack 广告与原生 HTTP 端点**逐字节相等**（桥不改变协议字节）；SSHReceivePack 批会话：stdin 泵入/exit-status 末条/广告含 tip（复用 gitwire.ServeSSHSession——serveGitExec 抽出的 io 流会话核心）；服务端流式 RPC 错误面：存储名错误在 Recv 时以 NotFound 冒出 |
| T62 | `TestT62GitalySidechannelTransport`（+ blob/repo 面单测组） | **sidechannel 复用传输**（docs/integration/gitaly-compat.md 切片三，fetch 通路解锁 → 45/163）：mini-workhorse 说全客户端半协议——TCP 连接首字节 "backchannel" 魔数 + yamux 会话（gRPC 走流 1）+ 回调注册表（"sidechannel" 魔数 + BE int64 id + "ok" 确认）；**真 git clone 经 PostUploadPackWithSidechannel 完成**（请求=非对称帧客户端→服务端 pkt-line ≤65531B/帧 + 裸 "0000" 帧模拟半关闭，响应=裸字节；克隆内容与源仓 head/文件逐字节/fsck 对拍）；SSHUploadPackWithSidechannel 交互会话（v0 最小 fetch：广告→want/flush/done→NAK+PACK）；**同一监听器 TransportCredentials 嗅探**：普通 gRPC 客户端照常（透传），无 sidechannel-id 的调用失败。协议细节由上游 internal/grpc/sidechannel 源码钉住（doc/sidechannel.md 只讲了 yamux 顶层） |
| T63 | `TestT63GitalyDiffSurface` | **diff 与路径域历史面**（docs/integration/gitaly-compat.md 切片四，59/163）：真 git 三提交仓（改/增/删全形态）——**RawDiff 与 `git diff` 逐字节一致**（diff --git 头/index 缩写 OID 行/new file·deleted file mode/@ 计数=1 省略/dev-null 语义全对齐；行级引擎=go-diff 行模式+DiffCharsToLines 回解码）；CommitDelta ≡ --name-status；DiffStats ≡ --numstat；CommitStats ≡ show --numstat 求和；FindCommits pathspec ≡ log --format=%H -- path；LastCommitForPath ≡ log -1；CommitsByMessage 子串；TreeEntry 内容流；GetPatchID 稳定；FindChangedPaths Tree 形态 ≡ 增删集 |
| T64 | `TestT64GitalyOperationsSurface`（+ operations_test.go 单测组） | **OperationService ref 面 + web 提交流 + LFS pointer 三件套**（docs/integration/gitaly-compat.md 切片五，69/163）：真 git push 含手工 LFS pointer 与"tip 前已删"文件的仓库——ListLFSPointers 命中 pointer（FileOid/FileSize/Data 与 cat-file 逐项一致，历史语义=rev-list --objects）；GetLFSPointers 混型对象（pointer/tree/commit OID）静默过滤=上游测试同构；分支经 gRPC 建/删后 ls-remote OID 一致（错旧值 CAS=FailedPrecondition）；annotated tag 经 gRPC 创建后 fetch 回源仓 `cat-file -p` 全字段核对（object/type/tag/tagger 时间戳/消息）+ `^{commit}` 剥皮；UserFFBranch 前后 ls-remote 对拍（非 FF="not fast forward"）；**UserCommitFiles 全动作流**（CREATE/UPDATE/MOVE InferContent/DELETE）产物 git fetch 回源仓，`git show` 核对 author/committer/消息体/树内容（MOVE 保留 pointer 原文、DELETE 生效）。ListBlobs/ListAllBlobs 同切片升为全历史/全对象库枚举（修复原 tip 快照差异，单测断言同步） |
| T65 | `TestT65GitalyMergeSurface`（+ operations_merge_test.go 单测组） | **用户面合并五件套**（docs/integration/gitaly-compat.md 切片六，74/163）：**git 自家合并机械作 oracle**——UserMergeBranch 干净合并的树与 `git merge-tree --write-tree` 逐字节相等（fetch 回源 rev-parse ^{tree} 对拍）；冲突判定与 git 一致（同文件双改两侧，ConflictingFiles 精确到 alpha.txt）；UserMergeToRef 两父合并提交落 refs/merge-requests/* 原样广告（`--format=%P` 验证）；UserSquash 树=end 提交树（CommitDelta 空集）；UserCherryPick 与干净克隆中 `git cherry-pick` 树相等+源作者保留+二次拾取 ChangesAlreadyApplied；UserRevert 与 `git revert` 树相等+作者=操作用户；合并后 FF 双视图收敛（ls-remote+fetch 复核）。**双视图规则**（gitaly 面统一约定）：读/期望=广告（织入）OID、写=raw timeline CAS——合并提交广告侧线性化为树全同孪生（诚实差异登记 docs/integration/gitaly-compat.md §6）；cherry/revert/commit 落广告 tip 使两视图收敛。引擎=internal/merge 3-way 经合成层（FlushSynthBatch 持久化点先于任何 ref CAS） |
| T66 | `TestT66GitalyRebaseSurface`（+ operations_rebase_test.go 单测组） | **rebase 系 + 补丁应用 + 子模块更新**（docs/integration/gitaly-compat.md 切片七，78/163——**OperationService 16/16 收满**）：**git rebase / format-patch+am 作 oracle**——UserRebaseToRef 与 UserRebaseConfirmable 的重放树均与本地 `git rebase` 逐字节相等（log 逐条核对 subject/committer/链形）；UserApplyPatch 吃真 `git format-patch` 双补丁 mbox，树与本地 `git am` 逐字节相等（作者/主题来自邮件头保留）；冲突补丁 "Patch failed at" FailedPrecondition 干净失败；UserUpdateSubmodule 前后 ls-tree 核对 160000 gitlink 与目标 OID（already-at/非子模块路径拒）。**纯 Go rebase 引擎**：todo=可达闭包差集+丢 merge+patch 等价剔除（treeDiff 变更集 SHA-1 签名近似 --cherry-pick）+最老优先；逐提交 3-way、变空丢弃；**根分支非快进 CAS 触发 ResetWoven**（rebase 换链使织入追加锚失效——复用 force-push 视图重建，T66 击落）。**纯 Go mbox 解析+统一 diff 应用器**（patchapply.go：From-行切分/RFC-822 头/[PATCH] 剥除/hunk 上下文匹配+偏移搜索；二进制与 rename/copy 显式拒绝）。service.CreateCommit 对 160000 gitlink 免 blob 校验 |
| T67 | `TestT67GitalyMaintenanceSurface`（+ coverage_gaps/repository_maintenance 单测组） | **Repo 维护面**（docs/integration/gitaly-compat.md 切片八，89/163——RepositoryService 19/42）：**git archive / show-ref / diff --raw 作 oracle**——GetArchive 的 tar 输出与 `git archive --format=tar --prefix=…` **逐字节相等**（双提交+嵌套目录+update-index --chmod=+x 夹具，pax_global_header comment/mode/uname=root/tar_umask=002/10240B 记录全对齐）；tar.gz 解包 payload 与 git tar 逐字节相等（gzip 头无 name/mtime = gzip -n）；zip 条目逐项对拍（名/size/mtime/内容/目录条目）；CalculateChecksum 从 `git show-ref --head` 行集复算相等（逐行 SHA-1 XOR，上游同构）；Fsck 健康仓净；GetRawChanges 的 blob OID/size/mode 与 rev-parse 一致；RemoveRepository 后 ls-remote 失败、幂等重删成功。**存储层删除路径**：MetaStore.DeleteRepo 六表事务删行 + shard 目录回收（字节先行注册后删）+ 节点/meta 服务对称端点；RenameRepo 丢 hooks 行修复。**测试完整性审计**：16 个已实现无调用 RPC 补齐（枚举/读面），击落三枚 bug——patchStats 无尾换行漏计（--numstat 语义）、revision `~N` 把 ~1 读成 11 跳、RenameRepo 丢 hooks |
| T68 | `TestT68GitalyBundleSurface` | **bundle 面**（docs/integration/gitaly-compat.md 切片九）：**git bundle 双向 oracle**——banyan CreateBundle 产出过 `git bundle verify`（annotated tag+分支广告），`git clone` 从 bundle 克隆内容/文件逐字节对拍（头部含尾部 HEAD 行=git bundle create --all 同形——缺它 clone 落在空 master）；CreateBundleFromRefList 按 patterns 过滤（tags 排除）；**反向**：`git bundle create --all` 产物经 FetchBundle 流入 banyan 仓、CreateRepositoryFromBundle 建仓+种子一次完成（HEAD→默认分支），clone 对拍 subject/文件；坏载荷 InvalidArgument |
| T69 | `TestT69GitalyFetchSurface` | **远端面**（docs/integration/gitaly-compat.md 切片九）：FetchSourceBranch 仓对仓拷贝——目标 ref 广告=源 raw OID（新 ref 原样）；**FetchRemote 以 go-git transport 作 smart-HTTP 客户端拉 banyan 自身 /r/ 面**（服务端↔客户端跨实现互操作）；force 覆盖已有分支触发织入重建孪生（双视图规则）——断言内容级对拍；CreateRepositoryFromURL 建仓+拉取一次完成 |
| T70 | `TestT70GitalyClosingSurfaces`（+ commit_read_extras/rangediff/ref_extras 单测组） | **收尾三面：conflicts/fast-export/mirror/fork**（docs/integration/gitaly-compat.md 切片十，123/156 实现+33 定稿）：ListConflictFiles 标记体与 git 冲突形态一致（<<<<<<<）；ResolveConflicts 落 merged 分支（content 模式解决+theirs 自动合并，clone 核对）；**FastExport 流经 git fast-import 完整往返**（blob 必须先于所属 commit——fast-import 硬约束，首版交错被 oracle 击落）；UpdateRemoteMirror 推 banyan 自身 smart-HTTP 面（go-git receive-pack 客户端）；CreateFork 全 ref 拷贝（annotated tag 先落对象再 peel 走闭包）；FindRemoteRepository/RootRef 探测。**读面收尾**：RawBlame（首父链行级归属+porcelain 形态）/RangeDiff（patch-id 配对）/签名剥离/tag 正文/分支分页/语言份额/分叉计数——AdvertiseBundleURI 空广告同上游未配置形 |
| T71 | `TestT71GitalyHistoryRewrite`（+ cleanup_service_test 单测组） | **历史重写**（docs/integration/gitaly-compat.md 切片十一）：RewriteHistory strip blob+literal/regex redaction，oracle=clone——`rev-list --all` 只含新链（**disjoint：force 改写会织入旧链为祖先，删时间线+ResetWoven 重织视图击落**）；被剥路径全历史消失、secret→`***REMOVED***` 逐 rev 断言；BFG map 流删 keep-around 内部 ref、分支 ref 幸存 |
| T72 | `TestT72GitalyBackupSurface` | **服务端备份**（切片十一）：本地 sink 键位与上游 gocloud 同形（NNN.bundle/.refs/.custom_hooks.tar+manifests/*.toml+backup_ids/）；删仓→RestoreRepository→clone 与源仓逐 OID 对拍（含 tag）；hooks tarball 经 sink 往返；**增量**：run-2 manifest 堆叠 002 步+previous_ref_path，恢复按序重放全部 steps；WriteBackupID/ReadLatestBackupID 按 mtime 排序；未配置 sink=FailedPrecondition |
| T73 | `TestT73GitalySnapshotSurface` | **快照/复制/bundle-uri**（切片十一）：GetSnapshot=真 bare 仓 TAR，**手写 idx v2 由 git clone+fsck --strict 验证**（首版四连败：pack 名须取尾部 checksum、idx 校验和同源、packed-refs 剥离行=`^<oid>` 独立行、bare 骨架需 refs/ 目录条目）；CreateRepositoryFromSnapshot 经 httptest 往返（Authorization 断言）；ReplicateRepository 替换已有仓后 clone 对拍（分支/tag/HEAD）；GenerateBundleURI 产物过 `git bundle verify` |
| T74 | `TestT74GitalyObjectPool` | **对象池+内部元数据**（切片十一）：@pools/&lt;h2&gt;/&lt;h2&gt;/&lt;hex64&gt; 建池（二次=FailedPrecondition、非池路径=InvalidArgument）；**物化即 alternates**——空 fork link 后解析池对象；GetObjectPool/ScanPoolMetadata/ListPoolMetadata/ListPoolUpstreams/WalkRepos 注册表对拍；FetchIntoObjectPool 扩池；DisconnectGitAlternates 后物化对象仍在；DeletePool 连带成员解链 |
| T75 | `TestT75GitalyHookService` | **hook 面**（切片十一）：pre-receive/update **fail-closed**——httptest 回调 decline=exit 1+stderr 首行、allow=exit 0（payload 的 event/repo/user/updates 断言）；post-receive/ReceiveReportHook best-effort 投递计数；事务 hook 恒 0；PackObjectsHookWithSidechannel=FailedPrecondition 门 |
| T76 | `TestT76GitalyClosingPair` | **收尾双面**（切片十一）：CheckBlobsGenerated=go-enry 启发（package-lock.json=generated、main.go=not，`HEAD:<path>` 定界 revision）；SSHUploadArchive wire——band-1 重组载荷与 `git archive --format=tar HEAD` **逐字节相等**、坏参数=NACK+exit 1（首版 ACK+sideband 混帧被击落） |
| T77 | storage `TestMetaStoreConformance` + `TestRenameFenceRejectsMutations`（共享 metaOpeners 矩阵）+ config `TestMetaPostureDefaults` | **元数据姿态**（docs/gateway/authorization.md §0，2026-11-05）：conformance 与围栏两组电池统一跑 **mem/sqlite/mysql/postgres 四后端矩阵**（env 门控 DSN：`BANYAN_TEST_MYSQL_DSN`/`BANYAN_TEST_POSTGRES_DSN`，本地 docker mysql:8/postgres:16）——真服务器复验击落两处 DDL 漂移（`key` 是 MySQL 保留字→列改名 blob_key；围栏表 `frm TEXT PRIMARY KEY`→VARCHAR(190)，MySQL 不许 TEXT 进键）；dropAllTables 补全至全部 10 表（漏表让共享库跑序依赖）；config 姿态矩阵钉住生产缺省 postgres、mem 生产拒、gateway/storage 不持有元数据、缺 DSN 报错带示例 |
| T78 | `TestWeaveRederivationConsistency` | **织入重推导一致性 + 重建字节稳定**（docs/design/data-model.md §13，2026-11-05 登记 / 2026-11-07 溯源头修复后断言升级）：双网关共享存储——网关 B 全冷（新 ObjectService：synth 表/解码 memo/位置缓存全空 + 新 view.Service rootCache）从时间线 tips 独立重推导，广告 tip 必须**逐字节一致**；夹具全员钉同一 committer 秒（织入 (when, anchor, oid) 平局裁决的最锐边）——任何挂钟输入进合成或非确定平局裁决都会在此分叉 OID。②ResetWoven 全量重建升级双断言：**重建后广告 tip == reset 前逐字节**（脊柱经 `banyan-woven-from` 溯源恢复折叠进度、逐字复现）+ **重建视图 subject 恰一次**（原已知问题——脊柱内嵌嫁接被再织一遍、subject 两次+tip 漂移——由溯源头闭合，见 T88）。仅保留 marker 的修法曾被击落并永久拒绝：marker 假设"已折叠前缀活在 /.woven 输出链"，而 reset 恰好抹掉输出锚——保留 marker 会让未回退源静默丢失（T44 nested 内容蒸发） |
| T79 | `TestTokenAuthE2E` | **PAT 端到端**（docs/gateway/authorization.md §8，2026-11-06）：RPC 铸造→Basic(user,token) 真 git 取/推——read scope 可取**永不能推**（上限不是旁路）、write 推过、属主错配即拒、吊销即刻 401、清单只回哈希永不回明文 |
| T80 | `TestRPCFineGrainedAuthorization` | **RPC 分级授权**（docs/gateway/authorization.md §8b）：管理端点（repack/cluster/force-push）开发者 403·管理员 200·admin token 200；内容写（POST refs/建仓）read token 403——AdminChecker 可选扩展缝 + 与 wire 同一 CanPush |
| T81 | `TestDiffBlameRPC` | **diff/blame RPC 字节 oracle**（docs/gateway/service-api.md §3，2026-11-06）：`GET /api/v1/diff` 输出 == `git diff`（含 path 目录前缀过滤 == `git diff -- lib`）；`GET /api/v1/blame` == `git blame --line-porcelain`——顺带把引擎的 porcelain 格式钉死到 git 逐字节（boundary/previous 头、同源行分组、组长度只在组首） |
| T82 | `TestGzipFetchBody` | **gzip rpc POST 体**（docs/operations/testing.md 档案破案，2026-11-06）：同一 v2 fetch 终轮请求 plain 与 gzip 各发一次——gzip 体必须同样走到 packfile 节并回 PACK 字节（修复前：空响应，git 2.50 expected acknowledgments） |
| T83 | `TestT83GitLabCEE2E`（opt-in：`BANYAN_GITLAB_URL`+`BANYAN_GITLAB_TOKEN`） | **GitLab-CE 活体 e2e**（docs/integration/gitaly-compat.md §8，2026-11-07，demo/gitlab 栈）：对运行中的真 GitLab——建项目（CreateRepository）、HTTP push（workhorse→PostReceivePack）、读面核对（默认支全名解析/GetTreeEntries path "."/ListCommits/GetBlob/GetArchive+include_lfs_blobs）。本轮活体实测揪出的六处协议缝各有近端回归：v2 HMAC bearer（`TestGitalyAuthGate` 四态：新鲜过/过期拒/未来拒/错签拒）、FindDefaultBranchName 全限定名+上游启发式（单测+T60 空仓/head_only/建仓后三态）、ListRefs 前缀 pattern（`refs/heads/` 与 `refs/heads` 两形态）、tree path "."（单测）、FindTag 结构化错 detail（T60 断言 FindTagError 骑在 status 上）、archive LFS 门（pointer 树拒/无 pointer 树放行）。CI 无 demo 环境即跳过 |
| T84 | `TestT84SubtreeProjectionRefs` | **子树投影 ref**（docs/integration/gitlab-integration.md §7，2026-11-07）：默认分支的每个顶层目录以 `refs/banyan/tree/<dir>` 出现在广告（含 v2 ls-refs/want 许可/T60 的 gitaly ListRefs 三面一致）；投影 OID ≡ `/tree/<dir>` 面的视图；fetch refspec 物化（`git clone -b` 只认分支/标签短名——**git 客户端约束**，配方是 clone+fetch）；推回投影 ref 走子树写路径（目录锚定+向上装配，根 tip 前进）；混合推送（根分支+投影 ref 同包）各按命名空间路由；删除/嵌套路径的拒绝与 /tree/ 面同语义。全部经 GitLab URL 活体验证（ls-remote/fetch/push→main 前进）。连带：repack 夹具加大 delta 赢面（投影链进入可达闭包是诚实的字节数变化）、Engines facade 补内部 gitwire（投影前 facade 的 listRefs 裸指针崩溃被 T81 击落） |
| T85 | `TestT85GitLabPostReceiveAdapter` | **post-receive → internal API 适配器**（docs/integration/gitlab-integration.md §2，2026-11-07）：带平台身份的 push（gitaly 桥以 X-Banyan-Gitlab-* 头穿线——测试用 `git -c http.extraHeader` 等价注入）触发 `/internal/pre_receive`（引用计数配对）+ `/internal/post_receive`（changes 行式载荷、identifier/user_id/protocol）；JWT 以共享 secret HS256 签名、iss=gitlab-shell、带 exp（fake 复刻 Rails 的 verify_api_request 检查）；无身份的普通 push 适配器静默。live 实证（demo 栈）：经 GitLab 的 push 后**零手动缓存过期**读面即刻出新提交、internal API 200、PostReceiveWorker 完成 |
| T86 | `TestT86GitLabAllowedGate` | **`/allowed` 策略门**（docs/integration/gitlab-integration.md §2，2026-11-07）：ref 移动前调 `/internal/allowed`（真 gitaly 的 pre-receive 钩子同款——`GitAccess.check(action, changes)`，保护分支为首的 ref 级策略在 Rails）；fake 复刻契约：拒绝=401+`{status:false,message}` 且消息透传客户端、ref 不动、**计数对不触发**（上游顺序：策略先于 `/pre_receive`）、Rails 不可达/答非所问 fail-closed、放行后全链（allowed→pre→post）走通、新建分支的 changes 行携带**全零 oldrev**（ChangesList 按空白切分——空字段使 newrev 吞掉 ref 名，活体第八缝①）；JWT 签名断言与 T85 同源。live 矩阵（main 保护）：developer→main 拒（Rails 原话）、developer→新分支过、root→main 过、`pushed new` 事件解析正确（缝①同修 post_receive）。缝②（ListBlobs/ListLFSPointers 的 rev-list 参数语法 `--not`/`--all`）由 LfsCheck 路径活体击出并修复 |
| T87 | `TestT87GitLabPushOptionsAndSizeLimit` + T62 重写子测试 | **push options 端到端 + 推送大小上限**（docs/integration/gitlab-integration.md §4，2026-11-07）：① receive-pack 广告 `push-options`、`git push -o` 的选项段解析（命令表 flush 后 pkt-line+flush）、`post_receive.push_options` 载荷逐字上报（fake 断言 `["merge_request.create","ci.skip"]`）；无 -o 的推送不带该键。② `/allowed` verdict `git_config_options.receive.maxInputSize` 强制：512B 上限下随机大包被拒（**夹具必须不可压**——pack 是 deflate，同字节串能缩到上限以下，本测试的教训）、ref 不动、上限放宽即过。③ T62 的 SSH sidechannel 子测试重写为 gitlab-shell 形态（ProxyPktLine）：请求/响应并发拷贝、git 会话字节整体作为 band1 载荷（sideband 是透明管道——裸等 git 级 flush 会死锁）、band 校验、EOF 终结（flush-pkt=invalid sideband packet）——活体第九缝的回归钉。live：MR-from-push 实开 MR #1、设 1MB 推 2MB 被拒、SSH clone+fsck 全净 |
| T88 | `TestWovenProvenanceChainedAdoption` | **织入溯源头 + 链式果纳重建**（docs/design/data-model.md §13，2026-11-07）：`banyan-woven-from` 提交编码级溯源——嫁接/追加合成物声明其源提交（源空间），kindFull 重折保留条目自身溯源、`SubtreeSplit` 带入投影；ResetWoven 重建以**两条款**跳过（条目被认领 / 条目是已嵌入源的折叠——fold 输出的 diff 恰为其源变更），跳过评估先于自身 claim 收集（防自引用）、被跳过者仍上缴溯源。夹具钉**链式果纳**最锐形态（调试实证：n 推送先于 lib 时间线存在时根织入直折 n1，lib 视图输出 g'(n1) 后被 lib 客户端果纳推回——重建时脊柱只携带 wf={n1,lc}，若无条款②则 g'(n1) 重新嫁接并**回退** lc 的 a.txt 编辑）：ResetWoven 后根/lib 两视图——广告 tip 逐字节不漂、subject 恰一次、内容与 fsck 全对 |
| T89 | `TestT89SubtreesListing` | **子树管理清单 API**（docs/gateway/authorization.md §8c，2026-11-08）：`GET /api/v1/subtrees?repo=`——`/.woven` 内部族与 aux ref（tag）被过滤、根锚（仓库自身）不入清单、按锚分组、支线按名排序、每支线 head 带**最近提交**（subject/author/when，经 lookup.Commit 装饰）、head ≡ 子树时间线本头（/tree/ 语义非织入投影）、`projected` 标记 ≡ 投影 ref 集（顶层目录、T84 同推导）；未知仓 404 信封。夹具注意：**子树视图按支线解析**——往 `/tree/<dir>` 推新分支前该分支须先在根存在（否则 "subtree view unavailable"） |
| T90 | `TestT90SubtreeAccessRules` | **运行时子树访问规则 + GitLabSSO**（docs/gateway/authorization.md §8c，2026-11-08）：假 GitLab（/api/v4/user：root=管理员/alice/bob）+ `SubtreeRulePolicy(GitLabSSO)` 栈——Bearer 与 Basic(token) 双凭证形态；root（实例管理员）PUT 规则 `/secret→[alice]`、非管理员 403；bob 经 `/r/<repo>.git/tree/secret` clone **拒**、alice 过；**根 clone 不受影响**（边界断言：深规则不能遮浅锚——根锚服务全闭包，docs/web/console.md 同构）；push 同门（alice 血统分支经 bob 凭证拒、经 alice 凭证落）；删规则即恢复开放；清单往返 users 排序；相对路径 400 |
| T91 | `TestCLISnapshot` | **CLI 快照 checkpoint/restore**（docs/client/cli.md §3.7，2026-11-08）：干净树 no-op；M/D/?? 三态存档**不动工作树**（非 stash 式回退）；ls 带 message；脏树 apply 拒（exit 1）；`--force` 逐字节还原（脏字节取镜像、删除文件自缓存复活、untracked 归位，status 回到快照脏集）；落地快照态后**清空缓存**再干净树 apply——干净文件经服务端回填 + 基漂移提示（manifest 拨回旧视图）；drop 后 ls 空、apply 缺名 exit 2 |
| T92 | `TestCLIStack` | **CLI 栈式变更**（docs/client/cli.md §3.8，2026-11-08）：路径分片 push 只回退选中路径（untracked 捕获+删除）；全量 push 后树净；ls oldest-first；pop 顶条写回离栈；`stack commit` 逐条落台**写回工作树**（真 git 克隆交叉验证：subject 顺序、untracked 落成 tracked、fsck、栈 blob 库回收）；peer 推动头后 commit→exit 1 条目原样保留→pull→重试全落地（路径级重放无需 rebase） |
| T93 | `TestSyncSubtree` | **子树→根显式对齐 sync**（docs/design/data-model.md §12 / docs/client/cli.md §3.9，2026-11-08）：真分歧夹具=TOCTOU 竞态确定性模拟（对象搭 aux 分支推送入库+root 时间线直 CAS，夹具注释说明守卫无法顺序构造该状态）→ sync 重放 1 提交+pull 物化两侧；**Base 推进到同步时刻根头**；根视图 git 克隆交叉验证（内容两侧齐全、每 subject 恰一次、fsck）——/.woven 重置防旧折痕重复是该测试钉住的修复；吸收孪生（peer pull 带走本地内容）→ uptodate；同文件双改 → exit 1 具名路径、时间线头与 Base 不动；merge 链 → exit 2（SYNC_UNSUPPORTED）；根工作区 → exit 2 |

| T43 | `TestForcePushRevivalRejected` / `…Recovery` / `TestRootForcePushViewMonotonic` | **子树 force push 回滚守卫**（docs/gateway/git-compat.md §6a）：wire 无 force 信号、old 恒=广告值——回滚/重写丢他人内容的 force push 以连续 push 形态到达；四种形态（adopt/收敛 × 纯回滚/回滚+己方提交）真实 git 客户端全拒绝（non-fast-forward: would rewind，旧代码上实证全部 forced-update 落地）；拒后 pull --rebase 恢复路线畅通；根仓库 force push 放行（git 原生 verbatim）但织入视图单调——root-local 内容与目录时间线内容均不回魂 |
| T44 | `TestForcePushOptIn` | **仓库级 force push 开关 + 织入重建**（docs/gateway/git-compat.md §6a）：PUT /api/v1/repos/&lt;repo&gt;/force-push 运行时生效——关（默认）回滚仍拒（T43 语义不回归）；开后回滚链落地（forced update）且 `/.woven` 家族重置全量重织：lib 时间线回滚（a.txt 回退）而**下层 lib/nested 独立时间线内容保留**（时间线边界=回滚边界）、根视图历史**重建**（被回滚提交的织入体从 log 消失、nested 织入保留）、回滚后正常 push 即刻恢复；开关再关守卫重新武装 |
| T33 | `TestFetchTreesBinary` | **fetch-trees 二进制帧**（docs/gateway/service-api.md §3.3、docs/operations/performance.md P1-2）：bin/JSON 语义全等（树与 blob 集合逐字节一致）、二进制小于 JSON、真实 CLI clone 走 bin 路径物化 |
| T32 | storage `TestExtractObjectRoundTrip`/`TestLocalGetObjectViaDisk` + T23-T26 回归 | **P1-1 单对象解压**（docs/operations/performance.md）：idx 偏移 + delta 基链递归解压（含 ofs/ref 两型）；集群 wire 从整包降为单对象（/internal/object 端点）；修复 idx oid→offset 在 delta pack 上的按位配对错位（Parser Observer 为权威源），idx crc 段补真值 |
| T20 | `TestTagsAndAuxRefs` | **refs 命名空间泛化**：annotated/lightweight tag、tag 对象闭包、自定义 refs/<ns>、tag 删除与非 FF 拒绝、浅克隆 tag 剥离 |
| T21 | `TestRepackFragmentMerge` / `TestRepackRestartSurvival` | **repack**：碎片合并+delta（字节收缩、对象保全、ExactPackHit 恢复）、SQLite 重启后存活（UPSERT 重指向回归） |
| T22 | `TestCLIThinWorkspace` / `TestCLISubtreeWorkspace` | **薄工作区 CLI**（docs/client/cli.md/10）：最小化 clone（无 .git、无提交闭包）、离线 status、服务端 changeset commit 经**真 git 客户端交叉验证**（fresh clone + fsck + subject）、log/cat 零下载、pull=服务端 tree-diff、base-moved 冲突退出码 1→pull→重提、子树工作区 commit 织入根视图 |
| T23 | `TestHAClusterScatteredPlacement` | **散布放置**（docs/storage/ha.md §3）：每 pack 持有集=3 个不同节点、任一持有者可服务且校验和通过、登记即事实（读不依赖放置函数）、增量 push 新随机放置、cluster/nodes 端点 |
| T24 | `TestHANodeFailureAndRecovery` | **节点宕故障转移与恢复**（docs/storage/ha.md §8）：kill 后新写/克隆照常（W=2 掩蔽死节点）、健康表 dead 判定、reconciler 补齐回 3 副本且 dead 节点移出持有集、恢复后克隆仍正确 |
| T25 | `TestHAPlacementDynamic` | **动态副本**（docs/storage/ha.md §6）：R 3→2 触发剪枝（先登记后删除）、2→3 触发补齐；剪枝后读正常 |
| T26 | `TestHARefsRestart` | **refs 重启 RPO=0**（docs/storage/ha.md §4）：SQLite 句柄释放+同址重启、心跳重新注册、重启前提交零丢失、重启后继续 push/clone |

## 运行

```bash
go test ./integration/... -v        # 全量（子包并行；墙钟≈最慢子包，需 PATH 中有 git）
go test ./integration/subtree/ -run TestMultiUserSameFileCollaboration -v   # 单用例（-run 前先选子包）
go test ./...                       # 单元 + 集成
```

无 git CLI 的环境自动跳过（`support.NeedGit` → `t.Skip`）。拆包后每个
子包是独立测试进程：expvar（/debug/vars）注册表按进程隔离，包内测试
仍串行——T54 的增量断言语义不变且更干净。

## 新增功能的测试约定

新能力落地时同步新增集成用例（命名 `Test<场景>`，种子用 `support.SeedRepo`），
文件**落到上表对应功能域的子包**（跨包子包共享基建走 `integration/support`），
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

单元侧：`storage.TestGenerationDiamond`（代数精确/污染语义 + 树反向索引，docs/storage/indexing-packing.md §2）。

**存储一致性修复回归组（2026-11-01，评审四缺口）**：`TestRepackUnreadableFragmentSurvives`（repack 跳过读失败分片但不得 drop 其注册——数据丢失窗口关闭）、`TestUserMergeDurabilityGate`（failPutBackend 注入：synth flush 失败→merge 硬失败且 ref 不动——"durability before any ref CAS"从注释变成闸门）、`TestHACorruptReplicaFallback`（HA 三节点同长字节翻转腐坏两副本：GetPack 尾部校验跳坏 holder、OpenPack verbatim 经校验落地回放、无缓存 gateway 也走校验临时文件——clone+fsck oracle）；伴 B 修复（LocalShardStore 全写路径 tmp+rename 原子落盘）由既有 storage/integration 全量回归覆盖。

**一致性遗留双修复回归组（2026-11-04，评审 F/D 两项）**：
`TestRenameFenceRejectsMutations`（memory+sqlite 双实现：围栏期全变更面
ErrRepoRenaming、读不受挡、同 (from,to) 幂等重装/异重叠拒绝、Abort 解
围栏、翻转迁移+解围栏、墓碑拒绝死名再建、DeleteRepo 回收墓碑）；
`TestHARenameWriteBarrier`（HA 确定性交错：围栏期 push 被拒且注册表零
残留，生产路径完成 rename 后旧数据新名完整可读，墓碑挡死名复活）；
`TestHARenameVsPushRace`（5 轮并发不变量：ACK 的 push 必在新名完整可
见——竞态版首跑击落"翻转后 auto-create 复活死名"形态，墓碑语义由它
逼出）；`TestHAFleetSweepOrphans`（孤儿 pack/去持有化残留/整键幽灵/
tmp 残片全清 + grace 内新文件保留 + dry-run 只计数 + 活数据 clone+fsck
无损）；`TestHAFleetSweepHashedDeletedRepo`（hashed 删除仓库的不可变
键三节点整键回收，存活仓库键不动可 clone——docs/storage/ha.md §8.7）。

**生产可用 P0 回归组（2026-11-05，docs/project/roadmap.md 生产主轴）**：
`TestProductionRequiresAuth`（config 姿态矩阵：双客户端角色无认证拒启、
gitaly 无 token 拒启、dev 显式放开、refs/storage 内部面不受限）；
`TestServeHTTPDrainsInFlight`（drain 契约：ctx 取消后在途请求跑完才返
回、ErrServerClosed 吞掉、listener 关闭）；`internal/metrics` 单测组
（Prometheus 文本格式 HELP/TYPE/标签转义/累积 bucket/并发安全/幂等声明）；
`TestMetricsObserveGitTraffic`（真 git push+clone 落进进程注册表——请求
计数/延迟/ingest 字节**差值**断言（registry 进程级共享，禁绝对值），
/metrics 端点 exposition 格式）；`TestMetricsCASConflictCounter`（陈旧
expected 的 CAS 计一次冲突——计数点在两实现源头，monolith 拓扑同样可
测）。

## CI push-run 抖动档案（2026-11-05，未归因·仪器就位）

当日 7 次 push-run 失败（next 修错连推 ×5 + main 两次合入）全数取证为
**同一形态**：`--- FAIL: TestAuthorizationWire`，失败耗时 **0.26–0.46s**
（用例正常 1.5s+——死在最先几个 git 操作：种子 push/匿名 clone/首次
克隆附近），套件其余用例全绿、包完整跑完。仪器侧发现并修复 v2 注解
过滤器的自噬 bug（`^--` 吃掉 `--- FAIL` 行、`\s` 吃掉全部缩进断言行），
v3 起断言文本直达注解。

**归因证据链**：同一棵树（e0671b8≡41b6aed）在 03:24–09:47 窗口内两败，
其后同树连绿 23 轮（含 15 轮空提交探针 + workflow_dispatch）；恒定失败率
无法产生该分布（0.12²×0.88²³≈0.07%）——失败概率随时间窗涨落而非随代码，
指向 runner 侧瞬态（启动期基础设施/磁盘/网络），非代码竞态。本地全谱
锤击零复现：TAW 隔离 115 轮（暖）+ 60 轮（每轮全新进程，对齐 CI 冷启动）、
`-race` 15 轮（服务端数据竞态排除）、integration×storage 并发碰撞
（runner 保真布局：共享 2 CPU 预算 + 真 mysql:8/postgres:16，DB 就绪门控）
多轮全绿、全量 `go test ./...` ×7。

**复现/取证工具链**（`F:\Workspace\test\`）：`ci-repro/Dockerfile`
（ubuntu:26.04 + apt golang + gcc，GOTOOLCHAIN=auto——runner 等价环境）；
`probe2.sh`（push-run 探针：`?event=push&per_page=1` 盯本尊，败即拉 v3
注解）。再发作时：一次失败注解直接点名断言行——git 报错文本=基础设施
实锤，断言不匹配=代码 bug，即刻归因修复。

**档案承诺兑现（2026-11-06）**：v3 仪器首击即点名——`TestAuthorizationWire`
（0.27s）auth_test.go:112 **"按 OID 取隐藏分支对象应被拒"却成功**：bob 以
raw-OID fetch 拿到了 alice 隐藏分支 tip（want 校验被放行）。该 flake 首次
留下断言文本。已排除的机制：广告序随机（MemoryMetaStore.ListTimelines
确为裸 map 序，但 ls-remote 客户端排序，hiddenTip 取首行恒为
refs/heads/alice/wip）；同秒时间戳平局。候选：wantsBeyondTips 的
fail-open 分支（ResolveRefs 出错→放行）或可见性/闭包判定的时间窗。
**破案（2026-11-06 深夜，双缺陷叠加）**：①**服务端真 bug（T82 钉死）**——git 会把
v2 fetch 第二轮 POST（重发全量 have 表）以 Content-Encoding: gzip 压缩（<1KiB
也压）；服务端裸读把 gzip 字节当 pkt-line→首包乱码→会话静默终结→空响应
（git 2.50 报 expected acknowledgments）；修=三处 POST 面 bodyReader 解压。②**测试潜伏 bug（被①掩盖数月）**——hiddenTip 取广告首行，
而 mem 广告序=map 随机：main 在前时抓到的是**合法可取**的 main tip；本地
git 2.50 必触发①→fetch 必败→负向断言碰巧全过（115 连绿的真相）；runner
滚动到 Ubuntu 26/git 2.53 后该场景不再 gzip→fetch 成功→按 map 序偏置概率
暴露（实测 2-key map 序偏差可达 7/8，与历史 ~12% 失败率吻合）。修=hiddenTip
显式钉 alice/wip 行；mem ListTimelines 补排序对齐 SQL 确定性。取证链：
v3 注解具名断言→断言携带现场（bob 可见=main@hiddenTip）→packet trace 锁
空响应→服务端 peek/头部日志锁 gzip。
