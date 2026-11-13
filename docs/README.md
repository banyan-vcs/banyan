# banyan 文档索引

按子模块组织（原按编号扁平排列；编号已退役，路径即语义）。

| 目录 | 内容 |
|---|---|
| [design/](design/overview.md) | 总览与叙事：[overview](design/overview.md) · [big-picture](design/big-picture.md)（锚定提交/织入）· [data-model](design/data-model.md) · [derived-data](design/derived-data.md)（派生视图）· [workflow](design/workflow.md)（M4 目录级工作流） |
| [storage/](storage/storage.md) | 存储面：[storage](storage/storage.md)（pack 分片/多副本）· [indexing-packing](storage/indexing-packing.md) · [ha](storage/ha.md)（集群/一致性） |
| [gateway/](gateway/git-compat.md) | 网关面：[git-compat](gateway/git-compat.md)（smart HTTP/SSH 兼容）· [service-api](gateway/service-api.md)（JSON API/gitaly 装配）· [authorization](gateway/authorization.md)（授权模型：静态/远程快照/SSO/多 OAuth provider §8d） |
| [web/](web/web-layer.md) | web 层：[web-layer](web/web-layer.md)（`banyand web`：三层独立站、会话库、OAuth、导入、gitlab 档位）· [console](web/console.md)（产品端控制面 `banyand console`，M6 设计稿） |
| [client/](client/cli.md) | 薄工作区 CLI（`banyan`） |
| [integration/](integration/gitaly-compat.md) | 平台集成：[gitaly-compat](integration/gitaly-compat.md)（gitaly gRPC 兼容）· [gitlab-integration](integration/gitlab-integration.md)（GitLab-CE 接入/子树控制面） |
| [operations/](operations/docker-demo.md) | 运维：[docker-demo](operations/docker-demo.md) · [performance](operations/performance.md)（基准与红线）· [testing](operations/testing.md)（测试地图） |
| [project/](project/roadmap.md) | 路线图 |
| [assets/](assets/architecture-simple.svg) | 架构图与头像 |

文档间引用约定：正文用仓库相对路径（如 `docs/gateway/authorization.md §8`）。
