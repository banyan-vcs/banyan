// T70 — conflicts, fast-export, remote mirror and fork (docs/19 closing
// slices). Git is the oracle: conflict markers match `git merge` output,
// the fast-export stream round-trips through `git fast-import`, the mirror
// push lands on banyan's own smart-HTTP face, forks clone identically.
package integration

import (
	"bytes"
	"context"
	"os/exec"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT70GitalyClosingSurfaces(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t70"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t70")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	rs := gitalypb.NewRepositoryServiceClient(conn)
	cs := gitalypb.NewConflictsServiceClient(conn)
	rm := gitalypb.NewRemoteServiceClient(conn)

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/shared.txt", "base\nline\nend\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c1 base")
	git(t, src, "checkout", "-q", "-b", "feature")
	write(t, src+"/shared.txt", "base\nfeature line\nend\n")
	write(t, src+"/feature-only.txt", "feature\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c2 feature")
	git(t, src, "checkout", "-q", "main")
	write(t, src+"/shared.txt", "base\nmain line\nend\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c3 main")
	git(t, src, "tag", "-a", "v1.0", "-m", "release")
	git(t, src, "push", "-q", s.repoURL("t70"), "main", "feature", "--tags")

	work := t.TempDir()
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "t70"}
	mainTip := strings.Fields(git(t, src, "rev-parse", "main"))[0]
	featureTip := strings.Fields(git(t, src, "rev-parse", "feature"))[0]

	t.Run("list conflicts carries markers", func(t *testing.T) {
		stream, err := cs.ListConflictFiles(ctx, &gitalypb.ListConflictFilesRequest{
			Repository:      repo,
			OurCommitOid:    mainTip,
			TheirCommitOid:  featureTip,
		})
		require.NoError(t, err)
		var path string
		var body []byte
		for {
			resp, rerr := stream.Recv()
			if rerr == io.EOF {
				break
			}
			require.NoError(t, rerr)
			for _, f := range resp.GetFiles() {
				switch payload := f.GetConflictFilePayload().(type) {
				case *gitalypb.ConflictFile_Header:
					path = string(payload.Header.GetOurPath())
				case *gitalypb.ConflictFile_Content:
					body = append(body, payload.Content...)
				}
			}
		}
		assert.Equal(t, "shared.txt", path)
		assert.Contains(t, string(body), "<<<<<<<")
		assert.Contains(t, string(body), "main line")
		assert.Contains(t, string(body), "feature line")
		assert.Contains(t, string(body), ">>>>>>>")
	})

	t.Run("resolve conflicts lands the merge", func(t *testing.T) {
		filesJSON := `[{"old_path":"shared.txt","new_path":"shared.txt","content":"base\nresolved line\nend\n"}]`
		rstream, err := cs.ResolveConflicts(ctx)
		require.NoError(t, err)
		require.NoError(t, rstream.Send(&gitalypb.ResolveConflictsRequest{
			ResolveConflictsRequestPayload: &gitalypb.ResolveConflictsRequest_Header{
				Header: &gitalypb.ResolveConflictsRequestHeader{
					Repository:     repo,
					OurCommitOid:   mainTip,
					TheirCommitOid: featureTip,
					TargetBranch:   []byte("merged"),
					CommitMessage:  []byte("merge with resolution\n"),
					User:           &gitalypb.User{GlId: "u1", Name: []byte("Resolver"), Email: []byte("r@x")},
				},
			},
		}))
		require.NoError(t, rstream.Send(&gitalypb.ResolveConflictsRequest{
			ResolveConflictsRequestPayload: &gitalypb.ResolveConflictsRequest_FilesJson{FilesJson: []byte(filesJSON)},
		}))
		require.NoError(t, rstream.CloseSend())
		resp, err := rstream.CloseAndRecv()
		require.NoError(t, err)
		assert.Empty(t, resp.GetResolutionError())

		out := git(t, src, "ls-remote", s.repoURL("t70"), "refs/heads/merged")
		assert.Contains(t, out, "refs/heads/merged")
		merged := work + "/merged"
		git(t, work, "clone", "-q", "-b", "merged", s.repoURL("t70"), "merged")
		assert.Equal(t, "base\nresolved line\nend\n", readRepoFile(t, merged, "shared.txt"))
		assert.Equal(t, "feature\n", readRepoFile(t, merged, "feature-only.txt"), "non-conflicting theirs side auto-merges")
	})

	t.Run("fast export round-trips through git fast-import", func(t *testing.T) {
		stream, err := rs.FastExport(ctx, &gitalypb.FastExportRequest{Repository: repo})
		require.NoError(t, err)
		var raw []byte
		for {
			resp, rerr := stream.Recv()
			if rerr == io.EOF {
				break
			}
			require.NoError(t, rerr)
			raw = append(raw, resp.GetData()...)
		}
		require.NotEmpty(t, raw)
		head := raw
		if len(head) > 400 {
			head = head[:400]
		}
		t.Log("stream head:", strings.ReplaceAll(string(head), "\n", " | "))

		imported := work + "/imported"
		git(t, work, "init", "-q", "-b", "main", "imported")
		cmd := exec.Command("git", "-C", imported, "fast-import", "--quiet")
		cmd.Stdin = bytes.NewReader(raw)
		out, err2 := cmd.CombinedOutput()
		require.NoError(t, err2, "fast-import: %s", out)
		// The stream names refs by the source branch names.
		assert.Equal(t, "base\nmain line\nend\n", git(t, imported, "show", "refs/heads/main:shared.txt"))
	})

	t.Run("update remote mirror pushes to banyan itself", func(t *testing.T) {
		dest := "t70-mirror"
		git(t, src, "push", "-q", s.repoURL(dest), "main:refs/heads/seed")
		mstream, err := rm.UpdateRemoteMirror(ctx)
		require.NoError(t, err)
		require.NoError(t, mstream.Send(&gitalypb.UpdateRemoteMirrorRequest{
			Repository: repo,
			Remote: &gitalypb.UpdateRemoteMirrorRequest_Remote{
				Url: s.repoURL(dest),
			},
		}))
		require.NoError(t, mstream.CloseSend())
		_, err = mstream.CloseAndRecv()
		require.NoError(t, err)

		out := git(t, src, "ls-remote", s.repoURL(dest))
		assert.Contains(t, out, "refs/heads/main")
		assert.Contains(t, out, "refs/heads/feature")
	})

	t.Run("create fork clones every ref", func(t *testing.T) {
		_, err := rs.CreateFork(ctx, &gitalypb.CreateForkRequest{
			Repository:      &gitalypb.Repository{StorageName: "default", RelativePath: "t70-fork"},
			SourceRepository: repo,
		})
		require.NoError(t, err)
		forked := work + "/forked"
		git(t, work, "clone", "-q", s.repoURL("t70-fork"), "forked")
		assert.Equal(t, "base\nmain line\nend\n", readRepoFile(t, forked, "shared.txt"))
		out := git(t, src, "ls-remote", s.repoURL("t70-fork"))
		assert.Contains(t, out, "refs/heads/main")
		assert.Contains(t, out, "refs/heads/feature")
		assert.Contains(t, out, "refs/tags/", "annotated tags ride along")
	})

	t.Run("find remote probes", func(t *testing.T) {
		out, err := rm.FindRemoteRepository(ctx, &gitalypb.FindRemoteRepositoryRequest{
			Remote: s.repoURL("t70")})
		require.NoError(t, err)
		assert.True(t, out.GetExists())

		root, err := rm.FindRemoteRootRef(ctx, &gitalypb.FindRemoteRootRefRequest{
			RemoteUrl: s.repoURL("t70")})
		require.NoError(t, err)
		assert.Equal(t, "refs/heads/main", root.GetRef())
	})

}
