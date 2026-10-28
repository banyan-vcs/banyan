// T63 — diff and path-scoped history surface (docs/19 slice 4): every
// answer byte-compares or set-compares against the git CLI's own output —
// git diff (patch body incl. index lines), --name-status, --numstat,
// show --stat, log -- <path>, log -1 -- <path>.
package integration

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT63GitalyDiffSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t63"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t63")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/a.txt", "line1\nline2\nline3\nline4\nline5\n")
	write(t, src+"/keep.txt", "kept\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c1 base")
	write(t, src+"/a.txt", "line1\nline2 changed\nline3\nline4\nline5 removed\nline6 new\n")
	write(t, src+"/sub/b.txt", "brand new file\nwith two lines\n")
	git(t, src, "rm", "-q", "keep.txt")
	write(t, src+"/gone.txt", "to be deleted next\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "feat: change a, add sub/b, drop keep")
	git(t, src, "rm", "-q", "gone.txt")
	git(t, src, "commit", "-qm", "chore: drop gone")
	git(t, src, "push", "-q", s.repoURL("proj"), "main")

	c1 := strings.TrimSpace(git(t, src, "rev-parse", "HEAD~2"))
	c2 := strings.TrimSpace(git(t, src, "rev-parse", "HEAD~1"))
	c3 := strings.TrimSpace(git(t, src, "rev-parse", "HEAD"))

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}
	ds := gitalypb.NewDiffServiceClient(conn)
	cs := gitalypb.NewCommitServiceClient(conn)

	t.Run("raw diff equals git diff", func(t *testing.T) {
		stream, err := ds.RawDiff(ctx, &gitalypb.RawDiffRequest{Repository: repo, LeftCommitId: c1, RightCommitId: c3})
		require.NoError(t, err)
		var got []byte
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			got = append(got, resp.GetData()...)
		}
		want := git(t, src, "diff", c1, c3)
		assert.Equal(t, want, string(got))
	})

	t.Run("commit delta equals name-status", func(t *testing.T) {
		stream, err := ds.CommitDelta(ctx, &gitalypb.CommitDeltaRequest{Repository: repo, LeftCommitId: c1, RightCommitId: c3})
		require.NoError(t, err)
		got := map[string]string{}
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, d := range resp.GetDeltas() {
				status := "M"
				if d.GetOldMode() == 0 {
					status = "A"
				} else if d.GetNewMode() == 0 {
					status = "D"
				}
				got[string(d.GetToPath())] = status
			}
		}
		want := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(git(t, src, "diff", "--name-status", c1, c3)), "\n") {
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) == 2 {
				want[parts[1]] = parts[0]
			}
		}
		assert.Equal(t, want, got)
	})

	t.Run("diff stats equal numstat", func(t *testing.T) {
		stream, err := ds.DiffStats(ctx, &gitalypb.DiffStatsRequest{Repository: repo, LeftCommitId: c1, RightCommitId: c3})
		require.NoError(t, err)
		got := map[string][2]int32{}
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, st := range resp.GetStats() {
				got[string(st.GetPath())] = [2]int32{st.GetAdditions(), st.GetDeletions()}
			}
		}
		want := map[string][2]int32{}
		for _, line := range strings.Split(strings.TrimSpace(git(t, src, "diff", "--numstat", c1, c3)), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 3 {
				a, d := 0, 0
				fmtSscan(parts[0], &a)
				fmtSscan(parts[1], &d)
				want[parts[2]] = [2]int32{int32(a), int32(d)}
			}
		}
		assert.Equal(t, want, got)
	})

	t.Run("commit stats equal show --stat", func(t *testing.T) {
		out, err := cs.CommitStats(ctx, &gitalypb.CommitStatsRequest{Repository: repo, Revision: []byte(c2)})
		require.NoError(t, err)
		// git show --numstat <c2>, summed
		sumA, sumD := 0, 0
		for _, line := range strings.Split(strings.TrimSpace(git(t, src, "show", "--numstat", "--format=", c2)), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 3 {
				a, d := 0, 0
				fmtSscan(parts[0], &a)
				fmtSscan(parts[1], &d)
				sumA += a
				sumD += d
			}
		}
		assert.Equal(t, int32(sumA), out.GetAdditions())
		assert.Equal(t, int32(sumD), out.GetDeletions())
	})

	t.Run("find commits by path matches git log", func(t *testing.T) {
		stream, err := cs.FindCommits(ctx, &gitalypb.FindCommitsRequest{
			Repository: repo, Revision: []byte("main"), Paths: [][]byte{[]byte("a.txt")}})
		require.NoError(t, err)
		var ids []string
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, c := range resp.GetCommits() {
				ids = append(ids, c.GetId())
			}
		}
		var want []string
		for _, line := range strings.Split(strings.TrimSpace(git(t, src, "log", "--format=%H", "--", "a.txt")), "\n") {
			if line != "" {
				want = append(want, strings.TrimSpace(line))
			}
		}
		assert.Equal(t, want, ids)
	})

	t.Run("last commit for path", func(t *testing.T) {
		out, err := cs.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
			Repository: repo, Revision: []byte("main"), Path: []byte("sub/b.txt")})
		require.NoError(t, err)
		want := strings.TrimSpace(git(t, src, "log", "-1", "--format=%H", "--", "sub/b.txt"))
		assert.Equal(t, want, out.GetCommit().GetId())
	})

	t.Run("commits by message", func(t *testing.T) {
		stream, err := cs.CommitsByMessage(ctx, &gitalypb.CommitsByMessageRequest{
			Repository: repo, Revision: []byte("main"), Query: "chore"})
		require.NoError(t, err)
		var ids []string
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, c := range resp.GetCommits() {
				ids = append(ids, c.GetId())
			}
		}
		require.Len(t, ids, 1)
		assert.Equal(t, c3, ids[0])
	})

	t.Run("tree entry streams blob content", func(t *testing.T) {
		stream, err := cs.TreeEntry(ctx, &gitalypb.TreeEntryRequest{
			Repository: repo, Revision: []byte("main"), Path: []byte("sub/b.txt")})
		require.NoError(t, err)
		var data []byte
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			data = append(data, resp.GetData()...)
		}
		assert.Equal(t, "brand new file\nwith two lines\n", string(data))
	})

	t.Run("patch id is stable", func(t *testing.T) {
		a, err := ds.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{Repository: repo, OldRevision: []byte(c1), NewRevision: []byte(c2)})
		require.NoError(t, err)
		b, err := ds.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{Repository: repo, OldRevision: []byte(c1), NewRevision: []byte(c2)})
		require.NoError(t, err)
		assert.NotEmpty(t, a.GetPatchId())
		assert.Equal(t, a.GetPatchId(), b.GetPatchId())
	})

	t.Run("find changed paths", func(t *testing.T) {
		stream, err := ds.FindChangedPaths(ctx, &gitalypb.FindChangedPathsRequest{
			Repository: repo,
			Requests: []*gitalypb.FindChangedPathsRequest_Request{{
				Type: &gitalypb.FindChangedPathsRequest_Request_TreeRequest_{
					TreeRequest: &gitalypb.FindChangedPathsRequest_Request_TreeRequest{
						LeftTreeRevision: c2, RightTreeRevision: c3}}}}})
		require.NoError(t, err)
		paths := map[string]bool{}
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, p := range resp.GetPaths() {
				paths[string(p.GetPath())] = true
			}
		}
		assert.Equal(t, map[string]bool{"gone.txt": true}, paths)
	})
}

func fmtSscan(s string, out *int) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	*out = n
}
