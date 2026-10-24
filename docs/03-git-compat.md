# 03 · git 兼容层（gitwire）

兼容性承诺：**标准 git CLI 的 clone / fetch / push 对根仓库与虚拟子树仓库都直接可用**。协议取 Smart HTTP v1（go-git 同款；标准 git 客户端自动协商回退）。protocol v2 已立项（perf-2，docs/12 P1-5）——ls-refs/fetch 命令面。

## 1. 端点路由

```
GET  {base}/r/{repo}/info/refs?service=git-upload-pack      # fetch/clone 发现
GET  {base}/r/{repo}/info/refs?service=git-receive-pack     # push 发现
POST {base}/r/{repo}/git-upload-pack                        # fetch/clone 数据
POST {base}/r/{repo}/git-receive-pack                       # push 数据

虚拟子树仓库：{repo} 段后接 /tree/ 前缀路径
  /r/demo/tree/src/lib        ← demo 仓库的 /src/lib 子树仓库
  （"/r/demo/tree/src/lib/info/refs" 中的 repo=demo, subpath=/src/lib）
```

对照 git 源码 `http-backend.c` 的服务路由与 `gitProtocolHttp` 规则；差异仅在虚拟仓库路径解析。

> **为什么是 `/tree/` 而不是 `//`：** 骨架阶段用真实 git 客户端冒烟测试发现，标准 git 会把 URL 路径中的 `//` 规范化为 `/`，双斜杠形式无法到达服务端。对外统一使用无歧义的 `/tree/` 前缀；服务端解析器对 `//` 保留兼容（供自定义客户端/测试）。

## 2. 能力协商（capability advertisement）

`info/refs` 应答携带（对照 `upload-pack.c` / `receive-pack.c` 的广告逻辑）：

| 能力 | 值 | 说明 |
|---|---|---|
| `symref=HEAD:refs/heads/main` | HEAD 指向 | clone 默认分支 |
| `agent=banyan/0.x` | 服务端标识 | |
| `object-format=sha1`（或 sha256） | 哈希格式 | 与仓库注册时一致 |
| `side-band-64k` | fetch/push | 设计值（MVP 未广告，应答为裸 pack） |
| `report-status` / `report-status-v2` | push | ref 更新结果回执 |
| `delete-refs` | push | 允许 ref 删除命令（实测必需：未广告时 git 静默丢弃删除） |
| `shallow` | fetch | deepen N / 加深 / unshallow（§3a；deepen-since/not 未实现不广告） |
| `filter` | fetch | **已广告已接线**（§4）：`blob:none` / `tree:0`；不支持的 spec 以 ERR 拒绝（对照 upload-pack.c） |
| `ofs-delta` | fetch | serve 路径 delta 包（docs/12 P1-4）：过阈值后在线 delta 化，OFS_DELTA 优先（客户端未接受时 REF_DELTA）；push 侧 go-git 解码器本就接受两种 |
| `allow-reachable-sha1-in-want` | fetch | 非广告 want（partial clone 惰性取回按 OID 点名 blob/tree）须从广告 tips 可达——`haveClosure` 全闭包走查即为该契约的校验实现 |
| `no-thin` | push | 拒绝 thin pack（存储侧 pack 必须自包含；M3 可放开） |

## 3. upload-pack（服务 fetch/clone）

v1 会话流程（对照 `upload-pack.c` 协商状态机）：

```
客户端 POST body：
  <want OID> [capabilities]
  ...
  <have OID>...            ← 协商：客户端已有的 commit
  flush / done

服务端：
  ① 解析 wants/haves（haves 用于跳过公共历史）
  ② 对虚拟仓库：先做 timeline 投影（01 §5.2），得到 git 对象视图
  ③ 对象闭包计算：wants 可达集 − haves 可达集
     （M1 遍历；M3 用 commit-graph + 位图，见 04）
  ④ 若请求带 filter spec：对闭包应用过滤（§4）
  ⑤ storage 层取对象（02 §3 读路径）→ 在线合成 pack
  ⑥ side-band-64k 流式回写（NAK → pack → ...）
```

**have 协商的投影一致性**：虚拟仓库客户端报告的 have OID 是投影 OID。投影是纯函数且 SHA 稳定（01 §5.2），因此投影 OID 天然可作协商货币——客户端上次 clone 到的投影 commit 这次仍在投影历史中，服务端按投影 OID 裁剪公共前缀。

## 3a. 浅化（deepen / shallow）

对照 **git 2.50 `git-http-backend` 参考流字节级实现**（Node CGI 垫片 + 双向字节转储实测），无状态 v0 语义全部按"每 POST 独立计算"：

```
请求侧（客户端）：
  want <tip> [caps]
  shallow <oid>          ← 客户端当前浅边界（加深/unshallow 时上报）
  deepen <N>             ← N=2147483647 即 --unshallow
  [have...] done

响应侧（服务端，deepen 请求）：
  shallow <oid>...       ← 新边界：已纳入但父链未全纳入的提交
  unshallow <oid>...     ← 客户端上报边界中本次已补全者
  0000                   ← flush（空边界 = 裸 flush）
  [协商轮：到此为止，无 NAK]
  [done 轮：ACK/NAK + pack（边界提交保留 parent 头，父链不入包）]
```

- **边界计算**：wants 沿全部父边 BFS，纳入深度 0..N-1 层；超深/unshallow 自然收敛全量（`shallowCut`）。
- **浅仓普通 fetch（无 deepen）**：客户端上报边界 + have；服务端**不发 shallow 段**，pack 裁剪至客户端边界（等价参考服务端的 have 减除；嫁接层之下对象不入包）。
- **客户端浅文件维护**：deepen 段整体替换边界；未提及的旧条目由 git 按父链完备性自动清理（根提交做边界会被立即清除——测试夹具须让边界提交带父）。
- **投影空间**：子树 URL 的浅化在同一套语义上运行（wants/边界均为投影 OID）。
- 广告 `shallow` 能力；`deepen-since`/`deepen-not`/`deepen-relative` 未实现不广告（对应 `--shallow-since`/`--shallow-exclude` 不支持）。
- **浅仓 push 拒绝**（参考服务端默认 `receive.shallowUpdate=false` → 403；banyan 400，同为干净拒绝）。

## 4. 服务端 filter（已广告已接线）

> **当前状态**（T14）：`filter` 能力已广告；`blob:none` 与 `tree:0` 在闭包遍历中生效
> （被滤对象不进 pack，被滤树不再展开）；显式 want 豁免过滤——惰性取回正是按 OID
> 点名缺失对象。`sparse:oid` 与 `subtree:` 扩展不在线上服务，请求即 ERR 拒绝
> （对照 upload-pack.c 对未知 filter 的行为）。

`filter` 能力对齐 git partial clone 语义（`Documentation/technical/partial-clone.adoc`）：服务端在 pack 构造时按 filter-spec 裁剪对象集，客户端本地标记 promisor remote，事后按需补取。

支持规格：

| spec | 语义 | 来源 |
|---|---|---|
| `blob:none` | 不含 blob（显式 want 豁免），仅 commit/tree | git 标准 |
| `tree:0` | 仅 commit + 显式 want 的对象 | git 标准 |
| `sparse:oid=<oid>` | 按稀疏模式 blob 过滤 | git 标准（未实现，ERR 拒绝） |
| **`subtree:<path>`** | **仅 path 子树内对象 + 全部 commit** | **banyan 扩展（未实现，ERR 拒绝）** |

`subtree:<path>` 的设计定位是虚拟仓库的下载优化（等价于克隆子树仓库但 URL 仍在根仓库），子树仓库 URL 本身已覆盖该场景，故暂不实现。

**补取路径**：partial clone 客户端事后取缺失对象时，git 发起一次普通 fetch（want=缺失 OID，且要求服务端具备 `allow-reachable-sha1-in-want`，否则客户端直接拒绝请求）。此请求落在 `/git-upload-pack`：want 豁免过滤与 have 排除，经可达性校验后按 OID 直取（全局索引 → 分片节点）——这正是「客户端精确要什么就给什么」在 git 协议内的实现。`banyan` CLI 的懒回填 RPC（05）与之共享同一对象定位代码。

## 5. receive-pack（服务 push）

v1 会话流程（对照 `receive-pack.c`）：

```
客户端 POST body：
  <old> <new> <refname> [capabilities]
  ...
  flush
  packfile bytes

服务端（02 §4 写路径）：
  ① 解析 ref 命令行（虚拟仓库时 refname 前缀 refs/heads/ → timeline name）
  ② pack 校验入库（分片 CAS，不解散）
  ③ 解包索引得对象清单 → 解析 commit 锚定（虚拟仓库：anchor=subpath）
  ④ head CAS（乐观锁，失败 → report-status 报 non-fast-forward）
  ⑤ 组装（目录锚点 push 时，01 §4.2）
  ⑥ report-status：每 ref 一行 ok/ng
```

## 6. ref 视图（虚拟仓库）

`refs.go` 为 `(repo, subpath)` 生成对外 ref 列表：

```
根仓库 /r/demo：
  refs/heads/main      → 根时间线 head（非 synthetic 投影零翻译，01 §5.1）

子树仓库 /r/demo/tree/src/lib：
  refs/heads/main      → ProjectTimeline(demo, /src/lib, main) 的 head
                         = split(根历史).末端 ⊕ 本地 commits 的最后者

banyan 内部命名空间（仅 rpc 暴露，不上 wire）：
  refs/banyan//src/lib/main   ← 目录时间线原生形态
```

## 7. hooks / 策略点

`receive-pack.c` 的 pre-receive/post-receive/update 钩子在 banyan 中抽象为 `Policy` 接口（M1 仅日志策略）：push 校验、组装前准入（M4 合并队列的挂点）、CI 触发。

## 8. 已知限制（接受并记录）

| 限制 | 影响 | 缓解 |
|---|---|---|
| 无 protocol v2 | git 客户端自动回退 v1（§9 实测），无感 | **已立项 perf-2**（docs/12 P1-5）：ls-refs/fetch 命令面（按需 ref 广告 + 服务端驱动协商），push 按语义恒 v0 |
| ~~v1 have 协商~~ | 已实现（终态 ACK 语义：有公共提交时 ACK 即终态、同响应内送包） | 组合测试实测过期客户端 pull 正常 |
| push `no-thin` | 客户端 push 略增流量 | 服务端存储简单性优先 |
| ~~serve 路径零 delta~~ | **已实现**（docs/12 P1-4）：原始负载过 64KiB 阈值后在线 delta 化（OFS_DELTA 优先/REF_DELTA 兜底），相似对象 2–5× 字节差 | T20 + storage 单测钉住 |
| 增量 fetch 无真 thin pack | 对客户端已有对象出 REF_DELTA 的字节收益仍缺 | 依赖编码器对缺席基对象的支持，远期（docs/12 P2-6） |
| ~~服务端 filter 未广告~~ | **已实现**（§4）：`blob:none`/`tree:0` + `allow-reachable-sha1-in-want` 惰性取回 | T14 钉住（partial clone 全链路） |
| ~~浅克隆（deepen）~~ | **已实现**（§3a：deepen N / 加深 / unshallow / 客户端边界上报裁剪，对照 git 2.50 http-backend 参考流字节级实现） | T15/T16 钉住 |
| `deepen-since` / `deepen-not` | 未广告未实现（`--shallow-since`/`--shallow-exclude`） | 需要时按 §3a 同框架扩展 |
| 浅仓 push | 拒绝（HTTP 400）——对照参考服务端默认行为（`receive.shallowUpdate=false` → 403），同为干净拒绝 | 完整仓库 push 不受影响 |

## 9. 协议兼容矩阵（git 2.50.0.windows.1 实测，集成测试 T13–T16 钉住）

| 场景 | 行为 | 结果 |
|---|---|---|
| 显式 `protocol.version=0` clone/fetch | v1 会话，裸 pack | ✅ fsck 干净 |
| 强制 `protocol.version=2` clone | 客户端带 `Git-Protocol: version=2` 发现，服务端回 v0 广告（`# service=git-upload-pack`），客户端按规范**优雅降级**走 v1 | ✅ |
| 默认（git ≥2.26 fetch 默认 v2） | 同上降级路径——全部存量用例的 clone/fetch/pull 均已隐式覆盖 | ✅ |
| v2 强制增量 fetch（有 haves） | 降级后单 POST `want + haves + done` → 服务端终态 ACK（公共提交=旧 tip）+ 增量包，零多余协商轮 | ✅ |
| v2 强制子树 URL clone | `/tree/<path>` 路径解析不受协议版本影响 | ✅ |
| `git clone --filter=blob:none` | **已广告生效**：初始包仅 commit/tree；首次工作树物化触发惰性取回（want=缺失 blob OID，经 `allow-reachable-sha1-in-want` 校验） | ✅ |
| `git clone --filter=tree:0` | 仅 commit 对象；树的惰性取回同上 | ✅ |
| promisor 配置下的后续 fetch | 请求携带 `filter` 行，服务端解析并每轮生效 | ✅ |
| 不支持的 filter spec（如 `blob:limit=1k`） | ERR 拒绝（对照 upload-pack.c） | ❌ 干净拒绝 |
| `git clone --depth 1/2`（shallow） | §3a 浅化协议：shallow 段组帧 + 按边界裁剪 pack；`.git/shallow` = 边界提交 | ✅ |
| `--depth` 超过历史深度 | 空边界 = 裸 flush 段，客户端接受为全量（非浅仓） | ✅ |
| 浅仓普通 fetch（无 deepen） | 客户端上报浅边界，服务端不发 shallow 段、pack 裁剪至客户端边界（历史止于旧 tip） | ✅ |
| 浅仓 `fetch --depth N`（加深） | `shallow <新边界>` + `unshallow <已补全>` 段，浅边界下移 | ✅ |
| `git fetch --unshallow` | 客户端 `deepen 2147483647` + 上报边界 → 服务端 `unshallow` 回显 → 浅文件清除 | ✅ |
| 子树 URL 浅化 / 解浅 | 投影 OID 空间内同一套语义 | ✅ |
| 浅仓 push | 拒绝（对照参考服务端 `receive.shallowUpdate=false` 默认 403） | ❌ 干净拒绝 |

**推论**：v1 服务端对现代 git 客户端是零配置兼容的——v2（自动降级）、partial clone（filter 已生效）、shallow 三大特性全部可用；剩余边界只有 v2 命令面未实现（效率项而非兼容项，P1-5）与浅仓 push（参考服务端同样默认拒绝）。

## 10. 实现策略审计（2026-10-18，对照"git 兼容为设计目标"；2026-10-24 更新）

**go-git 触点**：仅 `internal/storage/packfile.go`——pack 解码（Scanner+Parser）与编码（`packfile.Encoder`，含 delta 窗口——P1-4 修正：NewEncoder 第三参是 useRefDeltas 而非 delta 开关，delta 压缩由 Encode 的 window 参数控制，0 即关闭）。协议层（gitwire 全部）自研、以 git 源码为基准；go-git 能力缺口（协议 v2、服务端 filter、delta 调用语义）按 AGENTS.md 原则直接对照 git 源码实现，不受其边界约束。

**已对齐 git 行为**（参考流/集成测试实证）：v0 广告与 symref、终态 ACK 协商（upload-pack.c 基础模式）、report-status 行序与 ng 格式、delete-refs 广告、浅化全语义（§3a，对照 http-backend 字节级）、pkt-line 编解码（gitprotocol-common）、idx v2。

**已接受偏离与修复优先级**（正确性均无碍，客户端实测容忍）：

| # | 偏离 | 影响 | 建议优先级 |
|---|---|---|---|
| 1 | ~~无 have 减除~~ **已关闭（2026-10-18，docs/09 §3.1）**：haves 已知提交可达集排除；浅客户端按 shallow 边界止步 + 穿过排除提交遍历（T15/T16/T19 钉住）；代价 = have 侧一次遍历，位图终解 | — | ✅ |
| 2 | 响应无 side-band（裸 pack） | 客户端无进度显示；大 clone 体验差 | M2/M3（广告 side-band-64k + 通道 2 进度） |
| 3 | **tags 不支持**：`refs/tags/*` 收 `ng unsupported ref namespace` | `git push --tags` / tag clone 失败（干净拒绝，非静默） | ✅ 已关闭（2026-10-18，T20：refs/tags/* + 任意 refs/<ns>/<name>，tag 对象入包/闭包剥离/浅克隆） |
| 4 | EncodePack 无 delta；push `no-thin` | 双向带宽 | M3 repack 职责（`pack-objects.c` delta 算法） |
| 5 | 无 multi_ack/multi_ack_detailed/no-done | 大仓库协商轮数多（单 ACK 终态即可收敛） | 低（规模化后再议） |
| 6 | 浅仓 push 400（参考端 403 + 消息） | 错误提示不够友好 | 低（对齐状态码与 ng 原因行） |
| 7 | deepen-since / deepen-not 未实现 | `--shallow-since`/`--shallow-exclude` 不可用（未广告，客户端明确失败） | 低（§3a 框架内扩展） |
| 8 | sha256 广告分支就绪但未端到端验证 | sha256 仓库未承诺 | 需求出现时验证 |
