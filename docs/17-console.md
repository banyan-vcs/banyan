# 17 · 产品端控制面 console：逻辑仓库树与继承式成员权限（M6 设计稿）

## 0. 定位与命名

`cmd/banyans`（banyan **s**erver，产品端服务）+ `internal/console`（控制面
实现）。它在 gateway **之上**，替代演示版的"代码管理服务端"角色：

- **banyand 保持无产品语义**：gitwire（git 协议）+ rpc（存储运维 API）
  + refs + storage，不认识用户/组/项目这些产品概念；
- **banyans 是产品前门**：用户、令牌、组/项目/子项目、成员与权限，
  以及所有产品端 API 与 git 流量入口；
- **两套数据彻底分离**：console 自带 sqlite（modernc 纯 Go），只存产品
  端数据；gateway 元数据（refs/timelines/packs/hooks）与产品库互不感知；
- 演示模式（banyand 直连进程内 StaticPolicy）原样保留——现有测试与
  demo 全部依赖它，console 是并列的第二种部署形态而非替换。

## 1. 拓扑与请求流

```
git/CLI/Web ──HTTP──▶ banyans（console）
                       │ 认证（PAT/会话）→ 逻辑路径解析 → 粗粒度门
                       │ 改写 /{group}/{project}[/path].git
                       │      → /r/{repo_uid}[/tree/{sub}]
                       │ 签名身份头（凭据不下放）
                       ▼
                     banyand（gateway：gitwire + rpc 核心）
                       │ 深度执行：protected / personal / want 覆盖
                       ▼
                     refs 服务 + ScatteredStore
```

两层执行、互不重复：console 负责**身份 + 路径 + 仓库级粗粒度门**
（无任何访问权的路径一律 404，不泄露存在性）；网关继续承担**深度
检查**（保护分支、个人分支可见性、隐藏分支 want 覆盖、锚点粒度
fetch 限制），依据是从 console 同步的快照（§5）。规则只在 console
定义、以快照形态到达网关——延续 docs/07 §4"规则永不复制到网关，
网关跑同一抽象 + 同步的状态"的原则。

代理纪律（T46 教训）：console 对 git 流量是**流式转发**
（io.Copy 级，无整包缓冲），sideband/pack 逐块穿透。

## 2. 逻辑寻址与物理映射

- **格式**：`{group}/{project}` 为仓库，`{group}/{project}/{path...}`
  为子项目（path 是项目内相对路径，多段合法）。v1 **单层组**：按段
  解析无歧义（1=group、2=project、3+=path）；嵌套组见 §8。
- **物理名**：console 建项目时生成不可变 `repo_uid`（12 字节 hex），
  以它调用网关建物理仓（POST /api/v1/repos）。组/项目改名 = 产品库
  改一行，存储零感知（docs/03 rename 保留为运维后门）。console 模式
  下网关关闭 push 自动建仓——console 是唯一建仓前门。
- **子项目** = 产品库条目 + 既有的子树虚拟仓锚
  （`/r/{repo_uid}/tree/{sub}`）。注册前缀链解析与 hooks 的
  matchHooks 同型：请求路径 P 的执行链 = group → project → 所有
  已注册且路径为 P 前缀的子项目，最具体者在末端。
- **bootstrap**：注册时路径尚不存在，可选服务端 CreateCommit 建立
  空目录（服务层已有该能力，时间戳单调防织入平局）。
- **URL**：`/{group}/{project}.git`（根克隆）、
  `/{group}/{project}/{path}.git`（子项目克隆）。API 仍挂 `/api/v1`。

## 3. 产品端数据模型（console 独立 sqlite）

统一资源树，继承链即祖先链：

```sql
nodes(id, kind,          -- 'group' | 'project' | 'dir'
      parent_id,         -- 树结构：group→project→dir(子项目)
      segment,           -- 本段名；project 附唯一性约束于 group 内
      repo_uid,          -- 仅 project：物理仓名
      visibility,        -- 仅 project：'private'(默认)|'public'
      created_at)
users(id, username UNIQUE, pass_hash, is_admin, created_at)
tokens(id, user_id, name, token_hash, scopes, expires_at, revoked)
ssh_keys(id, user_id, key_blob UNIQUE, comment, created_at)
members(id, node_id, user_id, role, created_at,
        UNIQUE(node_id, user_id))   -- + 索引 (user_id)
```

约束：节点深度上限 16（组+项目+子项目路径段）；`dir` 允许挂在
`dir` 下（services/auth 直接注册，services 不必先注册——前缀链只算
已注册节点）。schema 保持可移植 SQL（HA-2 时毕业 Postgres）。

## 4. 成员与权限

**角色**（对齐 docs/07 的两操作面 + 管理面）：

| 角色 | fetch | push | 管理动作 |
|---|---|---|---|
| Owner | ✓ | ✓ | 节点成员管理、删项目/子项目 |
| Maintainer | ✓ | ✓ | 建子项目、protected 配置 |
| Developer | ✓ | ✓ | — |
| Reporter | ✓ | — | — |

实例 admin（users.is_admin）全局最高（巡检/恢复，同 docs/07 §3）。
个人分支规则（refs/heads/<user>/*）不变，仍由网关深度执行。

**继承语义：沿链只增不减（max 聚合）。**

```
effectiveRole(user, P) = max{ role | memberships(user, n), n ∈ chain(P) }
```

子项目默认继承父项目权限（链上 max 自然涵盖），子项目自身成员表
独立加授（"独立、复用"：同一张 members 表、同一套解析，服务任意
节点）。**不设子级降权/负规则**——这不是简化偷懒，而是存储模型
决定的硬边界：访问粒度是**锚点**，浅锚点的闭包包含深路径的全部
内容（在根锚点可 clone 整仓），子级"收回"父级授权在 fetch 上天然
不可执行。要藏住某子路径，必须在能覆盖它的最浅锚点收紧（例如
把根锚点 Reporter 名单清掉）。设计把这个事实显式写进权限语义，
而不是留一个看似可行实则漏风的"覆盖"开关。

管理动作的门：管某节点成员 = 链上任一 Owner 或实例 admin；建子项目
= 链上任一 Maintainer+。解析成本 O(链长×一次索引查询)，链长 ≤ 16。
**保护分支只在根锚点生效**（实现期定案）：它守的是项目集成分支；
子树时间线有自己的协作语义（收敛队列，docs/15），推子树的 main 是
锚点粒度的日常工作，不算动集成分支。单段子项目名不得占用网关运维
子资源（placement/size/hooks/rename/force-push）与 `members`。

## 5. 认证与授权流

- **PAT**：`banyan_` 前缀随机 token，库存 sha256，带过期（v1 全权
  限，scope 列留作后续）；git 用 HTTP Basic(user, PAT)，API 另接受
  `Authorization: Bearer`。密码登录仅 web 会话用（M6.4）。
- **签名身份头**：console 验证凭据后向网关注入
  `X-Banyan-Identity: <user>.<unix_ts>.<hmac-sha256>`（集群共享
  internal_token 作 HMAC 密钥，±30s 窗）。网关只验签名——**用户表
  与令牌哈希永不下放**，凭据不二跳。公开仓的匿名读者以签名的
  pseudo-identity `anonymous` 通过（单一执行路径，gitwire 不为
  匿名开口子）。
- **快照 v2**：console 暴露 `/internal/authz/policy/v2`（集群内部面），
  按 repo_uid 输出 {public, protected_branches, anchors{锚点→直接
  成员}}与用户名/admin 集（个人分支归属判定）。网关侧 UpstreamPolicy
  以同一前缀-max 公式聚合；demo StaticPolicy 路径与格式不动。TTL 10s
  + 未知仓触发一次带限流的强制刷新 + **变更踢**：console 的每次
  策略变更同步 POST 网关 `/internal/authz/refresh`（管理动作零延迟
  生效，而非等 TTL——T50 实证 PATCH 后紧接的 push 立即受新规则）。
- **SSH**：密钥是产品数据，终结点必须在 console。一期仅 HTTP；
  SSH（console 终结 → 内部 HTTP 走同一代理）列 M6.3，可滑出。

## 6. 控制面 API（/api/v1，console）

| 面向 | 端点 |
|---|---|
| 自身 | `GET /me`（用户+可达项目树）、`GET/POST/DELETE /me/tokens` |
| 实例 admin | `GET/POST /users`、`GET/POST /groups` |
| 项目 | `GET/POST /repos/{group}/{project}`（POST 建项目=产品库+物理仓两跳）、`DELETE`（归档）、`PATCH`（改名/visibility） |
| 成员 | `GET/PUT/DELETE /repos/{g}/{p}[/path]/members/{user}`（body 带 role） |
| 子项目 | `POST /repos/{g}/{p}/{path}`（可选 bootstrap） |
| 透传 | 存储运维端点（repos 的 placement/size/hooks/repack、objects、fetch-trees…）console 做**参数映射**（逻辑名→repo_uid）后原样转发网关，CLI 无感切换 |

错误信封沿用 docs/05 §3.2；无权限访问不存在的资源同样 404。

## 7. 里程碑与测试

- **M6.1 ✅（2026-10-26）** internal/console：模型 + sqlite + 链解析 +
  角色聚合（纯库，无 HTTP）+ auth.UpstreamPolicy（签名身份/快照 v2/
  前缀-max 聚合/未知仓强制刷新）。测试 T49。
- **M6.2 ✅（2026-10-26）** cmd/banyans：CRUD API + 逻辑路由 + 流式
  代理 + 签名身份 + 快照 v2 + 变更踢（/internal/authz/refresh）。
  测试 T50：真实 git 客户端经 console clone/push（根+子项目）；
  保护分支 PATCH 即时生效；Reporter 拒 push；锚点级授予后过且根
  404；public 匿名 fetch；PAT 吊销即失效；64MiB 软限制下全链路
  存活（代理贡献经直连网关隔离实验证零）。banyand 侧 auth.upstream
  接线（与 remote/demo 模式互斥）。
- **M6.3** 子项目 bootstrap / visibility 细化 / 归档恢复 / SSH
  （T51/T52，SSH 允许滑出到独立里程碑）。
- **M6.4** web 会话 + 最小管理 UI（复用 web/ 零依赖经验）；审计
  事件流（enterprise 候选触点）。

新用例登记 docs/13；console 侧同样"每个能力一个集成用例"。

**T50 附带观察（存储面跟进项，非 console 范畴）**：all-in-one 形态
下 exact-pack 命中的 clone 仍有 ~2.5×repo 的分配落在
enumerateClosure（go-git MemoryObject 路径；差分 profile 直证），
T46 的 haCluster 基线（150MiB 峰值 12.4MiB）未暴露此形态——待专项
核查（怀疑点：首推轻量引导提交跨 pack 后的元数据索引路径）。

## 8. 边界与诚实清单（一期不做）

- **负规则 / 中断继承**：语义上不可执行（§4），不做；"在浅锚点收紧"
  是等价且可执行的替代。
- **嵌套组**（`a/b/c` 组歧义）：需要查表驱动解析或显式分隔符，
  列后续；统一资源树结构上已兼容。
- **web UI、会话存储**：M6.4 前用 PAT + curl/CLI 管理一切。
- **console HA**：v1 单实例单写人（sqlite）；HA-2 Postgres 毕业时
  一并解决。网关/存储的 HA 不受影响（console 挂 = 产品面不可入，
  已建立的 git 直连网关形态不受波及——若部署选择直连）。
- **组织级 SSO/SCIM、审计、配额**：enterprise 方向，另章。
