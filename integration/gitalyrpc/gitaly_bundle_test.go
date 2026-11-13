// T68 — bundle surface (docs/integration/gitaly-compat.md slice 9). Git is the oracle on both
// directions: banyan-created bundles must pass `git bundle verify` and
// clone bit-identically, and git-created bundles must seed a banyan
// repository that clones back with matching refs and content.
package gitalyrpc

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT68GitalyBundleSurface(t *testing.T) {
	support.NeedGit(t)
	s, meta, objects := support.NewServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t68"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t68")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	rs := gitalypb.NewRepositoryServiceClient(conn)

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, src+"/readme.md", "bundle me\n")
	support.Write(t, src+"/lib/util.go", "package lib\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "c1 seed")
	support.Write(t, src+"/lib/util.go", "package lib\n\n// evolved\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "c2 evolve")
	support.Git(t, src, "tag", "-a", "v1.0", "-m", "release one")
	support.Git(t, src, "push", "-q", s.RepoURL("t68"), "main", "--tags")

	work := t.TempDir()
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "t68"}

	t.Run("create bundle passes git bundle verify and clones", func(t *testing.T) {
		stream, err := rs.CreateBundle(ctx, &gitalypb.CreateBundleRequest{Repository: repo})
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

		bundlePath := filepath.Join(work, "banyan.bundle")
		require.NoError(t, os.WriteFile(bundlePath, raw, 0o644))
		out := support.Git(t, src, "bundle", "verify", bundlePath)
		assert.Contains(t, out, "v1.0", "annotated tag advertises")
		assert.Contains(t, out, "main", "branch advertises")

		seeded := filepath.Join(work, "from-banyan")
		support.Git(t, work, "clone", "-q", bundlePath, "from-banyan")
		assert.Equal(t, "c2 evolve\n", lastSubject(t, seeded))
		assert.Equal(t, "package lib\n\n// evolved\n", readRepoFile(t, seeded, "lib/util.go"))
		assert.Len(t, branches(t, seeded), 1)
	})

	t.Run("create bundle from ref list restricts refs", func(t *testing.T) {
		cstream, err := rs.CreateBundleFromRefList(ctx)
		require.NoError(t, err)
		require.NoError(t, cstream.Send(&gitalypb.CreateBundleFromRefListRequest{
			Repository: repo, Patterns: [][]byte{[]byte("refs/heads/*")}}))
		require.NoError(t, cstream.CloseSend())
		var raw []byte
		for {
			resp, rerr := cstream.Recv()
			if rerr == io.EOF {
				break
			}
			require.NoError(t, rerr)
			raw = append(raw, resp.GetData()...)
		}
		bundlePath := filepath.Join(work, "heads-only.bundle")
		require.NoError(t, os.WriteFile(bundlePath, raw, 0o644))
		out := support.Git(t, src, "bundle", "verify", bundlePath)
		assert.NotContains(t, out, "refs/tags/v1.0", "tags excluded by pattern")
	})

	t.Run("fetch bundle seeds from a git-made bundle", func(t *testing.T) {
		bundlePath := filepath.Join(work, "git-made.bundle")
		support.Git(t, src, "bundle", "create", "-q", bundlePath, "--all")
		data, err := os.ReadFile(bundlePath)
		require.NoError(t, err)

		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t68-seeded"}
		_, err = rs.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: dest})
		require.NoError(t, err)
		fstream, err := rs.FetchBundle(ctx)
		require.NoError(t, err)
		require.NoError(t, fstream.Send(&gitalypb.FetchBundleRequest{Repository: dest, Data: data}))
		require.NoError(t, fstream.CloseSend())
		_, err = fstream.CloseAndRecv()
		require.NoError(t, err)

		seeded := filepath.Join(work, "from-git")
		support.Git(t, work, "clone", "-q", s.RepoURL("t68-seeded"), "from-git")
		assert.Equal(t, "c2 evolve\n", lastSubject(t, seeded))
		assert.Equal(t, "package lib\n\n// evolved\n", readRepoFile(t, seeded, "lib/util.go"))
	})

	t.Run("create repository from bundle", func(t *testing.T) {
		bundlePath := filepath.Join(work, "git-made.bundle")
		data, err := os.ReadFile(bundlePath)
		require.NoError(t, err)

		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t68-frombundle"}
		bstream, err := rs.CreateRepositoryFromBundle(ctx)
		require.NoError(t, err)
		require.NoError(t, bstream.Send(&gitalypb.CreateRepositoryFromBundleRequest{Repository: dest, Data: data}))
		require.NoError(t, bstream.CloseSend())
		_, err = bstream.CloseAndRecv()
		require.NoError(t, err)

		seeded := filepath.Join(work, "from-bundle")
		support.Git(t, work, "clone", "-q", s.RepoURL("t68-frombundle"), "from-bundle")
		assert.Equal(t, "c2 evolve\n", lastSubject(t, seeded))
		// default branch from the bundle HEAD resolves to main.
		assert.Equal(t, "package lib\n\n// evolved\n", readRepoFile(t, seeded, "lib/util.go"))
	})

	t.Run("malformed bundle is rejected", func(t *testing.T) {
		dest := &gitalypb.Repository{StorageName: "default", RelativePath: "t68-badbundle"}
		_, err := rs.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: dest})
		require.NoError(t, err)
		fstream, err := rs.FetchBundle(ctx)
		require.NoError(t, err)
		require.NoError(t, fstream.Send(&gitalypb.FetchBundleRequest{
			Repository: dest, Data: []byte("this is not a bundle\n")}))
		require.NoError(t, fstream.CloseSend())
		_, err = fstream.CloseAndRecv()
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}

func lastSubject(t *testing.T, repo string) string {
	t.Helper()
	return support.Git(t, repo, "log", "-1", "--format=%s")
}

func readRepoFile(t *testing.T, repo, path string) string {
	t.Helper()
	return support.Git(t, repo, "show", "HEAD:"+path)
}

func branches(t *testing.T, repo string) []string {
	t.Helper()
	out := support.Git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/")
	var names []string
	for _, l := range splitLines(out) {
		if l != "" {
			names = append(names, l)
		}
	}
	return names
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

