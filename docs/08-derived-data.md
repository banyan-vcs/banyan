# 08 · 派生数据引擎（derived）

派生数据的统一方法论："递归定义，可选急算或懒算，可选同步或异步最终一致。"

banyan 中一切"从对象与时间线推出的答案"都是派生数据。派生数据引擎（`internal/derived/`）统一四个接口约束：

```go
// 派生数据通用契约
type Derived[T any] interface {
    // 纯函数性：输入（对象/时间线状态）确定 ⇒ 输出确定
    // 记忆化：以 (输入版本, 查询) 为缓存键
    // 可订阅：上游前进一步，增量重算（M3+）
    Get(ctx, query) (T, error)
}
```

## 1. 派生数据清单

| 派生数据 | 定义（递归形式） | 服务端 RPC | 急算/懒算 | 阶段 |
|---|---|---|---|---|
| **commit-graph** | 节点 = commit，边 = parent；generation = max(parents.gen)+1 | merge-base RPC | push 后增量急算 | M2 |
| **merge-base(a,b)** | gca：gen(a) 与 gen(b) 对齐后上溯的公共节点 | log/diff 依赖 | 懒算 + 记忆化 | M2 |
| **diff(c1, c2)** | tree 递归对比（同锚点直接比；跨锚点先投影到公共锚点） | `diff` RPC | 懒算 | M1 |
| **路径历史 /path** | commits(c, /p) = 若 c.tree@/p ≠ c.parents.tree@/p 则含 c ∪ 各 parent 的 commits(...) | `file-history` RPC | 懒算 + 位图（M3） | M2 |
| **blame(/p, rev)** | blame = { 块 → 引入 commit }；对路径历史做 diff3 递归归并 | `blame` RPC | 懒算 + 记忆化 | M3 |
| **subtree split(root, P)** | split(c) = c.tree 提升 P 子树（01 §5.2） | 虚拟仓库 clone | push 后急算（缓存） | M1 |
| **可达性位图** | reach(c) = {c} ∪ reach(parents)；按 timeline head 存位图 | upload-pack 闭包计算 | repack 时急算 | M3 |

## 2. 一致性级别

- **读路径派生**（diff/blame/file-history）：懒算 + 记忆化，最终一致可接受（上游前进一步即重算增量）。
- **写路径派生**（subtree split 缓存、组装）：push 后同步或紧随其后急算——虚拟仓库 clone 依赖它，不可滞后太多。
- **性能派生**（位图、commit-graph）：repack 时机急算，滞后只影响速度不影响正确性。

## 3. 骨架对应

`internal/derived/`：`commitgraph.go`、`pathhistory.go`、`blame.go` 仅定义接口与算法注释，M2/M3 填充。`model/projection.go` 中的 subtree split 是第一个落地的派生数据（M1 就需要）。
