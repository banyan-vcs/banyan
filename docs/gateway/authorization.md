# 授权设计与个人分支可见性（M5 切片）

> 2026-10-25 落地。定位：**抽象层 + demo 策略 + 集群控制层 + Web 演示**。
> 能力广告诚实：认证是 demo 级 HTTP Basic（静态用户表），策略是静态
> 规则表——生产形态（token introspection / SSO / 数据库策略）在同一个
> `auth.Service` 缝隙替换，wire 层零改动。

## 0. 启动姿态：安全默认（2026-11-05，docs/project/roadmap.md 生产主轴 P0）

**生产缺省下，无认证的进程拒绝启动**——部署错误不该等第一个请求到达
才暴露：

- 客户端面角色（role 为空 / all-in-one / gateway）在**既无
  `auth.upstream` 又无 `auth.users`** 时启动即报错，错误信息直接给出
  出路：`set dev: true for an unauthenticated local deployment`；
- `gitaly.listen` 配置了而 `gitaly.token` 为空：同样拒启（gRPC 面同权
  严肃，docs/integration/gitaly-compat.md §3）；
- `dev: true`（YAML）或 `-dev`（旗标）= **显式开发模式**：免认证本地
  跑，`-help` usage 与配置 Describe 均标注。demo 栈即此形态（单体
  compose 挂 auth-demo.yaml 走正路；集群 demo 网关走 `-dev`）；
- refs/storage 角色不受此限——集群内部面（自有边界），客户端流量
  永远经网关；
- **元数据同姿态**（2026-11-05）：生产（非 dev）下持有元数据的角色
  （all-in-one/refs）`meta` 缺省即 **postgres**（mysql 并列支持、sqlite
  为单机显式选择）；`meta: mem` 内存态仅 dev 合法，生产拒启。缺 DSN
  的报错自带示例串。gateway/storage 不持有元数据、不受影响；
- 首推建仓（auto-create）无需新开关：有认证时匿名推不进（CanPush
  挡在 receive-pack），无认证时进程不再启动。

§1 的"nil Service = 授权关闭"由此收窄：**nil 只在 dev 模式合法**。
回归：TestProductionRequiresAuth（单测，8 情形：三种客户端角色 ×
有/无认证、gitaly token 对、dev 出路）。

## 1. 抽象（internal/auth）

nil Service = 授权关闭（**仅 dev 模式下能启动到这一步**，§0；测试即
-dev 形态）；一经接线，**wire 层没有任何绕过点**：

| 决策 | 语义 | 粒度 |
|---|---|---|
| `Identify(r)` | 凭证 → 身份（Basic 校验；token/SSO 的替换缝） | — |
| `IdentifyKey(user, key)` | SSH 公钥 → 身份（§7；authorized_keys 载荷比对） | — |
| `VisibleRefs(id, repo, refs)` | 广告过滤 = 身份视角的 ref 视图 | 分支 |
| `CanFetch(id, repo, sub)` | 下载准入（sub="" 根仓库，"/lib" 子树虚拟仓库） | 仓库 / 目录 |
| `CanPush(id, repo, sub, branch)` | 提交互准（branch 为 refs/heads/ 后段；辅助 ref 传 ""） | 仓库 / 目录 / 分支 |

## 2. 执行点（gitwire）

| 端点 | 执行 |
|---|---|
| `info/refs`（v1 + receive 发现） | 匿名 401（带 Basic challenge）；广告 = VisibleRefs；自动建仓需 push 权 |
| `ls-refs`（v2） | 同上过滤 |
| `upload-pack`（v1/v2 fetch） | CanFetch；want 覆盖检查改为**对可见 tips**（`wantsBeyondTips`）——经他人个人分支才可达的对象，按裸 OID 也取不走 |
| `receive-pack` | 逐 ref CanPush → report-status `ng permission denied`；越权不改任何 ref |
| `/api/v1/*`（rpc） | 认证门（middleware）；细粒度规则在 git 面（见 §5） |

三粒度 × 两操作的 demo 矩阵（StaticPolicy）：

| | 下载（fetch/clone） | 提交（push） |
|---|---|---|
| 仓库 | 认证用户 | 认证用户 |
| 目录（子树虚拟仓库） | `subtree_access` 名单 | 同仓库规则（vr.Sub 参与） |
| 分支 | 可见才可取（want 覆盖） | 个人命名空间仅本人；`protected_branches`（main）仅 admin |

## 3. 个人分支命名空间：monorepo 分支过多解法

约定 `refs/heads/<user>/<name>`（首段命中用户表即为个人分支）：

- **可见性**：仅本人 + admin 出现在广告（v1/v2）与 Web 分支列表里——
  千人 monorepo 里每个人的 WIP 分支只污染自己的视图，而分支本身
  是一等公民（同一 push/fetch/织入路径，无任何降级）；
- **不可绕过**：他人按裸 OID fetch 隐藏分支的对象被 want 覆盖检查
  拒绝（不止是广告过滤）；往他人命名空间 push 被 per-ref 门禁拒绝；
- admin 全可见是刻意的运维设计（巡检/恢复需要）。

## 4. 集群形态（gateway 抽象控制层）

策略源 = refs 服务（`/internal/authz/policy` 快照端点，集群内部面）；
网关 `auth.remote: true` 接线 `RemotePolicy`：**同一抽象 + 同步的
状态**（10s 缓存；同步失败保旧快照；从未同步 = fail closed 全
401）。规则永不复制到网关。一体化 demo 直接进程内 StaticPolicy
（`auth:` 配置节）。

## 5. 边界与后续（诚实清单）

- ~~RPC 层只做认证~~ **已细粒度化**（2026-11-06，§8b）：管理端点走
  `auth.AdminChecker` 扩展缝（管理员/token admin scope），内容写
  （`/api/v1/commit`、`POST /refs`、建仓）走与 git wire 同一条
  CanPush；只读端点维持认证门；
- 密码表现明存储于配置（demo 定位）；~~token 认证为 M5 下一项~~
  **已落地**（§8：元数据面 PAT，read/write/admin scope）；
- Web 会话为进程内存（重启即登出）；生产需持久会话存储。

## 6. Web 演示（web/，TypeScript 零运行时依赖）

登录（凭证经 `/api/v1/auth/login` 单源校验，Web 不存密码表，会话
HttpOnly cookie）→ 仓库列表/建仓 → 仓库页**分支列表 = 登录者视角**
（个人分支可见性最直观的演示）→ 路径面包屑文件浏览 + blob 预览
（直接解析 git tree 对象格式；ref 列表来自与 git 客户端同一份
过滤广告）。所有请求携带会话用户凭证 → 服务端规则与 git 客户端
完全一致。`docker compose up -d`：Web 8101 / git 8100（compose 内
挂载 demo/auth-demo.yaml：alice/bob/admin，密码 demo）。
端到端走查手册（命令即脚本）见 [docker-demo.md](../operations/docker-demo.md)。

测试：T38（wire 全矩阵，真实 git 客户端 v0+v2）+ TestAuthorizationRPC
（认证门/login/建仓）；Web 端到端为实测验证（见提交记录）。

## 7. SSH 公钥身份（git-over-SSH，2026-10-25）

`IdentifyKey(user, keyBlob)` 加入 Service 缝——SSH 传输的公钥认证
（authorized_keys 行的 base64 载荷）与 Basic 认证同源解析为 Identity，
此后**走完全相同的四决策**：广告过滤、CanFetch、CanPush 对 ssh:// 会话
与 HTTP 会话逐字节同义。demo 策略 `auth.keys` 按用户配置公钥行；集群
网关经 refs 策略快照解析（快照携带 keys，fail-closed 语义不变）。传输
细节（会话形态/协商轮次）见 docs/gateway/git-compat.md §7b，T39 钉住。边界：SSH 传输要求
auth 必须开启（无钥匙表可认证任何人——拒绝启动而非裸监听）。

## 8. Token 认证与可扩展性缝（2026-11-06，docs/project/roadmap.md M5）

**PAT（personal access token）**：`banyan_` 前缀随机明文（crypto/rand，
32B base64url），落库只存 sha256（`storage.TokenRegistry`——元数据面的
一张 `tokens` 表，四后端 conformance 同矩阵）。明文只在铸造应答里出现
一次。HTTP Basic(user, token) 即 git 客户端唯一需要的形态；非空用户名
必须匹配 token 属主（与 console 约定一致，docs/web/console.md §4）。过期与吊销在
**读取时**判定——吊销即刻生效，无缓存窗口。

**scope 是上限不是旁路**：`Identity.Scope`（read < write < admin）由
凭证携带；TokenPolicy 装饰器在任何决策前先压 scope 上限（read 永不能
push，write 永不能管理），其余规则全部委托内层策略（静态密码表/上游
快照/远端快照）——**token 从不绕过内层规则，只收窄**。可扩展性即组合：

```go
svc := &auth.TokenPolicy{Inner: staticOrUpstream, Tokens: meta}
gw.Auth, api.Auth = svc, svc      // wire 层只见 auth.Service，零改动
```

装配（cmd/banyand）：all-in-one 在持有元数据时自动包裹 static 策略；
upstream console 形态不包裹（console 自有 PAT 体系，网关验签名，docs/web/console.md）；
纯网关角色无本地元数据，无 token 面（TokenRegistry 是可选接口，
远端 meta 不实现也不破坏编译）。

**token 生命周期 RPC**：`POST /api/v1/auth/tokens`（登录用户铸造，
应答唯一一次明文；scope 缺省 write，`expires_days` 可选）、
`GET /api/v1/auth/tokens`（本户清单；`?all=1` 需管理员——只回哈希，
永不回明文）、`DELETE /api/v1/auth/tokens/<hash>`（自吊销；管理员可
吊销任何人的）。

## 8b. RPC 细粒度授权（同一扩展缝）

管理端点（repack、rename、force-push 开关、placement、hooks、
`cluster/nodes`、`meta/backup`）经 `auth.AdminChecker`——**可选接口**，
策略实现它才生效，不实现维持原认证门（远端快照网关向后兼容）：
StaticPolicy=管理员表；UpstreamPolicy=console 快照 Admins；
TokenPolicy=admin scope 或内层管理员。内容写走 git wire 同一条
CanPush（`/api/v1/commit` 按解析后的默认分支判定保护分支；`POST /api/v1/refs` 同）。
建仓走**写权限级门**（认证 + scope≥write）：待建仓对 per-repo 策略
天然未知，upstream 快照下 CanPush 会 fail-closed 整个 console 两跳
建仓流——console 在自己侧做完产品级授权后以服务身份转发，网关的
AdminChecker 由此把快照里的服务主体 "console" 列为隐式管理员
（policy.go BuildSnapshot）。只读端点（log/diff/blame/objects/
fetch-trees/resolve）维持认证门。测试：T79（token E2E：read 可取不可
推、write 推过、吊销即刻 401、属主错配拒绝、明文只现一次）、T80
（RPC 分级：开发者 403 / 管理员 200 / admin token 管理 / read token
不能建仓不能动 ref）。

## 8c. 运行时子树访问规则 + GitLab SSO（2026-11-08，docs/integration/gitlab-integration.md 子树管理控制面）

**运行时规则**：`auth.subtree_access`（§2 的 YAML 种子）只在启动时读；
运行态规则住在元数据面——`storage.SubtreeRuleStore`（可选接口，
TokenRegistry 先例：memory + SQL 四后端同 conformance，表
`subtree_rules(repo, path, users)`，随 rename/delete 同事务迁移清理）。
`SubtreeRulePolicy` 装饰任意策略：**最浅覆盖规则管辖**（`/src` 的规则
同时管住 `/src/lib`——子树视图闭包向下包含，docs/web/console.md"在浅锚收紧"）；
**fetch 与 push 同门**；无覆盖规则的路径完全委托内层（从未管理规则的
部署逐字节不变）；规则面不可达=拒绝（fail-closed，与 /allowed 门同姿
态）；内层管理员恒过（管理员管理规则，锁死自己即锁死管理面）。反向
不可行是登记边界：`/src/lib` 的规则**不能**遮住 `/src`——浅锚服务整个
闭包（与 GitLab workhorse 仓库级授权同一结构性事实）。

**管理 RPC**：`GET/PUT/DELETE /api/v1/repos/<repo>/subtree-access`
（admin 门；PUT body `{path, users[]}`，path 为锚形 `/` 或 `/a/b`，
users 空数组=拒绝——开放靠删规则）。清单面
`GET /api/v1/subtrees?repo=`（认证门）：过滤 `/.woven` 内部族与
aux ref、按锚分组、每支线 head 带最近提交（subject/author/when）、
`projected` 标记默认支顶层目录的投影 ref 集。

**GitLabSSO 策略**（`auth.gitlab_sso: <url>` 配置才活，与 users 表/
upstream console 互斥）：HTTP Basic 密码位或 Bearer 头携带 GitLab 签发
token（OAuth2/PAT 同形），验证=一次 `GET <url>/api/v4/user`（按 token
sha256 缓存含负缓存，TTL 缺省 60s；GitLab 不可达=匿名 fail-closed）；
身份=GitLab 用户名，实例管理员（is_admin）应答 CanAdmin——即规则面
的管理者。SSH 缝保持匿名（GitLab 拓扑里 git-over-SSH 终结在
gitlab-shell）；分支可见性直通（GitLab 语义：项目成员见全部分支）。
装配：banyand 组合 `SubtreeRules(GitLabSSO)`；static+token 栈同样可包
（YAML 种子之外补运行态）。测试：T89（清单：过滤/分组/projected/
最近提交）、T90（GitLabSSO 假 GitLab + 规则栈：admin 设规则→bob 子树
clone 拒/过根 clone（边界断言）/push 同门/删规则恢复/非管理员 403）。
