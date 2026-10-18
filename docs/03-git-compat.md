# 03 · git 兼容层（gitwire）

兼容性承诺：**标准 git CLI 的 clone / fetch / push 对根仓库与虚拟子树仓库都直接可用**。协议取 Smart HTTP v1（go-git 同款；标准 git 客户端自动协商回退，无需 v2）。

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
| `filter` | fetch | 代码就绪（`gitwire/filter.go`）但**未广告未接线**（M3 开启）；当前客户端走全量回退（§9） |
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

## 4. 服务端 filter（设计就绪，未启用）

> **当前状态**：`gitwire/filter.go` 已实现 spec 解析与闭包过滤（含 `subtree:<path>` 扩展），但 upload-pack **未广告 `filter` 能力、未接线**——不广告能力时标准客户端不会发送 filter 行。启用前的实测行为见 §9：客户端警告后回退全量克隆，无兼容性破坏。

`filter` 能力对齐 git partial clone 语义（`Documentation/technical/partial-clone.adoc`）：服务端在 pack 构造时按 filter-spec 裁剪对象集，客户端本地标记 promisor remote，事后按需补取。

支持规格：

| spec | 语义 | 来源 |
|---|---|---|
| `blob:none` | 不含 blob，仅 commit/tree | git 标准 |
| `tree:0` | 仅 commit + 显式 want 的树 | git 标准 |
| `sparse:oid=<oid>` | 按稀疏模式 blob 过滤 | git 标准（对齐 `list-objects-filter`） |
| **`subtree:<path>`** | **仅 path 子树内对象 + 全部 commit** | **banyan 扩展** |

`subtree:<path>` 是虚拟仓库的下载优化：`git clone --filter=subtree:/src/lib` 等价于克隆子树仓库但 URL 仍在根仓库——供不想改变 URL 语义的 CI 场景使用。实现位于 `gitwire/filter.go`，对闭包内的 tree 遍历做路径前缀剪枝。

**补取路径**：partial clone 客户端事后 `git fetch` 缺失对象时，git 会发起一次普通 fetch（want=缺失 OID）。此请求同样落在 `/git-upload-pack`，服务端按 OID 直取（全局索引 → 分片节点），无需可达性计算——这正是 Scion「客户端精确要什么就给什么」思想在 git 协议内的实现。`banyan` CLI 的懒回填 RPC（05）与之共享同一对象定位代码。

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

`receive-pack.c` 的 pre-receive/post-receive/update 钩子在 banyan 中抽象为 `Policy` 接口（M1 仅日志策略）：push 校验、组装前准入（M4 合并队列的挂点）、CI 触发（对照 Scion 的 commit-vote）。

## 8. 已知限制（接受并记录）

| 限制 | 影响 | 缓解 |
|---|---|---|
| 无 protocol v2 | git 客户端自动回退 v1（§9 实测），无感 | 需要时 M3 补 |
| ~~v1 have 协商~~ | 已实现（终态 ACK 语义：有公共提交时 ACK 即终态、同响应内送包） | 组合测试实测过期客户端 pull 正常 |
| push `no-thin` | 客户端 push 略增流量 | 服务端存储简单性优先 |
| go-git 服务端 filter 缺失 | 自研（本层 §4） | 代码就绪未广告 |
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
| `git clone --filter=blob:none` | 未广告 `filter` → 客户端两次警告 `filtering not recognized by server, ignoring` 后**回退全量克隆**；对象集与全量 clone 完全一致；git 仍写 `remote.origin.promisor=true` 配置（对象全在，无害） | ✅ |
| `git clone --filter=tree:0` | 同上 | ✅ |
| promisor 配置下的后续 fetch | 请求携带 `filter` 行（uploadpack 显式忽略）+ 警告，fetch 本身正常 | ✅ |
| `git clone --depth 1/2`（shallow） | §3a 浅化协议：shallow 段组帧 + 按边界裁剪 pack；`.git/shallow` = 边界提交 | ✅ |
| `--depth` 超过历史深度 | 空边界 = 裸 flush 段，客户端接受为全量（非浅仓） | ✅ |
| 浅仓普通 fetch（无 deepen） | 客户端上报浅边界，服务端不发 shallow 段、pack 裁剪至客户端边界（历史止于旧 tip） | ✅ |
| 浅仓 `fetch --depth N`（加深） | `shallow <新边界>` + `unshallow <已补全>` 段，浅边界下移 | ✅ |
| `git fetch --unshallow` | 客户端 `deepen 2147483647` + 上报边界 → 服务端 `unshallow` 回显 → 浅文件清除 | ✅ |
| 子树 URL 浅化 / 解浅 | 投影 OID 空间内同一套语义 | ✅ |
| 浅仓 push | 拒绝（对照参考服务端 `receive.shallowUpdate=false` 默认 403） | ❌ 干净拒绝 |

**推论**：v1-only 服务端对现代 git 客户端是零配置兼容的——v2、partial clone、shallow 三大特性全部可用或走规范降级；剩余边界只剩服务端 filter（回退全量，带宽问题）与浅仓 push（参考服务端同样默认拒绝）。
