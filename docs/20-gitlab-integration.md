# 20 · GitLab 集成指南（banyan 作 gitaly 存储节点）

> 定位：**面向接入者的操作手册**——已有 GitLab-CE 把存储层换成 banyan，
> 或先用一键 demo 体验。只讲"怎么接、怎么验、坏了怎么查"；协议覆盖
> 矩阵与实现账本在 [docs/19](19-gitaly-compat.md)，本文不重复。
> 2026-11-07 以真 GitLab-CE 19.5.3 活体验收（docs/19 §8）。

## 0. 形态与边界

```
git/http ──▶ workhorse ──▶ gitlab-rails ──gRPC──▶ banyan(gitaly 面) ──▶ 对象平面
git/ssh  ──▶ gitlab-shell ┘                          ▲
                                              Rails 只改存储配置，其余原样
```

- **替换的只有存储节点**：GitLab 自身（Web/UI/CI/DB/Redis）全部保留；
  banyan 以单个 gitaly 节点身份注册，Rails/workhorse 走标准 gitaly 协议。
- **版本配对**：vendored 协议 v19.5.2 ↔ GitLab **同 minor**（19.5.x）。
  跨 minor 未验证；banyan 升级 = 锁定 GitLab minor 重放 proto + 全量回归
  （docs/19 §0 漂移隔离纪律）。
- **信任模型**：per-storage 共享 token，**token 之后全信**——banyan 的
  gitaly 面不做 GitLab 用户级鉴权（授权、保护分支、审计都在 Rails），
  与上游 gitaly 一致；推送路径经适配器把策略请回 Rails 强制（§2/§4）。
  token 保密级别 = 存储层 root。
- **praefect**：不需要。单节点形态；praefect 族 RPC 以 cluster-of-one
  语义诚实作答（docs/19 §6a）。

## 1. 一键试用（demo）

```bash
docker compose -f demo/gitlab/docker-compose.yml up -d   # ~4 GiB 内存
demo/gitlab/provision.sh
```

- GitLab UI `http://localhost:8180`（root 口令见 compose `GITLAB_ROOT_PASSWORD`）
- banyan 自有面 `http://localhost:8170`（`/api/v1/repos` 看 @hashed 落库）
- 走查内容：种子 root → 铸 PAT → 建项目 → HTTP push → API 读面（默认支/
  树/提交/裸 blob/归档）。T83 是同一循环的 Go 形态（opt-in）。

## 2. 接入已有 GitLab-CE

适用 17.8+（`repositories_storages` 形态；`git_data_dirs` 18.0 已移除）。

**① 起 banyan 存储节点**（配置节，生产姿态 token 必配否则拒绝启动）：

```yaml
listen: "0.0.0.0:8080"        # banyan 自有管理面（可只绑内网）
root: "/var/lib/banyan"
meta: postgres                 # 生产缺省；sqlite 单机显式选择
gitaly:
  listen: "0.0.0.0:8075"       # 对 Rails/workhorse 的 gRPC 面
  token: "<共享密钥>"
  storage_name: "default"      # 必须与 GitLab 侧存储名一致
  gitlab:                      # post-receive 适配器（强烈建议配上）：
    url: "http://<gitlab>:8180"           # 经 GitLab 的 push 触发平台处理
    secret_path: "/etc/gitlab/gitlab_shell_secret"  # 共享信任（见下）
```

`secret_path` 指 GitLab 的 `gitlab_shell_secret` 文件（omnibus 在
`/var/opt/gitlab/gitlab-rails/etc/`）——banyan 用它签发与 gitlab-shell
同形的 JWT 调 `/api/v4/internal/{pre,post}_receive`：push 落库后
Rails 自动过期读缓存、记活动事件、触发 pipeline、处理
`-o merge_request.create` 等 push 选项；`/pre_receive` 配对引用计数。
**同一配置还武装 `/allowed` 策略门**（T86）：ref 移动前 banyan 以
gitlab-shell 同形契约问一次 Rails——保护分支等 ref 级推送策略由此强制
（workhorse 前门只做仓库级授权，看不见 pack 里的 ref）；拒绝时 Rails 的
原因原样透传到 git 客户端，Rails 不可达则拒推（fail-closed，与上游
一致）。不配适配器时这些平台处理不会发生、策略门也不设防（数据正常，
只是 GitLab 侧"无感"），需手动 `expire_all_method_caches`（§6）。

**② GitLab 侧 `gitlab.rb`**（三处，注意是 `repositories_storages`）：

```ruby
gitlab_rails["repositories_storages"] = {
  "default" => {
    "gitaly_address" => "tcp://<banyan-host>:8075",
    "gitaly_token"   => "<共享密钥>",
  },
}
gitlab_rails["gitaly_token"] = "<共享密钥>"
gitaly["enable"] = false       # 关掉内置 gitaly
```

**③ 生效与验证**：

```bash
gitlab-ctl reconfigure
# §3 验证清单全过即接入完成
```

SSH 推送无需额外配置：gitlab-shell 的 gRPC 面与 Rails 同一存储配置，
直连 banyan 的 SSHService。

## 3. 接线验证清单（copy-paste）

```bash
# ① Rails 视角的存储配置（runner 须 -u git：postgres peer 认证）
docker exec -u git <gitlab> gitlab-rails runner \
  's=Gitlab.config.repositories.storages["default"]; puts s.inspect'

# ② 建项目（Rails → CreateRepository → banyan）后，banyan 面看到 @hashed 仓
curl -s http://<banyan>:8080/api/v1/repos | grep -o '"id":"[0-9a-f]\{16\}[^"]*"'

# ③ push + 读面（PAT 换 root token）
git push http://oauth2:<PAT>@<gitlab>/<group>/<project>.git main
curl -H "PRIVATE-TOKEN: <PAT>" http://<gitlab>/api/v4/projects/<id>/repository/tree?ref=main
curl -H "PRIVATE-TOKEN: <PAT>" http://<gitlab>/api/v4/projects/<id>/repository/archive.tar.gz -o a.tgz
```

任何一步 404/500 → §6 排查表。

## 4. 能力矩阵（速览）

| 面 | 状态 |
|---|---|
| 建仓/删仓/改默认支、push/clone/子模块、Web 浏览（树/提交/diff/blame） | ✅ 活体验收 |
| Web 编辑器提交、分支/tag 管理、MR 合并（含 squash/cherry-pick/revert） | ✅ OperationService 全 16 RPC |
| 归档下载（tar/tar.gz/zip）、bundle 导入导出、仓库导入（URL/bundle） | ✅（LFS 树见 §6） |
| wiki、个人 snippet 仓库 | ✅（同为 gitaly 寻址，自动落 banyan） |
| SSH 推送/拉取（gitlab-shell） | ✅ 同一授权面 |
| **push 后的活动流/pipeline/MR-from-push 触发/缓存失效** | ✅ **post-receive 适配器**（T85，§2 的 `gitlab.gitlab` 配置节）：经 GitLab 的 push 以 gitlab-shell 同形契约（JWT 签名）调 Rails internal API——缓存自动过期、事件/pipeline/MR-from-push 全触发（live 实证：push 后零干预读面即刻刷新）；不配置则退回"数据正常落库但平台无感" |
| **ref 级推送策略：保护分支（HTTP 推送的强制）** | ✅ **`/allowed` 策略门**（T86，同一 `gitlab.gitlab` 配置节）：ref 移动前问 Rails `GitAccess.check`——developer 推保护分支被拒且原因上屏、新分支/owner 放行（live 矩阵）；SSH 面由 gitlab-shell 前置检查、HTTP 面由此门补齐 |
| 服务端维护（housekeeping/repack） | 空操作语义（对象平面恒 compact，docs/19 §6） |

## 5. 回退与数据面

数据在 banyan 对象平面（@hashed 寻址与 GitLab 同构）。回退到真 gitaly：

```bash
# banyan 侧导出（任选）：全量 bundle 或服务端备份
#   CreateBundle / BackupRepository（docs/19 §6a 备份 sink 键位与上游同形）
# gitaly 侧恢复：git clone <bundle> 或按备份布局回灌
```

仓库名零翻译（`@hashed/xx/yy/<hex64>.git` ↔ banyan 仓名 `<hex64>`），
来回映射不需要改名。

## 6. 故障排查（症状 → 原因 → 处置）

| 症状 | 原因 | 处置 |
|---|---|---|
| 建项目 400 `invalid bearer token` | token 两边不一致 / 时钟漂移 >30s（v2 HMAC 有窗口）/ GitLab 版本过老不会签 v2 | 核对 `repositories_storages` 与 banyan `gitaly.token`；`chrony`/NTP 对时 |
| **项目页 500**（异常栈 `undefined method 'data=' for nil` → `tree_entry`） | 旧版 banyan 对 TreeEntry 缺失路径答零消息空流（上游答 NotFound；项目页恒探测 `.git-blame-ignore-revs`，API 走查测不到） | 升级 banyan（修复 `f2a4204` 起含） |
| Web/API 一切像空仓（tree 404、无默认支） | 读缓存陈旧：适配器未配置/未生效（正常由 post_receive 链过期） | 检查 banyan 日志 `post-receive adapter → …` 与 §2 配置；应急 `gitlab-rails runner 'Project.find(<id>).repository.expire_all_method_caches'`（-u git） |
| 计划删除的项目（路径带 `-deletion_scheduled-N`）还在项目列表 | 正常：CE 默认**缓冲期删除**（7 天后才真删），不是卡死 | 管理区可立即删除或恢复；`pending_delete=false` 属预期 |
| archive 500（workhorse 日志 `SendArchive: …`） | 树内实有 LFS pointer 且 `include_lfs_blobs`（smudge 面未建，诚实拒绝）；或历史版本缺结构化错 | 前者为能力边界（登记 docs/19 §8）；升级 banyan ≥ 0e137d8 |
| 空仓 `FindDefaultBranchName` 答 `refs/heads/main` 但项目页仍"空仓" | 正常契约：无分支时启发式答空 | 先推一个提交 |
| push 被拒 `! [remote rejected] … (You are not allowed to push code to protected branches…)` | **正常强制**：`/allowed` 策略门在起作用（T86）——保护分支等 ref 级策略由 Rails 判定，消息原样透传 | 无需处置；确需推送走 MR 或放开保护设置 |
| push 被拒 `gitlab policy check unavailable` | Rails 不可达/答非所问，门 fail-closed（与上游钩子一致） | 查 GitLab 健康（`gitlab-ctl status`）与适配器配置 §2；恢复后重推 |
| Rails runner 连不上（`Peer authentication failed`） | runner 以 root 跑，postgres peer 拒 | `docker exec -u git …` |
| 种子 root 失败 `Password must not contain commonly used…` | GitLab 黑名单（demo/pass/gitlab 字样都拒） | 随机强口令；种子读 `ENV['GITLAB_ROOT_PASSWORD']` |
| **omnibus 端口**：宿主 curl 000 但容器健康检查绿 | nginx 监听 `external_url` 的端口（容器内 8180），映射须 `8180:8180` 非 `8180:80` | 改 compose 端口映射 |
| 改了挂载的 gitlab.rb 不生效 | `GITLAB_OMNIBUS_CONFIG` 只在**首次 boot** 生成 `/etc/gitlab/gitlab.rb` | 直接改容器内 `/etc/gitlab/gitlab.rb` 再 reconfigure |
| 拉镜像卡死在大层（1.6GB） | 部分镜像站对大 blob 静默断流（curl 直拉正常） | 手工组装 OCI 归档（逐 blob 摘要校验后 `docker load`），或换镜像源重试 |

## 7. 子项目（子目录）clone

banyan 的招牌能力——任意子目录本身即仓库——在 GitLab 模式下照样可用：
GitLab 的 push 经 workhorse→gitaly 落到 banyan 的接收路径，**目录锚定
照常发生**，子树时间线就在对象平面里。

**首选 · 子树投影 ref（`refs/banyan/tree/<dir>`，T84）**。默认分支的
每个**顶层目录**在父项目的 ref 空间里自动获得一个投影 ref，指向
`/tree/<dir>` 面服务的同一个子树视图——一条 GitLab URL、一套 GitLab
凭证/权限、实时同一份数据（推送向上装配回根视图）。全部经 GitLab
URL 活体验证：

```bash
git ls-remote http://gitlab.example.com/group/monorepo.git
  # refs/banyan/tree/src      ← 顶层目录各一条

# 取子树（git 的 clone -b 只认分支/标签短名——客户端约束，
# 所以配方是 clone + 一条 fetch refspec）：
git clone http://gitlab.example.com/group/monorepo.git
git fetch origin refs/banyan/tree/src:refs/heads/src
git checkout src                     # 工作区 = src/ 内容，历史仅 src/

# 改完推回（子树工作请在投影血统的分支上做——服务端回滚守卫与
# git 客户端的 fast-forward 前置检查都要求同血统）：
git push origin src:refs/banyan/tree/src
#   → 落入 src/ 目录时间线并向上装配：根视图 main 随之前进
```

语义要点：投影集 = 默认分支的**顶层**目录（嵌套目录的变更织入其顶层
投影）；删投影 ref 不支持（子树视图是派生态）；与根分支的混合推送可
以同包各走各的命名空间。GitLab 分支/标签列表不受影响（只查
`refs/heads|tags`）。

**备选 A · banyan 自有面直取**（运维/调试后门；绕过 GitLab 权限，走
banyan 自己的 PAT 面，docs/07 §8）：

```bash
git clone http://<banyan-host>:8080/r/<64hex>/tree/<dir>   # 64hex = sha256(project_id)
```

**备选 B · GitLab 拉镜像**（把子树物化成一个真 GitLab 项目换取全平台
能力；代价是镜像延迟、单向副本、要建项目，且同机部署需在 Admin →
Settings → Network → Outbound requests 放行内网 import）：

```bash
curl -X POST -H "PRIVATE-TOKEN: <PAT>" .../api/v4/projects \
  -d '{"name":"monorepo-src","import_url":"http://<banyan-host>:8080/r/<64hex>/tree/src","mirror":true}'
```

**协议层未来**：M3 的服务端 filter 完整规格（含 `subtree:<path>`，
docs/14）落地后，可对 GitLab URL 直接 partial clone 子树。

## 8. FAQ

- **EE 能用吗？** 协议面一致（gitaly 协议不分 CE/EE），EE 特有存储面
  （如 Gitaly Cluster 拓扑）未验证——单节点形态先行。
- **性能/容量？** 服务端有基准门（docs/11）；gitaly 面走与 git HTTP 同一
  对象平面，读路径共享整 pack 直传与织入物化（docs/03）。
- **多存储节点？** GitLab 侧可配多条 storage（不同 storage_name 各指向
  一个 banyan 实例）；banyan 自身的 HA 拓扑（docs/10）另论。
- **升级节奏？** 跟 GitLab minor：banyan 换 vendored proto + 全量回归
  （docs/19 §1），demo 栈锁同 minor 镜像先行验证。

---

相关：[docs/19](19-gitaly-compat.md)（协议覆盖与实现账本）·
[docs/12](12-docker-demo.md)（demo 总览）·
[docs/07](07-authorization.md)（banyan 自身授权面——gitaly 面之外的访问）。
