// Package derived 是派生数据引擎（docs/04-derived-data.md）。
// 契约：纯函数 + 记忆化（键=输入版本+查询）+ 可订阅增量重算（M3+）。
package derived

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/model"
)

// CommitGraph 是 commit 图派生数据：generation number（git commit-graph
// 算法，docs/04 §1）支撑 O(log n) merge-base。
type CommitGraph interface {
	// Generation 返回 commit 的 generation number（根为 1）。
	Generation(ctx context.Context, repo string, oid model.OID) (uint32, error)
	// MergeBase 返回最近公共祖先；无公共祖先返回零值。
	MergeBase(ctx context.Context, repo string, a, b model.OID) (model.OID, error)
}

// PathHistory 是路径历史派生数据：commits(c, /p) 的递归定义（docs/04 §1）。
type PathHistory interface {
	History(ctx context.Context, repo string, path string, since model.OID, limit int) ([]model.OID, error)
}

// BlameHunk 是 blame 的一块归因区。
type BlameHunk struct {
	Start, End  int    // 行区间 [Start, End)
	Commit      model.OID
}

// Blame 是 blame 派生数据：Scion 的文件级 diff 递归归并算法（docs/04 §1）。
type Blame interface {
	Blame(ctx context.Context, repo string, rev model.OID, path string) ([]BlameHunk, error)
}

// TODO(M2): commit-graph 首个实现（push 后增量急算）。
// TODO(M2): path-history 实现（懒算 + 位图）。
// TODO(M3): blame 实现（递归 + 记忆化缓存）。
