# 05 · banyan CLI（对标 Scion 的 `sl`）

## 定位

> 全新 CLI，对标 Scion 的 SL，具体功能后续补全。（用户决策）

`banyan` 是**智能遥控器 + git 兼容本地仓**：

- 本地形态 = 标准 git 仓库 + promisor remote 指向 banyan 服务端——Claude Code / IDE / 一切 git 工具链照常工作（Scion 迁移教训：生态兼容性优先）；
- 读操作（log/blame/diff/file-history）走服务端 RPC——服务端集中计算 + 记忆化，客户端零遍历（Scion"问题查询是 RPC"）；
- 缺失对象懒回填走服务端逐对象批量 RPC——补上 go-git 客户端 filter 不能回填的缺口。

## 命令面（分期）

| 命令 | 功能 | 阶段 |
|---|---|---|
| `banyan clone <url>[/subdir]` | 一键克隆（含子目录；等价 URL 的 `//` 形态自动展开） | M1 |
| `banyan log [path]` | 服务端 log（路径历史 RPC），不拉对象 | M2 |
| `banyan blame <path>` | 服务端 blame RPC | M3 |
| `banyan diff [rev [path]]` | 服务端 diff RPC | M2 |
| `banyan file-history <path>` | 路径历史 RPC（变更序列 + 大小/方向） | M2 |
| `banyan fetch-objects <oid>...` | 逐对象批量获取（懒回填内部命令，同 partial clone 补取） | M1 |
| `banyan snapshot` | 快照式本地状态（对标 sl 的 snapshot 工作流） | M4 |
| `banyan stack` | 栈式变更/提交（对标 Scion stacked changesets） | M4 |
| `banyan sync` | 子目录时间线对根的显式 rebase（01 §7 同步策略的 CLI 面） | M3 |

## RPC 契约（HTTPS + JSON，M1 版）

```
POST /api/v1/objects          {repo, oids[]}        → {found: {oid: meta}...}   # 定位+获取
POST /api/v1/log              {repo, anchor, name, path?, limit}
POST /api/v1/diff             {repo, from, to, path?}
POST /api/v1/blame            {repo, rev, path}
POST /api/v1/file-history     {repo, path, since?}
POST /api/v1/timelines        {repo}                → 锚点/timeline 枚举
```

选 JSON 而非 gRPC：M1 阶段减少依赖与联调成本；契约集中在 `internal/rpc/`，M3 若需要流式（blame 大文件）再升级为 gRPC/连接式协议。

**缓存友好**：所有读 RPC 响应带 `ETag: <投影 head OID>`，不可变 ⇒ CDN/浏览器/代理可缓存（Scion 快照 header 思想）。

## 与 git 客户端的关系

```
banyan clone http://srv/r/demo/tree/src/lib
  = git clone --filter=blob:none http://srv/r/demo/tree/src/lib（虚拟仓库）
  + 配置 remote.promisor / remote.partialclonefilter
  + 本地 hook：对象缺失时调 banyan fetch-objects（而非裸 git 的再 fetch）
```

banyan 不隐藏 git——`banyan` 仓库目录内 `git status`、`git log`、IDE 全部可用；banyan 只在"更聪明的读"和"懒取对象"两处接管。
