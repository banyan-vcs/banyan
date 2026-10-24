# 01 · 核心数据模型：目录锚定时间线（Anchored Timeline）

## 设计意图（用户原话锚定）

> 不同于 Git 基于 root tree 建立 commits history，希望将 **tree、blob 存储与 commit 存储分离**，可以**基于任意 tree 创建 commit**，commit 之间**基于 tree 目录关系，建立额外的 commit timeline**。

git 的隐含约束是：commit → root tree → 整个仓库快照，历史只能挂在根上，于是"子目录的历史"只能是派生视图（`git log -- <path>` 或 `git subtree split`）。banyan 把这个约束拆掉：

1. **对象层与提交层分离**：blob/tree 是不可变内容寻址对象（与 git 编码完全一致），commit 是引用它们的独立记录。
2. **commit 锚定任意 tree**：每条 commit 带 `anchor_path`，其 `tree_oid` 是**该路径处的 tree**，不要求是根。
3. **目录关系生成额外时间线**：每个目录可有自己的一条或多条 commit timeline；目录嵌套关系通过**组装算法**自底向上把子目录状态合成为父目录（乃至根）的新 tree 版本。

## 1. 对象层（与 git 同构）

```
Blob  ::= content bytes                     oid = H("blob <size>\0" || content)
Tree  ::= [(name, mode, target_oid)]        oid = H("tree <size>\0" || entries)
```

- 哈希与编码规则与 git 完全一致（默认 SHA-1，可选 SHA-256 `object-format`）。
- 不可变、内容寻址、可全局去重——这是分片 CAS 存储的地基（见 [02-storage.md](02-storage.md)）。
- **树不要求出现在任何根树下**：`/src/foo` 处的 tree 是一等公民，独立存在、独立被 commit 引用。

## 2. 提交层：AnchoredCommit

```go
type AnchoredCommit struct {
    OID        OID        // 内容寻址（序列化体含 anchor_path，见下）
    AnchorPath string     // 锚定目录："/"、"/src"、"/src/foo"（规范路径）
    TreeOID    OID        // anchor_path 处的 tree（任意 tree，不限根）
    Parents    []OID      // 父 commit，须同 AnchorPath（跨锚点关系见 §4）
    Author, Committer Identity
    Message    string
    Synthetic  bool       // 由组装算法生成的标记（git trailer: banyan-synthetic）
}
```

要点：

- **git commit 是特例**：`AnchorPath == "/"` 且 `TreeOID` 为根树时，序列化结果与 git commit 逐字节一致（`tree <oid>\nparent ...\nauthor ...\n...`）。anchor_path 不为 `/` 时，序列化体附加 header `banyan-anchor <path>\n`（放在 tree 行之后），因此 OID 空间与 git 天然区分、不冲突。
- **Timeline**：同一 `(repo, anchor_path, name)` 下按 parent 关系串联的 AnchoredCommit 序列。
  - 根时间线（`anchor="/"`）语义等同 git branch；
  - 目录时间线（`anchor="/src/foo"`）就是"该子目录作为独立仓库"的历史。
- `name` 即 branch 名：timeline 标识三元组 `(repo, anchor, name)`。git 客户端在虚拟仓库 `/r/repo/tree/src/foo` 看到的 `refs/heads/main`，就是 `(repo, "/src/foo", "main")` 的投影（见 §5）。

## 3. 时间线与目录关系的两种基本方向

模型中信息流有两个无环方向，理解它们是理解整个模型的关键：

```
        ┌─────────────────────────────────────────────┐
        │ 组装方向（上行，写路径）                        │
        │   子目录本地 commits ──合成──▶ 父目录新 tree    │
        │   ──合成──▶ 根 synthetic commit 入根时间线      │
        └─────────────────────────────────────────────┘
        ┌─────────────────────────────────────────────┐
        │ 投影方向（下行，读路径）                        │
        │   根时间线某 commit 的 root tree               │
        │   ──截取──▶ 任意子目录在历史某刻的快照           │
        │   ──重写──▶ 该子目录时间线的 git 投影            │
        └─────────────────────────────────────────────┘
```

两个方向都以**已存在的不可变对象**为输入，因此无环、可缓存、可增量计算（派生数据引擎的活，见 [04-derived-data.md](04-derived-data.md)）。

## 4. 组装算法（Assembly，上行）

### 4.1 状态定义

任一时刻，目录 D 的**当前树** `CurrentTree(D)`：

```
CurrentTree(D) = base(D) ⊕ replace-children(D)

  base(D)      = D 本地时间线 head 的 TreeOID（若 D 无本地提交，取根投影，见 §5.2）
  replace-children(D) = 对每个子目录 C ⊂ D：
                        把 base(D) 中 C 名下的条目替换为 CurrentTree(C)
```

自底向上递归，叶目录无子目录时 `CurrentTree(D) = base(D)`。纯函数、确定性。

### 4.2 子目录提交的向上传播

当 push/commit 落在目录时间线 `(repo, "/src/foo", "main")` 上时：

```
1. 该 timeline head 前进：head = C_new（anchor=/src/foo，tree=客户端提交的根树）
2. 组装触发（默认同步模式；异步模式走合并队列，见 §7）：
   root_new = ReplaceSubtree(CurrentTree("/"), "/src/foo", C_new.TreeOID)
3. 生成 synthetic commit 入根时间线：
   S = { anchor="/", tree=root_new, parents=[根时间线原 head],
         message="banyan: assemble /src/foo@<C_new.OID>", synthetic=true }
4. 根时间线 head 前进到 S
```

`ReplaceSubtree(root, path, subtree)` 是对不可变树的纯函数改写：拷贝路径上每一层的 tree 条目，产生新 OID——与 git 的 tree 改写语义一致。

**自洽性**：组装完成后，`CurrentTree("/")` 中 `/src/foo` 处的子树与 `/src/foo` 时间线 head 的树逐字节一致（因为第 2 步刚替换过）；此时根时间线最新 commit 的 root tree 又成为其他目录投影的新输入——上行与下行在不可变对象上汇合，没有循环定义。

### 4.3 并发与冲突

- 对象写入（blob/tree/commit 进 CAS）**无锁**：内容寻址，天然幂等。
- 唯一串行点：**timeline head 的 CAS 更新**（元数据服务内每 repo 一条线性化日志，见 [02-storage.md](02-storage.md#元数据服务)）。
- 同锚点并发提交：后到者在 CAS 上失败，走标准 git 语义——非 fast-forward 拒绝，客户端 rebase/merge 后重推。
- **跨锚点无冲突**：`/src/foo` 与 `/docs` 的提交互不阻塞（这是相对 git 单根历史的核心并发收益）；只有当两者的组装都指向同一个根 head 时，第二次组装基于第一次的结果重算（CAS 重试一次即可，ReplaceSubtree 幂等）。

## 5. git 投影（Projection，下行）

投影回答两个问题：git 客户端 clone 根仓库时看到什么？clone 虚拟子树仓库时看到什么？

### 5.1 根投影（trivial）

`anchor="/"` 的非 synthetic commit 序列 **原样就是** git commit 链（§2 的特例保证逐字节兼容）。synthetic commit 投影为普通 git commit（带 `banyan-synthetic` trailer，合法且对 git 透明）。

**因此 `/r/{repo}` 的 clone/fetch 是零翻译成本的。**

### 5.2 子树投影（subtree projection）

对路径 P，`ProjectTimeline(repo, P, name)` 输出一条 git commit 链：

```
完整历史 = split(根时间线, P) ⊕ P 本地 commits

  split(根时间线, P)：沿根时间线做 subtree split——对每个根 commit，
      取 root tree 中 P 处的子树提升为根树，重写为投影 commit。
      纯函数：输入 commit OID 确定 ⇒ 输出确定 ⇒ SHA 稳定 ⇒ 可缓存可增量。
  P 本地 commits：客户端在虚拟仓库里 push 上来的 commit，其根树
      天然就是"P 处的子树"形态（git 客户端在 /r/repo/tree/src/foo 视角下
      构建的 commit 根树就是 src/foo 的内容），原样接在 split 历史之后。
  汇合点：P 本地时间线的 base marker 记录分叉自根时间线的哪个 commit
      （即 split 历史的末端），之后根时间线再前进时 P 历史默认不自动
      跟随（显式 rebase 语义，见 §7 同步策略）。
```

投影 commit 的元数据规则固定（message 保留、committer 追加 `banyan-projected-from: <原 commit OID>` trailer），保证同版本算法下 SHA 稳定，派生缓存永不错效。

### 5.3 虚拟仓库视图

gitwire 层为每个 `(repo, P)` 暴露一个虚拟仓库 `/r/{repo}//{P}`：

| git 客户端操作 | banyan 服务侧行为 |
|---|---|
| `GET /info/refs?service=git-upload-pack` | 列出各 timeline 的投影 head 作为 refs |
| `POST /git-upload-pack`（fetch/clone） | 按协商 wants 构造投影对象集 → 在线合成 pack |
| `POST /git-receive-pack`（push） | 解包 → 对象入 CAS → commits 锚定 P → 触发组装（§4.2） |

## 6. ref 映射

| banyan 概念 | git 可见形态 |
|---|---|
| 根时间线 `(repo, "/", "main")` | `/r/repo` 的 `refs/heads/main` |
| 目录时间线 `(repo, "/src/foo", "main")` | `/r/repo/tree/src/foo` 的 `refs/heads/main` |
| 目录时间线（服务端内部视角） | `refs/banyan/<anchor>/<name>`（仅 banyan CLI/rpc 暴露） |
| 分叉标记 base marker | 投影 commit 的 `banyan-projected-from` trailer |

git 客户端永远只看到 `refs/heads/*`；目录时间线的内部命名不泄漏到 wire 协议。

## 7. 设计取舍记录

| 决策 | 选择 | 理由 | 备选 |
|---|---|---|---|
| 组装时机 | push 时同步组装 | 一致性即时，git 客户端立刻能在根看到子目录提交 | 异步合并队列（M4 引入：组装进队列，测试通过才合成） |
| 子目录历史跟随 | 显式 rebase（快照分叉） | SHA 稳定、可控，用户心智与 git rebase 对齐 | auto-follow（每次根前进自动延长 split 历史，写放大且 SHA 不稳） |
| anchor 序列化 | git 兼容体 + `banyan-anchor` header | `/` 锚点逐字节兼容 git；非 `/` 锚点 OID 空间隔离，永不与 git 对象混淆 | 独立 commit 编码（失去零翻译收益） |
| 跨锚点 parents | 禁止（Parents 须同锚点） | 防止模型出现跨目录环；跨目录关系由组装/投影表达 | 允许跨锚点 merge commit（git 侧无对应语义，暂不需要） |
| synthetic commit 可见性 | 投影为普通 git commit | git 客户端无感，历史完整 | 隐藏（破坏 clone 一致性） |

## 8. 工作示例

```
初始：repo demo，根时间线 main：
  R1 {anchor:/, tree:T0}

开发者 A 直接 push 到 /r/demo（标准 git push）：
  R2 {anchor:/, tree:T1, parents:[R1]}          ← 根投影零翻译

开发者 B 在 /r/demo/tree/src/lib 工作（虚拟仓库，git 视角就是普通仓库）：
  本地 commit L1 {root tree: S1(即 src/lib 的内容)}
  push 后锚定存储：
  L1' {anchor:/src/lib, tree:S1, parents:[]}    ← B 的根树被原样锚定
  组装触发：
  T2 = ReplaceSubtree(T1, /src/lib, S1)
  R3 {anchor:/, tree:T2, parents:[R2], synthetic:true}

开发者 C 现在 clone /r/demo/tree/src/lib：
  split(R1..R3, /src/lib) = [S1@R1? 无则空, ...] ⊕ [L1']
  → 得到以 S1 为根的独立历史，git 完全可用：
    git log / git blame / 再 push L2' 均为标准语义

开发者 D clone /r/demo：
  看到 R1 → R2 → R3（R3 带 banyan-synthetic trailer），src/lib 的演进包含在内
```

## 9. 模型收益汇总

1. **子目录即仓库**是数据模型的一等能力，不是客户端技巧（partial clone/sparse 只是下载优化，见 [03-git-compat.md](03-git-compat.md#服务端-filter)）。
2. **提交并发域按目录分片**：不同目录的提交互不阻塞，天然适配"无锁合并"方向。
3. **O(变更大小) 的提交成本**：子目录 push 只处理子树 + 路径上的 tree 改写，不遍历整仓库。
4. **git 兼容零成本路径**：根投影逐字节兼容；子树投影是纯函数派生数据，可缓存可增量。

## 10. 统一 DAG 提交历史（设计 v2，2026-10-17）

§5 的 split 投影在实测中暴露与三条硬要求的根本冲突：**git 的 commit hash
绑定 (root tree, parents, 签名, message)**，因此"同一变更跨子树/父视图
同 hash" ⇔ "两视图共享同一批 commit 对象" ⇔ **统一 DAG**。任何重写历史
（split 投影、`git subtree split` 同理）必然换 hash——这是不可能三角。

### 三条要求 → 三个原语（internal/model/unified.go）

| 要求 | 原语 | 机制 |
|---|---|---|
| ① hash 兼容 git 设计 | 全部对象纯 git 规范编码 | 新路径不写任何 `banyan-*` 私有头（旧投影头弃用） |
| ② 子树与父树 hash 一致 | **SubtreeTip**（视图 tip） | tree=根提交在 X 处的子树、parent=根提交本身；确定性（签名固定、时间取根提交 committer 时间）⇒ 同 (rootOID, X) 恒同 hash。祖先即统一 DAG，**父树历史以原始 hash 进入子树** |
| ③ 子树提交并入父历史 | **AdoptSubtreePush**（收编式组装） | 规范 subtree-merge commit：tree=ReplaceSubtree(根树, X, C.tree)、parents=[根head, **C 原样**]——C 的 hash 在两个视图中完全一致 |

"仅父树修改到子树的提交才展示在子树中"由**服务端 log RPC** 承担
（`/api/v1/log?path=X`，rpc/log.go）：X 视角归一（树不含路径首段 ⇒ 该树
即 X 视角根树，处理子树侧提交的混合作用域）后按"相对任一父变化"过滤——
hash 与视图完全一致，过滤只是查询语义（查询即 RPC）。

### 权衡记录

| 维度 | v1 split 投影（弃用于读路径） | v2 统一 DAG |
|---|---|---|
| 跨视图 hash | ✗ 重写换 hash | ✓ 同一批对象 |
| 子树 push 在父视图 | synthetic 包装（换 hash） | C 本尊为 merge 第二父 |
| 子树内展示过滤 | 天然（历史即过滤后） | 服务端 log RPC / 文件级 pathspec |
| 子树 clone 带宽 | 小（仅 X 对象闭包） | 全历史（M3 服务端 subtree filter + promisor 解决） |
| 子树仓 `git log -- .` | 有效过滤 | 不过滤（祖先树是全仓，路径空间不匹配）——已记录 |

SplitHistory/AssembleRoot 保留为派生分析用途，不再服务读写路径。

## 11. 轻量提交与 Message 对象（设计 v3，2026-10-17）

commit 的源信息一分为二：**信封**（tree 行之外的一切：parents、签名、
gpgsig/mergetag 等多行头、消息正文）存为独立内容寻址的 **bmsg 对象**
（git 哈希方案、类型标签 `bmsg`，与 git 对象空间天然隔离、永不进 wire）；
提交退化为轻量记录 **(tree 边, bmsg 引用)**——两个定长指针。

### 运行时渲染（git 兼容场景）

git commit 对象成为渲染产物：`render(tree, envelope) = "tree <oid>\n" + envelope`，
分三个层次：

| 层次 | 机制 | hash 保证 |
|---|---|---|
| push 收编的 commit | 信封原样保存，渲染＝还原 | **字节级恒等**（实测 go-git 4043/4043 还原成功，含 gpgsig 多行头，render-fails 0） |
| banyan 原生 commit | 确定性渲染（签名/时间源自不可变输入） | §10 既有模式（视图 tip / 收编 merge 本就是运行时生成），本设计将其推广为普适机制 |
| M3 选项：pack 剥离 | 只存 tree/blob + 轻量记录，fetch 时渲染 | 省去 commit 字节存储，但丧失 ExactPackHit 整包直传——评估后再定 |

### 收益与实测

- 提交记录变定长边——CommitGraph 的原生形态，O(1) 结构查询不解码变长对象；
- bmsg 内容寻址去重：机器人/模板化 merge 消息在 monorepo 中大量重复
  （实测 go-git 4043 提交去重率 0%——每条时间戳唯一所致；去重在
  bot 批量提交/视图 tip 等场景兑现）；
- metadata-only 修改（amend/rebase 元数据编辑）只新增 bmsg，不动结构边；
- 与统一 DAG（§10）正交叠加：hash 一致性由"信封原样 + 确定性渲染"继承。

### 不变式（测试锁定，internal/model/message_test.go）

`Decompose ∘ Render = 恒等`（字节级，真实夹具含 gpgsig/双亲 merge/无父根
提交）；非 tree 首行拒绝；同信封不同 tree ⇒ 同一 bmsg、不同 git commit。

## 12. 提交历史设计 v4：时间线优先（2026-10-18，需求校准）

### 需求校准（推翻 §10 的 hash 同一性约束）

**核心问题重述**：大规模场景下，各子树由不同人/团队**互不干扰地并行迭代**，
每个子树的持续集成（clone/log/diff/构建）只应承担**自己路径的规模**。
严格的单根统一 DAG（v2）让每次子树 clone 拉全仓历史（实测 packfile
目录 12KB 内容拉 40MB/28649 对象），并行度越高浪费越大——与核心目标
背道而驰。

**一致性重新定义**：跨视图 SHA 同一（同 message 允许不同 sha）不再是
要求；一致性 = **clone 出的仓库在后续 pull/push 反复迭代中的收敛性**：

1. 子树 pull 拿到自己路径的最新状态（含根侧/他侧合入到该路径的变更）；
2. 子树 push 不丢失他人变更（内容级校验），连续 push/pull 无伪冲突；
3. 迭代可无限重复，历史只增不改（已发布的提交 SHA 稳定）。

### 架构：子树时间线优先 + 身份映射收编

- **子树视图 = 子树时间线**：`(repo, X, branch)` 的提交链，全部 X 作用域
  （树以 X 为根）。clone 只下载该链闭包——**子树规模的历史**。
- **根侧变更自动下投（auto-follow）**：服务根 head 的 `SplitHistory(X)`
  投影——根提交触碰 X 则在 X 的投影链上追加一个 X 作用域投影提交
  （不同视图不同 SHA，允许）。**收编 merge 身份映射**：AdoptSubtreePush
  生成的 M=[rootPrev, C]（C 为 X 作用域、树等价）在投影时直接映射为
  C 本尊（零新提交、零重复）——自己 push 的变更回来仍是自己的 SHA。
- **合头规则（后裔规则）**：
  - 无本地时间线 → 投影链头；
  - 投影链头是本地头的后裔（根侧有 X 新变更，常态）→ 投影链头
    （客户端 pull = **fast-forward**，根侧变更自动流下）；
  - 本地头是投影链头的后裔 → 本地头；
  - 真分叉（两侧各有新提交）→ 本地头优先（保守：root 侧等待显式
    sync；服务端不做内容合并——冲突必须由 git 三方合并显式解决）。
- **子树 push**：内容级校验（sameXView 竞态防护不变）→ 时间线 CAS →
  收编进根（AdoptSubtreePush 保留，C 原样第二父）。
- 根视图 = 根时间线原生服务（不变）；父级视图 = 同机制（父级时间线，
  闭包含其子树内容，层级递归）。

### 与 v2 的对照

| 维度 | v2 统一 DAG | v4 时间线优先 |
|---|---|---|
| 子树 clone 闭包 | 全仓（28649 对象 @go-git） | 子树规模（~1541 对象 @packfile） |
| 跨视图 SHA | 同一 | 允许不同（收编提交在根中仍以原 SHA 为第二父） |
| pull 迭代 | FF/合并（链式 tip 修复后） | FF 为主（后裔规则）；真分叉显式 sync |
| 根侧 X 变更下投 | 天然（同 DAG） | 投影链 auto-follow |
| 伪冲突 | 需链式 tip 规避 | 天然无（时间线全 X 作用域，merge-base 即 X 作用域） |

v2 的全部协议修复（协商终态 ACK、delete-refs、ng 行格式、竞态防护、
连续 push）在 v4 中原样保留；v2 的 resolveViewTip/链式 tip 退役。

## 13. v5：写路径时间线化 + 读路径线性织入（assembly-on-read，2026-10-18）

针对三条硬性要求（A/B 目录并行零锁竞争；仅同目录并发出现 git 式提交锁；根历史收敛整洁）：

**写路径**：唯一持久化可变状态 = 各时间线 head（根本地 + 每目录一条）。子树 push 只做本目录时间线 CAS（sameXView 双侧守卫：组合头 X 内容 + 根本地投影包含性）；根 push 的客户端链**原样落地**（其边界父 = 广告的织入 tip，已由 `/.woven` 时间线持久化）。无收编 merge、无根 head 全局争抢——①②由构造满足。

**读路径**（`internal/view`）：根视图 = **线性织入**——全部源提交（根本地链 + 各子树时间线本地链）按 (时间, 锚点, oid) 拓扑序折叠为单亲历史；子树条目经 `ReplaceSubtree` 嫁接进根树；未被打断的根本地提交 OID 原样复用（无子树活动的仓库根历史字节稳定）。**增量锚**：每分支（及每个作用域视图）的织入 tip 持久化为 `/.woven[<scope>]` 内部时间线——新条目只追加其后（到达序=因果序、前缀稳定、重启安全）。作用域视图 = 稳定前缀（投影+本地，身份 OID）+ 追加段（祖先作用域投影 kind2 / 投影增量 kind0 / 后代嫁接 kind1）。

**稳定性（2026-10-18 增补，c484dfc/14bfdff）**：惰性织入最初以 `/.woven` 祖先（重写空间）做增量排除，但源条目在源空间收集——嫁接源的原 OID 永不入重写祖先，每次物化都重复折叠（subject 重复、视图 tip 漂移、伪 fetch-first）。现以**每源织入标记** `/.woven<view>@<src>`（源空间 OID）排除，标记随折叠推进、陈旧（非当前 head 祖先）即忽略重折；作用域追加锚失效（前缀长过它）时标记随之重置。折叠期另有三重去重：kindFull 条目已在 `/.woven` 祖先（客户端链回流的重写体）跳过；嫁接条目结果树 == 当前树 / 已在织入祖先（treeKnown）/ **本批已产出**（batchTrees——同秒提交的时间并列可令旧副本后折，其 ReplaceSubtree 会内容回退）跳过。kind2 祖先条目的 no-op 判据 = 相对**自己父提交**的 X-diff（而非 xtree==cur，后者误判本地侧已前进的分叉并回滚 X）。投影侧 push 守卫的包含性测试第三形态：投影 tip 的树已记录于客户端链历史（TreeKnown）——rebase 恢复后的客户端持有投影的跨视图重写体（OID 不同）+ 更新内容，是**领先**而非陈旧。

**已验证**：根历史零 merge（T5）；A/B 非干扰 + 路径内互斥（T8/T8b）；重启持久化含织入（T17）；RPC/log 走织入视图（T11）；多用户五轮同文件协作全通过（T10，v5 断言：subject 恰一次、线性、blob 一致）；顺序多作用域到达零重复、tip 不漂移、X 外变更不阻塞（T18，含冲突恢复后继续推送）。

### 开放项（收窄后）

- **真并发同 X 分歧**：两个客户端同时改同一目录文件（同秒、各自基于旧 tip）——时间线 CAS 保证恰一者落地，但后到者内容被前者遮蔽（线性历史可见，视图取后者嫁接树）。需要 git 式内容合并或显式拒绝语义，当前按 last-writer-wins 呈现。push 时推进织入（原开放项的修复方向）对顺序流已非必需，保留为真并发场景的候选方案。
- **treeKnown/批内树集合为线性扫描**（上限 10 万提交）：热点仓库需要树→提交索引。
- 既有 v4 状态升级（/.woven 无标记）首折会全量重织：早期阶段直接重建库。

