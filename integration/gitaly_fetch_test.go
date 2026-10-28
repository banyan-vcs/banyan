// T69 — remote surface (docs/19 slice 9). FetchSourceBranch copies a branch
// inside the object plane; FetchRemote and CreateRepositoryFromURL speak the
// real smart-HTTP protocol as a client (go-git transport) — exercised here
// against banyan's own smart-HTTP face, a cross-implementation interop test
// of the server side as well.
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

func TestT69GitalyFetchSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t69"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t69")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	rs := gitalypb.NewRepositoryServiceClient(conn)

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/readme.md", "source of truth\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c1 base")
	git(t, src, "checkout", "-q", "-b", "feature")
	write(t, src+"/feature.txt", "feature work\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c2 feature")
	git(t, src, "checkout", "-q", "main")
	git(t, src, "push", "-q", s.repoURL("t69"), "main", "feature")

	work := t.TempDir()
	t.Run("fetch source branch copies across repositories", func(t *testing.T) {
		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t69-copy"}
		_ = seedRepo(t, s, "t69-copy") // an unrelated seed repo
		_, err := rs.FetchSourceBranch(ctx, &gitalypb.FetchSourceBranchRequest{
			Repository:       dest,
			SourceRepository: &gitalypb.Repository{StorageName: "default", RelativePath: "t69"},
			SourceBranch:     []byte("feature"),
			TargetRef:        []byte("refs/heads/incoming"),
		})
		require.NoError(t, err)
		out := git(t, src, "ls-remote", s.repoURL("t69-copy"), "refs/heads/incoming")
		featureTip := strings.Fields(git(t, src, "rev-parse", "feature"))[0]
		assert.Contains(t, out, featureTip)
	})

	t.Run("fetch remote pulls banyan's own smart-http face", func(t *testing.T) {
		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t69-remote"}
		_ = seedRepo(t, s, "t69-remote")
		_, err := rs.FetchRemote(ctx, &gitalypb.FetchRemoteRequest{
			Repository: dest,
			RemoteParams: &gitalypb.Remote{Url: s.repoURL("t69")},
		})
		require.NoError(t, err)
		// The freshly created feature ref carries the source OID verbatim;
		// main force-replaces the seed tip, so the woven view rebuilds it as
		// a tree-identical twin (the dual-view rule, docs/19 §6) — assert
		// content, not OID, for main.
		out := git(t, src, "ls-remote", s.repoURL("t69-remote"))
		featureTip := strings.Fields(git(t, src, "rev-parse", "feature"))[0]
		assert.Contains(t, out, featureTip)
		cloned := work + "/remote-clone"
		git(t, work, "clone", "-q", s.repoURL("t69-remote"), "remote-clone")
		assert.Equal(t, "source of truth\n", git(t, cloned, "show", "origin/main:readme.md"))
	})

	t.Run("create repository from url seeds in one call", func(t *testing.T) {
		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t69-fromurl"}
		_, err := rs.CreateRepositoryFromURL(ctx, &gitalypb.CreateRepositoryFromURLRequest{
			Repository: dest, Url: s.repoURL("t69")})
		require.NoError(t, err)
		cloned := work + "/from-url"
		git(t, work, "clone", "-q", s.repoURL("t69-fromurl"), "from-url")
		assert.Equal(t, "c1 base\n", git(t, cloned, "log", "-1", "--format=%s", "origin/main"))
	})
}
