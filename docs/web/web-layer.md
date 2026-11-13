# web 层：`banyand web`（三层独立站）

> 2026-11-14。web 层从 Node BFF 演化为 Go 服务（`internal/webapp`），成为
> `banyand` 的子命令。独立站模式下形成 **web / gateway / 存储** 三层；
> web 层有自己的数据库，可与 gateway 共用；独立站档位下 web 是唯一控制面，
> GitLab 集成档位下只透出 banyan 特有的子树功能。

## 0. 拓扑与请求流

```
浏览器 ── React SPA（web/，Vite）
   │ same-origin cookie（banyan-sid，HttpOnly）
banyand web（:3000，internal/webapp）
   · web_sessions / web_oauth_states / web_imports（表前缀 web_）
   · 登录：local（转发 gateway /api/v1/auth/login）
     或 OAuth（GitHub / gitlab.com / 私有 GitLab，多实例并存）
   · /api/v1/* 与 /r/*/info/refs 反代（注入会话凭证，剥 BASE_PATH）
   · 导入编排（web_imports 状态机 → gateway 同步导入 API）
   │ Authorization: Basic（local 密码 / oauth2:token / x-access-token:token）
banyand serve（gateway / all-in-one）
   · gitwire（smart HTTP/SSH）+ rpc（/api/v1）
   · auth：静态表 | auth.gitlab_sso | auth.oauth 多 provider（§docs/gateway/authorization.md 8d）
   · POST /api/v1/imports（internal/importer，go-git 服务端 fetch）
   ▼
存储（demo: LocalShardStore；集群: store 节点 + refs 服务）
```

SPA 永远见不到凭证：每个被代理的调用由 web 层注入会话的 Authorization，
即同一用户 git 客户端使用的凭证形态。

## 1. 数据库（独立或共用）

`-db`（env `BANYAN_WEB_DB`）三种形态：`sqlite:<path>`（缺省）、`mysql:<dsn>`、
`postgres:<dsn>`。全部表带 `web_` 前缀，因此**可以与 gateway 的元数据库共用
同一个库**——demo compose 里两者指向同一个 sqlite 文件
（`/var/lib/banyan/banyan.db`），少起一个实例；多进程共用 sqlite 的契约是
WAL + busy_timeout + immediate 事务（`meta.SQLiteDSN` 导出的同一套 pragma）。
生产可独立成库（mysql/pg DSN）。

| 表 | 内容 |
|---|---|
| `web_sessions` | 会话：username、credential、provider 标识、过期。TTL 12h，惰性清扫 |
| `web_oauth_states` | OAuth state（anti-CSRF；入库使回调可落在任一实例） |
| `web_imports` | 导入任务史（running/succeeded/failed）；重启时遗留 running 标记 failed("interrupted") |

**安全姿态（诚实清单）**：credential 列明文存储——与静态策略 YAML 明文密码
同档位（demo 姿态）；at-rest 加密列是登记的后续项。web 层不落任何 source
凭证：导入的 `source_authorization` 只存在于运行中的调用里。

## 2. 登录与 OAuth provider

provider 配置四源合并、按 id 去重（先到先得）：
`BANYAN_OAUTH_PROVIDERS`（env JSON，数组或 `{"providers":[…]}`）、
`OAUTH_PROVIDERS_FILE`（缺省 `/config/oauth-providers.json`）、legacy
`BANYAN_GITLAB_*` env 三件套、legacy `/config/gitlab-oauth.json`
（GitLab demo 的 provision.sh 原样兼容）。

流程是标准 authorization-code + state。scope：GitLab `read_api
read_repository`，GitHub `repo`（私有仓导入必需，consent 页明示）。
token exchange 对 GitHub 带 `Accept: application/json`；用户识别走各
provider 的 `/user`（GitLab `username` / GitHub `login`）。

gateway 侧对应 `auth.oauth`（多 provider 的 MultiSSO 策略，legacy
`auth.gitlab_sso` 单实例不变）——见 docs/gateway/authorization.md §8d。
web 层只做"识别与代持"，授权判定永远在 gateway 的策略里。

## 3. 档位（profile）

`-profile standalone | gitlab`（env `BANYAN_WEB_PROFILE`）。
`/api/mode` 广播 `{profile, sso, providers, capabilities}`，SPA 按能力表
裁剪导航与页面（路由级 Gate 兜底）：

| 档位 | 登录 | 能力 |
|---|---|---|
| standalone | local 表单 + 全部 provider 按钮 | 全量（仓库/代码浏览/历史/diff/blame/分支/token/子树/设置/建仓/导入）——**唯一控制面** |
| gitlab | 仅 GitLab SSO | 仓库列表（只读导航）+ 仓库 overview + **subtrees**（列表/活动/访问规则/clone recipe）；GitLab 保有项目管理与代码浏览 |

palette 跟随会话的 provider：GitLab 紫、GitHub/本地为 banyan 绿。

## 4. 仓库导入

`POST /api/imports`（web 层）：`{name?, url, auth:{mode, token?}}`。
- **session 模式**：仅 provider 会话可用——token 按约定包装
  （github → `x-access-token:<t>`、gitlab → `oauth2:<t>`）；**本地密码
  永不发给第三方源站**（该组合直接拒绝）。
- **token 模式**：粘贴 PAT；按 URL host 匹配 provider 决定包装；`banyan_`
  前缀的 banyan 自有 PAT 按属主约定 `<session-user>:<token>`（docs/web/console.md §4）。

web 层把任务写库后由 goroutine 调 gateway 的**同步**
`POST /api/v1/imports`（30min 超时），状态轮询 `GET /api/imports`。
gateway 侧：requireWriteRank、目标名必须不存在（409）、失败回滚建仓、
成功采用远端 HEAD 为默认分支；导入器跳过 `refs/banyan/*` 派生投影 ref。
导入引擎（`internal/importer`）与 gitaly FetchRemote 共用，pack 流式落盘
（大仓内存与 小仓同价）。SSH remote 不支持（docs/integration/gitaly-compat.md 登记项）。

## 5. 部署形态

| 栈 | 形态 |
|---|---|
| 根 docker-compose（demo） | `banyan` + `web`（`banyand web`，共用 sqlite 卷）—— 2 容器 |
| demo/gitlab | `banyan`（gitaly 面）+ `banyan-web`（`-profile gitlab -base-path /-/banyan`）+ GitLab-CE |
| 生产（示例） | web ×N（独立 mysql/pg）→ gateway ×N → refs + store 节点 |

镜像：`web/Dockerfile` 三阶段（node 构建 SPA → golang 构建 banyand →
alpine 仅二进制 + dist/，运行时无 Node）。`BASE_PATH` 构建期烘入 SPA、
运行期由 web 层剥除。

## 6. 与 console（`banyand console`）的边界

console 是产品端控制面（组/项目/成员继承、PAT、逻辑仓库路由、
`auth.upstream` 签名身份模式）——与 web 层是两个面：web 层走
"gateway 策略直通"（会话代持凭证），console 走"签名身份 + 策略快照"。
docs/web/console.md §7 的 M6.4（web 会话 + 管理 UI）在独立站形态由本层
承担；console 自身的 web UI 仍是后续项。
