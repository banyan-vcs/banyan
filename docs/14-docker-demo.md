# 14 · Docker demo 走查

> 定位：一条命令起栈，把 docs/13 的授权模型**用真实 git 客户端走一遍**。
> 本文命令全部 copy-paste 可用，即测试脚本（2026-10-25 Docker 29.x 实测；
> 协议矩阵另有 T38/T38b 进程内钉住，见 docs/07）。

## 0. 两套 demo 栈

| 栈 | compose 文件 | 演示 | 端口 |
|---|---|---|---|
| 授权 + Web（本文） | `docker-compose.yml` | 权限三粒度、个人分支可见性、Web 管理页 | 8100 / 8101 |
| HA 集群 | `docker-compose.cluster.yml` | 散布多副本、杀节点恢复、动态 R（docs/11，`bash demo/cluster-demo.sh`） | 8100 + 内部 |

两栈都占 8100，**不能同时跑**——切换前先 `docker compose down -v`。

## 1. 启动

```bash
docker compose up -d        # 首次构建约 1-2 分钟
# 受限网络走镜像：
#   docker compose build --build-arg GOPROXY=https://mirrors.aliyun.com/goproxy/
#   NPM_REGISTRY=https://registry.npmmirror.com docker compose up -d --build
```

| 服务 | 端口 | 说明 |
|---|---|---|
| `banyan` | 8100 / 8102 | git smart HTTP（8100）+ **git over SSH**（8102，demo 密钥 `demo/ssh/`）；授权开启（只读挂载 `demo/auth-demo.yaml`）；SQLite 元数据与 pack 分片持久化在 `banyan-data` 卷 |
| `banyan-web` | 8101 | 仓库管理与文件浏览（web/，零运行时依赖）；经 `BANYAN_URL` 内网调 banyan |

探活：`curl -u alice:demo http://localhost:8100/api/v1/repos`（匿名 401 即授权生效）。

## 2. demo 策略（demo/auth-demo.yaml）

用户 `alice` / `bob`（开发者）、`admin`（管理员），密码均为 `demo`：

| 规则 | 配置 | 效果 |
|---|---|---|
| 匿名一律拒绝 | — | git 面 401 + Basic challenge；RPC 面 401 |
| `main` 仅 admin 可推 | `protected_branches` | 其他人 push 收 `ng permission denied` |
| `/secret` 子树仅 admin 可取 | `subtree_access` | 其他人 clone/fetch 被拒（upload-pack permission denied） |
| `refs/heads/<user>/*` 个人分支 | 约定（docs/13 §3） | 仅本人 + admin 可见/可推 |
| 其余 | — | 认证用户可读可推；首次 push 自动建仓（门禁 = push 权） |

## 3. 走查：git 面（8100）

```bash
U=localhost:8100/r/demo.git          # host+path，不带 scheme，便于内嵌凭证

# ① 匿名被拒（credential.helper= 防止宿主机缓存凭证静默补位，见 §6）
git -c credential.helper= ls-remote http://$U          # → 401 / 认证提示

# ② admin 播种（main 受保护，必须 admin；首次 push 自动建仓）
git init -b main demo && cd demo && echo hello > README.md
git add . && git commit -m init
git push http://admin:demo@$U main
git commit --allow-empty -m "alice change"
git push http://alice:demo@$U main                     # → ! [remote rejected] main (permission denied)

# ③ 个人分支可见性 —— monorepo 分支过多的解法
git checkout -b alice/feat && echo wip >> README.md
git commit -am wip && git push http://alice:demo@$U alice/feat
git ls-remote http://alice:demo@$U                     # main + alice/feat
git ls-remote http://bob:demo@$U                       # 只有 main
git ls-remote http://admin:demo@$U                     # admin 全可见（运维巡检设计）

# bob 视角：从干净克隆验证（对象不在本地，请求才是真的）
cd .. && git clone http://bob:demo@$U bob && cd bob
git fetch http://bob:demo@$U $(git -C ../demo rev-parse alice/feat)
                                                       # → not our ref：裸 OID 也取不走
git checkout -b alice/evil && git commit --allow-empty -m x
git push http://bob:demo@$U alice/evil                 # → 冒名推他人命名空间被拒
cd ../demo

# ④ 目录粒度 + 子树即仓库（docs/03）
git checkout main && mkdir secret && echo key > secret/key
git add . && git commit -m secret && git push http://admin:demo@$U main
cd .. && git clone http://admin:demo@$U/tree/secret && cd secret
git log --oneline | head -3 && git fsck                # 子树投影 = 独立完整仓库
cd .. && git clone http://bob:demo@$U/tree/secret      # → permission denied（/secret 仅 admin）
```

要点：可见性过滤发生在**广告**（v1 info/refs 与 v2 ls-refs 同一份过滤），但
不依赖广告防绕过——fetch 的 want 覆盖检查对**可见 tips** 计算，他人个人分支
可达的对象按裸 OID 也取不走；push 逐 ref 检查，越权请求不改动任何 ref。

### 3b. 走查：SSH 面（8102，公钥认证）

demo 自带一次性密钥（`demo/ssh/<user>`，公钥已配进 `auth.keys`；勿复用）。
命令在 banyan 仓库根目录执行（密钥相对路径 `demo/ssh/`），沿用 §3 的 demo 仓库：

```bash
SU=localhost:8102/r/demo.git      # SSH 面：ssh://user@host/path
KEY() { echo "ssh -i demo/ssh/$1 -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"; }

# 认证即身份：同一人 HTTP 与 SSH 看到的完全一致
git -c core.sshCommand="$(KEY alice)" ls-remote ssh://alice@$SU   # main + alice/feat
git -c core.sshCommand="$(KEY bob)"   ls-remote ssh://bob@$SU     # 只有 main

# 克隆（v0）→ bob 先有一份旧 main → admin 推进 → bob fetch（多轮 have 协商）
git -c core.sshCommand="$(KEY bob)" clone -q ssh://bob@$SU sshc
echo via-ssh >> sshc/README.md && git -C sshc commit -qam via-ssh
git -C sshc -c core.sshCommand="$(KEY admin)" push -q ssh://admin@$SU main   # 受保护分支必须 admin（注意换 admin@ URL）
git -C sshc -c core.sshCommand="$(KEY bob)" fetch -q && \
  git -C sshc log --oneline origin/main | head -2

# v2 命令会话（GIT_PROTOCOL env 协商入口）
git -c protocol.version=2 -c core.sshCommand="$(KEY alice)" ls-remote ssh://alice@$SU

# 越权：bob 把自己的提交推进 alice 的命名空间
git -C sshc -c core.sshCommand="$(KEY bob)" push ssh://bob@$SU main:alice/evil
                                                                   # → ! [remote rejected] (permission denied)

# 未授权密钥在握手层即被拒（identity 解不出 → 连接拒绝）
ssh-keygen -q -t ed25519 -N "" -f /tmp/stray
git -c core.sshCommand="ssh -i /tmp/stray -o IdentitiesOnly=yes -o StrictHostKeyChecking=no" \
  ls-remote ssh://alice@$SU                                       # → Permission denied (publickey)
```

首次连接会提示未知 host key（`accept-new` 自动接受；指纹在
`docker logs banyan | grep "host key"`）。协议语义与 HTTP 的差异仅在
v0 协商应答轮次（docs/03 §7b）。

## 4. 走查：Web（8101）

- 登录（alice / bob / admin，密码 demo）——凭证经 `/api/v1/auth/login`
  单源校验，Web 自身不存密码表；
- 仓库列表 + 建仓表单（POST `/api/v1/repos`，走同一认证门）；
- 仓库页**分支列表 = 登录者视角**：alice 登录见 `alice/feat`，bob 登录不见
  ——与 git 客户端拿到的是同一份过滤广告（个人分支可见性最直观的演示）；
- 路径面包屑文件浏览 + blob 预览，`?ref=` 切分支；
- 边界（诚实清单，docs/13 §5）：**分支**可见性在 Web 上是强制的（分支
  列表来自过滤广告）；**目录**级限制只在 git wire（clone `/tree/secret`
  → 403）——Web 文件浏览走 RPC 读面（仅认证门），demo 里 alice 页面上
  仍能看到 `/secret` 列表，这正是已登记的 M5 后续项；
- 会话存 web 进程内存（HttpOnly cookie）：重启 web 容器 = 全员重新登录。

## 5. 数据持久化与清理

```bash
docker compose down        # 卷保留：再 up 数据还在（换策略文件也复用同一卷）
docker compose down -v     # 连 banyan-data 一起删 = 完全重置
```

## 6. 常见坑（实测记录）

| 症状 | 原因与处置 |
|---|---|
| 测匿名 401 却克隆成功 | 宿主机全局 `credential.helper=manager`（Windows 常见）静默补缓存凭证——命令加 `-c credential.helper=` |
| URL 不带凭证时命令"挂住" | git 交互式询问用户名密码——脚本场景加 `GIT_TERMINAL_PROMPT=0` |
| 首次 build 失败于 `go mod download` / `npm ci` | 默认源被墙——`--build-arg GOPROXY=…` / `NPM_REGISTRY=…`（§1） |
| 8100 探测到"别的 banyan" | 集群 demo 等旧容器未清——`docker ps` 核对，`docker compose -f docker-compose.cluster.yml down -v` 后重试 |
| 改了 web 代码要重建 | `docker compose build --build-arg NPM_REGISTRY=… web && docker compose up -d web`——直接 `up -d --build web` 会连带重建 banyan（又需要 GOPROXY 镜像）；web 会话在内存，重建即全员登出 |

## 7. 本地开发（不经 Docker）

```bash
# 服务端：显式 flag 覆盖 YAML（默认 < -config < flags），root 落本地目录
go run ./cmd/banyand -config demo/auth-demo.yaml -listen 127.0.0.1:8097 -root ./demo-data

# Web：工具链仅 tsc，运行时零依赖
cd web && npm install && npm run build
BANYAN_URL=http://127.0.0.1:8097 PORT=3000 npm start
```
