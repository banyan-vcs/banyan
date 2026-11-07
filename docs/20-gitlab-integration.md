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
  与上游 gitaly 一致。token 保密级别 = 存储层 root。
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
```

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
| **push 后的活动流/pipeline/MR-from-push 触发** | ⚠️ **开放缺口**：post-receive→Rails internal API 回调链未建（docs/14 M5 后继项）——push 数据正常落库，但 GitLab 侧事件不触发；读缓存需显式过期（§6 workaround） |
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
| Web/API 一切像空仓（tree 404、无默认支） | 读缓存陈旧（正常由 post-receive 链过期——本缺口） | `gitlab-rails runner 'Project.find(<id>).repository.expire_all_method_caches'`（-u git）；已开的替代路径：带显式 `ref` 的 API |
| archive 500（workhorse 日志 `SendArchive: …`） | 树内实有 LFS pointer 且 `include_lfs_blobs`（smudge 面未建，诚实拒绝）；或历史版本缺结构化错 | 前者为能力边界（登记 docs/19 §8）；升级 banyan ≥ 0e137d8 |
| 空仓 `FindDefaultBranchName` 答 `refs/heads/main` 但项目页仍"空仓" | 正常契约：无分支时启发式答空 | 先推一个提交 |
| Rails runner 连不上（`Peer authentication failed`） | runner 以 root 跑，postgres peer 拒 | `docker exec -u git …` |
| 种子 root 失败 `Password must not contain commonly used…` | GitLab 黑名单（demo/pass/gitlab 字样都拒） | 随机强口令；种子读 `ENV['GITLAB_ROOT_PASSWORD']` |
| **omnibus 端口**：宿主 curl 000 但容器健康检查绿 | nginx 监听 `external_url` 的端口（容器内 8180），映射须 `8180:8180` 非 `8180:80` | 改 compose 端口映射 |
| 改了挂载的 gitlab.rb 不生效 | `GITLAB_OMNIBUS_CONFIG` 只在**首次 boot** 生成 `/etc/gitlab/gitlab.rb` | 直接改容器内 `/etc/gitlab/gitlab.rb` 再 reconfigure |
| 拉镜像卡死在大层（1.6GB） | 部分镜像站对大 blob 静默断流（curl 直拉正常） | 手工组装 OCI 归档（逐 blob 摘要校验后 `docker load`），或换镜像源重试 |

## 7. FAQ

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
