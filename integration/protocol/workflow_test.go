// workflow_test.go — T41/T42: the M4 directory-level workflow (docs/15)
// with the real git CLI. T41 pins the per-timeline queue under a same-
// directory burst: every push lands, zero rejections. T42 pins convergence:
// divergent chains replay server-side (pure-Go merge-ort subset) — disjoint
// changes merge automatically, same-file collisions fail that ticket in
// isolation, and the recovered client keeps working (T18 semantics hold).
package protocol

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

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// newConvergeServer is newServer with M4 convergence enabled (docs/15).
func newConvergeServer(t *testing.T) *support.Server {
	t.Helper()
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(support.TempDirQuiet(t))
	nodes := []storage.Node{{ID: "t1", Local: true}}
	objects := storage.NewObjectService(ms, local, nodes, model.SHA1)
	mux := http.NewServeMux()
	gw := gitwire.NewServer(ms, objects, model.SHA1)
	gw.Convergence = true
	gw.Register(mux)
	rpc.NewServer(ms, objects, model.SHA1).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &support.Server{Server: srv}
}

func TestWorkflowBurst(t *testing.T) {
	support.NeedGit(t)
	s := newConvergeServer(t)
	support.SeedRepo(t, s, "t41")

	// 8 clients each add their own file under lib/ and push CONCURRENTLY:
	// without convergence 7 get non-FF rejections; with the merge train all
	// 8 must land on the first try.
	const n = 8
	dirs := make([]string, n)
	for i := 0; i < n; i++ {
		dirs[i] = support.CloneRepo(t, s, s.TreeURL("t41", "lib"))
		support.Write(t, filepath.Join(dirs[i], fmt.Sprintf("f%d.txt", i)), fmt.Sprintf("content-%d\n", i))
		support.Git(t, dirs[i], "add", "-A")
		support.Git(t, dirs[i], "commit", "-qm", fmt.Sprintf("t41: file %d", i))
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	outs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = support.GitTry(dirs[i], "push", "-q", "origin", "main")
		}(i)
	}
	wg.Wait()

	// The burst races git's own client-side freshness check: a member whose
	// info/refs GET observes a tip its local history does not contain
	// (another member already landed) is refused BY ITS OWN git client
	// ("fetch first") — the push never reaches the train. That window is
	// native git semantics, not a server rejection, and it widens under
	// CPU starvation (CI runners), so client-side refusals auto-heal with
	// pull --rebase + re-push (T18 semantics). What the train is pinned
	// on: every push the server actually receives lands on the first try —
	// any OTHER failure fails the test right here.
	pending := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			continue
		}
		if !strings.Contains(outs[i], "fetch first") {
			require.NoError(t, errs[i], "并发 push %d 不应被服务端拒绝（merge train 应全部落地）:\n%s", i, outs[i])
		}
		pending = append(pending, i)
	}
	for round := 0; len(pending) > 0 && round < 5; round++ {
		var rwg sync.WaitGroup
		for _, i := range pending {
			rwg.Add(1)
			go func(i int) {
				defer rwg.Done()
				outs[i], errs[i] = support.GitTry(dirs[i], "pull", "--rebase", "-q", "origin", "main")
				if errs[i] == nil {
					outs[i], errs[i] = support.GitTry(dirs[i], "push", "-q", "origin", "main")
				}
			}(i)
		}
		rwg.Wait()
		still := make([]int, 0, len(pending))
		for _, i := range pending {
			if errs[i] != nil && !strings.Contains(outs[i], "fetch first") {
				require.NoError(t, errs[i], "fetch-first 自愈轮 %d 的重推不应被服务端拒绝:\n%s", round, outs[i])
			}
			if errs[i] != nil {
				still = append(still, i)
			}
		}
		pending = still
	}
	require.Empty(t, pending, "fetch-first 自愈后仍有未落地的推送（5 轮）")

	fresh := support.CloneRepo(t, s, s.TreeURL("t41", "lib"))
	for i := 0; i < n; i++ {
		assert.Equal(t, fmt.Sprintf("content-%d\n", i),
			support.ReadFile(t, filepath.Join(fresh, fmt.Sprintf("f%d.txt", i))),
			"客户端 %d 的文件应在合并后的视图里")
	}
	assert.Contains(t, support.ReadFile(t, filepath.Join(fresh, "a.txt")), "a-v1", "既有内容不得回退")
}

func TestWorkflowConvergence(t *testing.T) {
	support.NeedGit(t)
	s := newConvergeServer(t)
	support.SeedRepo(t, s, "t42")

	// The service-side convergence semantics are pinned with rawPush
	// (hand-crafted receive-pack posts, the T8br infrastructure): the real
	// git client refuses to even SEND a sequential non-FF push ("fetch
	// first"), so sequential divergence never reaches the server — the
	// convergence surface is the concurrent-race window (T41), and these
	// cases drive the wire directly.
	t.Run("disjoint-divergence-converges", func(t *testing.T) {
		a := support.CloneRepo(t, s, s.TreeURL("t42", "lib"))
		b := support.CloneRepo(t, s, s.TreeURL("t42", "lib"))
		t0 := strings.TrimSpace(support.Git(t, b, "rev-parse", "origin/main"))

		support.AppendLine(t, filepath.Join(a, "a.txt"), "appended-by-a\n")
		support.Git(t, a, "commit", "-qam", "a edits a.txt")
		support.Git(t, a, "push", "-q", "origin", "main")

		// b diverged from t0: adds its own file. Raw post (the client would
		// refuse the send) → server-side convergence must land it.
		support.Write(t, filepath.Join(b, "b.txt"), "by-b\n")
		support.Git(t, b, "add", "-A")
		support.Git(t, b, "commit", "-qm", "b adds b.txt")
		bHead := support.HeadOf(t, b)
		uploadObjects(t, s, "t42", b, bHead)
		report := rawPush(t, s, s.TreeURL("t42", "lib"), t0, bHead, "refs/heads/main")
		require.NotContains(t, report, "ng", "分歧但不同文件的 push 应服务端收敛:\n%s", report)

		fresh := support.CloneRepo(t, s, s.TreeURL("t42", "lib"))
		assert.Contains(t, support.ReadFile(t, filepath.Join(fresh, "a.txt")), "appended-by-a", "A 的改动应在")
		assert.Equal(t, "by-b\n", support.ReadFile(t, filepath.Join(fresh, "b.txt")), "B 的改动应在")
	})

	t.Run("same-file-conflict-isolated", func(t *testing.T) {
		support.SeedRepo(t, s, "t42b")
		a := support.CloneRepo(t, s, s.TreeURL("t42b", "lib"))
		b := support.CloneRepo(t, s, s.TreeURL("t42b", "lib"))
		c := support.CloneRepo(t, s, s.TreeURL("t42b", "lib"))
		t0 := strings.TrimSpace(support.Git(t, b, "rev-parse", "origin/main"))

		support.AppendLine(t, filepath.Join(a, "a.txt"), "same-line-A\n")
		support.Git(t, a, "commit", "-qam", "a same edit")
		support.Git(t, a, "push", "-q", "origin", "main")

		// b rewrites the SAME line differently → conflict, ticket isolated.
		support.Write(t, filepath.Join(b, "a.txt"), "a-v1\nsame-line-B\n")
		support.Git(t, b, "commit", "-qam", "b conflicting edit")
		bHead := support.HeadOf(t, b)
		uploadObjects(t, s, "t42b", b, bHead)
		report := rawPush(t, s, s.TreeURL("t42b", "lib"), t0, bHead, "refs/heads/main")
		require.Contains(t, report, "ng refs/heads/main", "同文件同行的分歧 push 应冲突拒绝:\n%s", report)
		require.Contains(t, report, "conflict", "拒绝理由应为冲突:\n%s", report)

		// The train keeps rolling: c's disjoint push (also diverged from t0)
		// lands after the isolated conflict.
		support.Write(t, filepath.Join(c, "c.txt"), "by-c\n")
		support.Git(t, c, "add", "-A")
		support.Git(t, c, "commit", "-qm", "c adds c.txt")
		cHead := support.HeadOf(t, c)
		uploadObjects(t, s, "t42b", c, cHead)
		report = rawPush(t, s, s.TreeURL("t42b", "lib"), t0, cHead, "refs/heads/main")
		require.NotContains(t, report, "ng", "冲突票隔离后后续票应继续:\n%s", report)

		fresh := support.CloneRepo(t, s, s.TreeURL("t42b", "lib"))
		assert.Contains(t, support.ReadFile(t, filepath.Join(fresh, "a.txt")), "same-line-A", "胜者内容保持")
		assert.Equal(t, "by-c\n", support.ReadFile(t, filepath.Join(fresh, "c.txt")), "后续票内容落地")

		// Recovery still works: b pulls the converged view; its edit truly
		// conflicts with the winner, so the rebase stops for a manual
		// resolution — resolve, continue, push (T18 path on a converged
		// chain).
		out, err := support.GitTry(b, "pull", "-q", "--rebase", "origin", "main")
		require.Error(t, err, "与胜者同文件同行的改动，rebase 应停下待人工解决:\n%s", out)
		support.Write(t, filepath.Join(b, "a.txt"), "a-v1\nsame-line-A\nsame-line-B\n")
		support.Git(t, b, "add", "a.txt")
		support.Git(t, b, "-c", "core.editor=true", "rebase", "--continue")
		support.Git(t, b, "push", "-q", "origin", "main")
		fresh2 := support.CloneRepo(t, s, s.TreeURL("t42b", "lib"))
		assert.Equal(t, "a-v1\nsame-line-A\nsame-line-B\n", support.ReadFile(t, filepath.Join(fresh2, "a.txt")), "人工解决后的合并内容应落地")
	})
}
