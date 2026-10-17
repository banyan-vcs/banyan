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
| `side-band-64k` | fetch/push | 流式回写 + 进度通道 |
| `report-status` / `report-status-v2` | push | ref 更新结果回执 |
| `filter` | fetch | **自研支持**（见 §4），对齐 `uploadpack.allowFilter` 语义 |
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

## 4. 服务端 filter（自研，子目录下载优化）

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
| 无 protocol v2 | git 客户端自动回退 v1，无感 | 需要时 M3 补（ls-refs/fetch 命令帧格式） |
| push `no-thin` | 客户端 push 略增流量 | 服务端存储简单性优先 |
| go-git 服务端 filter 缺失 | 自研（本层 §4） | 已覆盖 |
| 浅克隆（deepen） | M1 不支持 `shallow` 能力 | M3：投影历史天然可截断，实现浅化很直接 |
