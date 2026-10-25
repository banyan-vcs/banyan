// Package derived is the derived-data engine (docs/08-derived-data.md).
// Contract: pure functions + memoization (key = input version + query) + subscribable incremental recomputation (M3+).
package derived

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/model"
)

// CommitGraph is commit-graph derived data: generation numbers (the git
// commit-graph algorithm, docs/08 §1) support O(log n) merge-base.
type CommitGraph interface {
	// Generation returns the commit's generation number (root = 1).
	Generation(ctx context.Context, repo string, oid model.OID) (uint32, error)
	// MergeBase returns the nearest common ancestor; zero value when none.
	MergeBase(ctx context.Context, repo string, a, b model.OID) (model.OID, error)
}

// PathHistory is path-history derived data: the recursive definition of commits(c, /p) (docs/08 §1).
type PathHistory interface {
	History(ctx context.Context, repo string, path string, since model.OID, limit int) ([]model.OID, error)
}

// BlameHunk is one attribution region of a blame.
type BlameHunk struct {
	Start, End int // line range [Start, End)
	Commit     model.OID
}

// Blame is blame derived data: Scion's file-level diff recursive merge algorithm (docs/08 §1).
type Blame interface {
	Blame(ctx context.Context, repo string, rev model.OID, path string) ([]BlameHunk, error)
}

// TODO(M2): first commit-graph implementation (eager incremental after push).
// TODO(M2): path-history implementation (lazy + bitmaps).
// TODO(M3): blame implementation (recursion + memoized cache).
