// T69 — remote surface (docs/19 slice 9). FetchSourceBranch copies a branch
// inside the object plane; FetchRemote and CreateRepositoryFromURL speak the
// real smart-HTTP protocol as a client (go-git transport) — exercised here
// against banyan's own smart-HTTP face, a cross-implementation interop test
// of the server side as well.
package gitalyrpc

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT69GitalyFetchSurface(t *testing.T) {
	support.NeedGit(t)
	s, meta, objects := support.NewServerPlane(t)
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
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, src+"/readme.md", "source of truth\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "c1 base")
	support.Git(t, src, "checkout", "-q", "-b", "feature")
	support.Write(t, src+"/feature.txt", "feature work\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "c2 feature")
	support.Git(t, src, "checkout", "-q", "main")
	support.Git(t, src, "push", "-q", s.RepoURL("t69"), "main", "feature")

	work := t.TempDir()
	t.Run("fetch source branch copies across repositories", func(t *testing.T) {
		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t69-copy"}
		_ = support.SeedRepo(t, s, "t69-copy") // an unrelated seed repo
		_, err := rs.FetchSourceBranch(ctx, &gitalypb.FetchSourceBranchRequest{
			Repository:       dest,
			SourceRepository: &gitalypb.Repository{StorageName: "default", RelativePath: "t69"},
			SourceBranch:     []byte("feature"),
			TargetRef:        []byte("refs/heads/incoming"),
		})
		require.NoError(t, err)
		out := support.Git(t, src, "ls-remote", s.RepoURL("t69-copy"), "refs/heads/incoming")
		featureTip := strings.Fields(support.Git(t, src, "rev-parse", "feature"))[0]
		assert.Contains(t, out, featureTip)
	})

	t.Run("fetch remote pulls banyan's own smart-http face", func(t *testing.T) {
		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t69-remote"}
		_ = support.SeedRepo(t, s, "t69-remote")
		_, err := rs.FetchRemote(ctx, &gitalypb.FetchRemoteRequest{
			Repository: dest,
			RemoteParams: &gitalypb.Remote{Url: s.RepoURL("t69")},
		})
		require.NoError(t, err)
		// The freshly created feature ref carries the source OID verbatim;
		// main force-replaces the seed tip, so the woven view rebuilds it as
		// a tree-identical twin (the dual-view rule, docs/19 §6) — assert
		// content, not OID, for main.
		out := support.Git(t, src, "ls-remote", s.RepoURL("t69-remote"))
		featureTip := strings.Fields(support.Git(t, src, "rev-parse", "feature"))[0]
		assert.Contains(t, out, featureTip)
		cloned := work + "/remote-clone"
		support.Git(t, work, "clone", "-q", s.RepoURL("t69-remote"), "remote-clone")
		assert.Equal(t, "source of truth\n", support.Git(t, cloned, "show", "origin/main:readme.md"))
	})

	t.Run("create repository from url seeds in one call", func(t *testing.T) {
		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t69-fromurl"}
		_, err := rs.CreateRepositoryFromURL(ctx, &gitalypb.CreateRepositoryFromURLRequest{
			Repository: dest, Url: s.RepoURL("t69")})
		require.NoError(t, err)
		cloned := work + "/from-url"
		support.Git(t, work, "clone", "-q", s.RepoURL("t69-fromurl"), "from-url")
		assert.Equal(t, "c1 base\n", support.Git(t, cloned, "log", "-1", "--format=%s", "origin/main"))
	})
}
