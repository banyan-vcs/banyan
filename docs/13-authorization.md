# 13 · 授权设计与个人分支可见性（M5 切片）

> 2026-10-25 落地。定位：**抽象层 + demo 策略 + 集群控制层 + Web 演示**。
> 能力广告诚实：认证是 demo 级 HTTP Basic（静态用户表），策略是静态
> 规则表——生产形态（token introspection / SSO / 数据库策略）在同一个
> `auth.Service` 缝隙替换，wire 层零改动。

## 1. 抽象（internal/auth）

nil Service = 授权关闭（存量行为，全部测试不依赖认证）；一经接线，
**wire 层没有任何绕过点**：

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

- RPC 层目前**只做认证**，不做细粒度授权（repo/dir/branch 规则在
  git wire 上；Web 经 gitwire 广告拿分支列表故页面可见性正确）——
  RPC 细粒度授权列 M5 后续；
- 密码表现明存储于配置（demo 定位）；token 认证（PAT/CI 透传）为
  M5 下一项；
- Web 会话为进程内存（重启即登出）；生产需持久会话存储。

## 6. Web 演示（web/，TypeScript 零运行时依赖）

登录（凭证经 `/api/v1/auth/login` 单源校验，Web 不存密码表，会话
HttpOnly cookie）→ 仓库列表/建仓 → 仓库页**分支列表 = 登录者视角**
（个人分支可见性最直观的演示）→ 路径面包屑文件浏览 + blob 预览
（直接解析 git tree 对象格式；ref 列表来自与 git 客户端同一份
过滤广告）。所有请求携带会话用户凭证 → 服务端规则与 git 客户端
完全一致。`docker compose up -d`：Web 8101 / git 8100（compose 内
挂载 demo/auth-demo.yaml：alice/bob/admin，密码 demo）。
端到端走查手册（命令即脚本）见 [14-docker-demo.md](14-docker-demo.md)。

测试：T38（wire 全矩阵，真实 git 客户端 v0+v2）+ TestAuthorizationRPC
（认证门/login/建仓）；Web 端到端为实测验证（见提交记录）。

## 7. SSH 公钥身份（git-over-SSH，2026-10-25）

`IdentifyKey(user, keyBlob)` 加入 Service 缝——SSH 传输的公钥认证
（authorized_keys 行的 base64 载荷）与 Basic 认证同源解析为 Identity，
此后**走完全相同的四决策**：广告过滤、CanFetch、CanPush 对 ssh:// 会话
与 HTTP 会话逐字节同义。demo 策略 `auth.keys` 按用户配置公钥行；集群
网关经 refs 策略快照解析（快照携带 keys，fail-closed 语义不变）。传输
细节（会话形态/协商轮次）见 docs/03 §7b，T39 钉住。边界：SSH 传输要求
auth 必须开启（无钥匙表可认证任何人——拒绝启动而非裸监听）。
