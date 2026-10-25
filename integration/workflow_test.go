// workflow_test.go — T41/T42: the M4 directory-level workflow (docs/15)
// with the real git CLI. T41 pins the per-timeline queue under a same-
// directory burst: every push lands, zero rejections. T42 pins convergence:
// divergent chains replay server-side (pure-Go merge-ort subset) — disjoint
// changes merge automatically, same-file collisions fail that ticket in
// isolation, and the recovered client keeps working (T18 semantics hold).
package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// newConvergeServer is newServer with M4 convergence enabled (docs/15).
func newConvergeServer(t *testing.T) *server {
	t.Helper()
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gw := gitwire.NewServer(meta, objects, model.SHA1)
	gw.Convergence = true
	gw.Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &server{Server: srv}
}

func TestWorkflowBurst(t *testing.T) {
	needGit(t)
	s := newConvergeServer(t)
	seedRepo(t, s, "t41")

	// 8 clients each add their own file under lib/ and push CONCURRENTLY:
	// without convergence 7 get non-FF rejections; with the merge train all
	// 8 must land on the first try.
	const n = 8
	dirs := make([]string, n)
	for i := 0; i < n; i++ {
		dirs[i] = cloneRepo(t, s, s.treeURL("t41", "lib"))
		write(t, filepath.Join(dirs[i], fmt.Sprintf("f%d.txt", i)), fmt.Sprintf("content-%d\n", i))
		git(t, dirs[i], "add", "-A")
		git(t, dirs[i], "commit", "-qm", fmt.Sprintf("t41: file %d", i))
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	outs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = gitTry(dirs[i], "push", "-q", "origin", "main")
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "并发 push %d 不应被拒绝（merge train 应全部落地）:\n%s", i, outs[i])
	}

	fresh := cloneRepo(t, s, s.treeURL("t41", "lib"))
	for i := 0; i < n; i++ {
		assert.Equal(t, fmt.Sprintf("content-%d\n", i),
			readFile(t, filepath.Join(fresh, fmt.Sprintf("f%d.txt", i))),
			"客户端 %d 的文件应在合并后的视图里")
	}
	assert.Contains(t, readFile(t, filepath.Join(fresh, "a.txt")), "a-v1", "既有内容不得回退")
}

func TestWorkflowConvergence(t *testing.T) {
	needGit(t)
	s := newConvergeServer(t)
	seedRepo(t, s, "t42")

	// The service-side convergence semantics are pinned with rawPush
	// (hand-crafted receive-pack posts, the T8br infrastructure): the real
	// git client refuses to even SEND a sequential non-FF push ("fetch
	// first"), so sequential divergence never reaches the server — the
	// convergence surface is the concurrent-race window (T41), and these
	// cases drive the wire directly.
	t.Run("disjoint-divergence-converges", func(t *testing.T) {
		a := cloneRepo(t, s, s.treeURL("t42", "lib"))
		b := cloneRepo(t, s, s.treeURL("t42", "lib"))
		t0 := strings.TrimSpace(git(t, b, "rev-parse", "origin/main"))

		appendLine(t, filepath.Join(a, "a.txt"), "appended-by-a\n")
		git(t, a, "commit", "-qam", "a edits a.txt")
		git(t, a, "push", "-q", "origin", "main")

		// b diverged from t0: adds its own file. Raw post (the client would
		// refuse the send) → server-side convergence must land it.
		write(t, filepath.Join(b, "b.txt"), "by-b\n")
		git(t, b, "add", "-A")
		git(t, b, "commit", "-qm", "b adds b.txt")
		bHead := headOf(t, b)
		uploadObjects(t, s, "t42", b, bHead)
		report := rawPush(t, s, s.treeURL("t42", "lib"), t0, bHead, "refs/heads/main")
		require.NotContains(t, report, "ng", "分歧但不同文件的 push 应服务端收敛:\n%s", report)

		fresh := cloneRepo(t, s, s.treeURL("t42", "lib"))
		assert.Contains(t, readFile(t, filepath.Join(fresh, "a.txt")), "appended-by-a", "A 的改动应在")
		assert.Equal(t, "by-b\n", readFile(t, filepath.Join(fresh, "b.txt")), "B 的改动应在")
	})

	t.Run("same-file-conflict-isolated", func(t *testing.T) {
		seedRepo(t, s, "t42b")
		a := cloneRepo(t, s, s.treeURL("t42b", "lib"))
		b := cloneRepo(t, s, s.treeURL("t42b", "lib"))
		c := cloneRepo(t, s, s.treeURL("t42b", "lib"))
		t0 := strings.TrimSpace(git(t, b, "rev-parse", "origin/main"))

		appendLine(t, filepath.Join(a, "a.txt"), "same-line-A\n")
		git(t, a, "commit", "-qam", "a same edit")
		git(t, a, "push", "-q", "origin", "main")

		// b rewrites the SAME line differently → conflict, ticket isolated.
		write(t, filepath.Join(b, "a.txt"), "a-v1\nsame-line-B\n")
		git(t, b, "commit", "-qam", "b conflicting edit")
		bHead := headOf(t, b)
		uploadObjects(t, s, "t42b", b, bHead)
		report := rawPush(t, s, s.treeURL("t42b", "lib"), t0, bHead, "refs/heads/main")
		require.Contains(t, report, "ng refs/heads/main", "同文件同行的分歧 push 应冲突拒绝:\n%s", report)
		require.Contains(t, report, "conflict", "拒绝理由应为冲突:\n%s", report)

		// The train keeps rolling: c's disjoint push (also diverged from t0)
		// lands after the isolated conflict.
		write(t, filepath.Join(c, "c.txt"), "by-c\n")
		git(t, c, "add", "-A")
		git(t, c, "commit", "-qm", "c adds c.txt")
		cHead := headOf(t, c)
		uploadObjects(t, s, "t42b", c, cHead)
		report = rawPush(t, s, s.treeURL("t42b", "lib"), t0, cHead, "refs/heads/main")
		require.NotContains(t, report, "ng", "冲突票隔离后后续票应继续:\n%s", report)

		fresh := cloneRepo(t, s, s.treeURL("t42b", "lib"))
		assert.Contains(t, readFile(t, filepath.Join(fresh, "a.txt")), "same-line-A", "胜者内容保持")
		assert.Equal(t, "by-c\n", readFile(t, filepath.Join(fresh, "c.txt")), "后续票内容落地")

		// Recovery still works: b pulls the converged view; its edit truly
		// conflicts with the winner, so the rebase stops for a manual
		// resolution — resolve, continue, push (T18 path on a converged
		// chain).
		out, err := gitTry(b, "pull", "-q", "--rebase", "origin", "main")
		require.Error(t, err, "与胜者同文件同行的改动，rebase 应停下待人工解决:\n%s", out)
		write(t, filepath.Join(b, "a.txt"), "a-v1\nsame-line-A\nsame-line-B\n")
		git(t, b, "add", "a.txt")
		git(t, b, "-c", "core.editor=true", "rebase", "--continue")
		git(t, b, "push", "-q", "origin", "main")
		fresh2 := cloneRepo(t, s, s.treeURL("t42b", "lib"))
		assert.Equal(t, "a-v1\nsame-line-A\nsame-line-B\n", readFile(t, filepath.Join(fresh2, "a.txt")), "人工解决后的合并内容应落地")
	})
}
