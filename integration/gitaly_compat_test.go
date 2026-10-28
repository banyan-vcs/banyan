// T60 — gitaly protocol compat e2e (docs/19): the same object plane the git
// smart-HTTP path serves is read back through the gitaly gRPC frontend, and
// every answer must equal what the real git client computes locally. The
// oracle is git itself: refs from ls-remote, commits from rev-parse/log,
// trees from ls-tree, blobs from cat-file.
package integration

import (
	"context"
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT60GitalyCompatSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)

	// The gitaly frontend over the same plane.
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t60-token"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t60-token")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()

	// Seed: three commits, a feature branch, an annotated and a lightweight
	// tag — pushed through the real git client.
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/README.md", "readme\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c1: initial")
	write(t, src+"/lib/a.txt", "a-v1\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c2: add lib")
	write(t, src+"/lib/b.txt", "b-v1\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c3: add b")
	git(t, src, "tag", "-a", "v1.0", "-m", "release one")
	git(t, src, "tag", "lw")
	git(t, src, "push", "-q", s.repoURL("proj"), "main", "--tags")

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}
	rs := gitalypb.NewRefServiceClient(conn)
	cs := gitalypb.NewCommitServiceClient(conn)
	bs := gitalypb.NewBlobServiceClient(conn)
	rps := gitalypb.NewRepositoryServiceClient(conn)

	t.Run("ref surface matches ls-remote", func(t *testing.T) {
		remote := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(git(t, src, "ls-remote", s.repoURL("proj"))), "\n") {
			parts := strings.SplitN(line, "\t", 2)
			// ls-remote appends "^{}" peel lines for annotated tags — display
			// artifacts, not refs; ListRefs surfaces them via peel_tags.
			if len(parts) == 2 && !strings.HasSuffix(parts[1], "^{}") {
				remote[parts[1]] = parts[0]
			}
		}
		stream, err := rs.ListRefs(ctx, &gitalypb.ListRefsRequest{Repository: repo, Head: true, Patterns: [][]byte{[]byte("refs/*")}})
		require.NoError(t, err)
		served := map[string]string{}
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, r := range resp.GetReferences() {
				served[string(r.GetName())] = r.GetTarget()
			}
		}
		assert.Equal(t, remote, served)
	})

	t.Run("default branch", func(t *testing.T) {
		def, err := rs.FindDefaultBranchName(ctx, &gitalypb.FindDefaultBranchNameRequest{Repository: repo})
		require.NoError(t, err)
		assert.Equal(t, "main", string(def.GetName()))
	})

	t.Run("commit identity and history", func(t *testing.T) {
		head := headOf(t, src)
		c, err := cs.FindCommit(ctx, &gitalypb.FindCommitRequest{Repository: repo, Revision: []byte("main")})
		require.NoError(t, err)
		require.NotNil(t, c.GetCommit())
		assert.Equal(t, head, c.GetCommit().GetId())
		assert.Equal(t, "c3: add b", string(c.GetCommit().GetSubject()))
		assert.Equal(t, "it", string(c.GetCommit().GetAuthor().GetName()))

		c1 := strings.TrimSpace(git(t, src, "rev-parse", "HEAD~2"))
		v, err := cs.CommitIsAncestor(ctx, &gitalypb.CommitIsAncestorRequest{Repository: repo, AncestorId: c1, ChildId: head})
		require.NoError(t, err)
		assert.True(t, v.GetValue())
		v, err = cs.CommitIsAncestor(ctx, &gitalypb.CommitIsAncestorRequest{Repository: repo, AncestorId: head, ChildId: c1})
		require.NoError(t, err)
		assert.False(t, v.GetValue())

		stream, err := cs.ListCommits(ctx, &gitalypb.ListCommitsRequest{Repository: repo, Revisions: []string{"main"}})
		require.NoError(t, err)
		var ids []string
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, gc := range resp.GetCommits() {
				ids = append(ids, gc.GetId())
			}
		}
		var want []string
		for _, line := range strings.Split(strings.TrimSpace(git(t, src, "rev-list", "main")), "\n") {
			want = append(want, strings.TrimSpace(line))
		}
		sort.Strings(ids)
		sort.Strings(want)
		assert.Equal(t, want, ids)

		n, err := cs.CountCommits(ctx, &gitalypb.CountCommitsRequest{Repository: repo, Revision: []byte("main")})
		require.NoError(t, err)
		assert.Equal(t, int32(countCommits(t, src)), n.GetCount())
	})

	t.Run("tags peel like git", func(t *testing.T) {
		tagOID := strings.TrimSpace(git(t, src, "rev-parse", "v1.0"))
		peeled := strings.TrimSpace(git(t, src, "rev-parse", "v1.0^{commit}"))
		tag, err := rs.FindTag(ctx, &gitalypb.FindTagRequest{Repository: repo, TagName: []byte("v1.0")})
		require.NoError(t, err)
		assert.Equal(t, tagOID, tag.GetTag().GetId())
		assert.Equal(t, peeled, tag.GetTag().GetTargetCommit().GetId())

		lwOID := strings.TrimSpace(git(t, src, "rev-parse", "lw"))
		tag, err = rs.FindTag(ctx, &gitalypb.FindTagRequest{Repository: repo, TagName: []byte("lw")})
		require.NoError(t, err)
		assert.Equal(t, lwOID, tag.GetTag().GetId())
		assert.Equal(t, lwOID, tag.GetTag().GetTargetCommit().GetId())
	})

	t.Run("tree entries mirror ls-tree", func(t *testing.T) {
		stream, err := cs.GetTreeEntries(ctx, &gitalypb.GetTreeEntriesRequest{Repository: repo, Revision: []byte("main"), Path: []byte("lib")})
		require.NoError(t, err)
		var entries []*gitalypb.TreeEntry
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			entries = append(entries, resp.GetEntries()...)
		}
		names := map[string]gitalypb.TreeEntry_EntryType{}
		for _, e := range entries {
			names[string(e.GetPath())] = e.GetType()
		}
		assert.Equal(t, gitalypb.TreeEntry_BLOB, names["lib/a.txt"])
		assert.Equal(t, gitalypb.TreeEntry_BLOB, names["lib/b.txt"])
	})

	t.Run("blob bytes equal cat-file", func(t *testing.T) {
		oid := blobOf(t, src, "lib/a.txt")
		stream, err := bs.GetBlob(ctx, &gitalypb.GetBlobRequest{Repository: repo, Oid: oid})
		require.NoError(t, err)
		var data []byte
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			data = append(data, resp.GetData()...)
		}
		want := git(t, src, "cat-file", "blob", oid)
		assert.Equal(t, want, string(data))
	})

	t.Run("ref writes surface back to git", func(t *testing.T) {
		head := headOf(t, src)
		upd, err := rs.UpdateReferences(ctx)
		require.NoError(t, err)
		require.NoError(t, upd.Send(&gitalypb.UpdateReferencesRequest{Repository: repo,
			Updates: []*gitalypb.UpdateReferencesRequest_Update{{
				Reference: []byte("refs/heads/deploy"), NewObjectId: []byte(head)}}}))
		_, err = upd.CloseAndRecv()
		require.NoError(t, err)
		remote := git(t, src, "ls-remote", s.repoURL("proj"), "refs/heads/deploy")
		assert.Contains(t, remote, head)

		_, err = rs.DeleteRefs(ctx, &gitalypb.DeleteRefsRequest{Repository: repo, Refs: [][]byte{[]byte("refs/heads/deploy")}})
		require.NoError(t, err)
		remote = git(t, src, "ls-remote", s.repoURL("proj"), "refs/heads/deploy")
		assert.Empty(t, strings.TrimSpace(remote))
	})

	t.Run("create repository and serve it", func(t *testing.T) {
		// GitLab's canonical hashed path: the trailing oid becomes the banyan
		// repo name (zero-translation addressing, docs/19 §2).
		hashed := &gitalypb.Repository{StorageName: "default", RelativePath: "@hashed/6b/86/6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b.git"}
		mapped := "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b"
		_, err := rps.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: hashed, DefaultBranch: []byte("master")})
		require.NoError(t, err)
		ex, err := rps.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: hashed})
		require.NoError(t, err)
		assert.True(t, ex.GetExists())
		def, err := rs.FindDefaultBranchName(ctx, &gitalypb.FindDefaultBranchNameRequest{Repository: hashed})
		require.NoError(t, err)
		assert.Equal(t, "master", string(def.GetName()))

		// push through gitwire into the gitaly-created repo, read it back
		git(t, src, "push", "-q", s.URL+"/r/"+mapped, "main:master")
		b, err := rs.FindBranch(ctx, &gitalypb.FindBranchRequest{Repository: hashed, Name: []byte("master")})
		require.NoError(t, err)
		assert.Equal(t, headOf(t, src), b.GetBranch().GetTargetCommit().GetId())
	})

	t.Run("object format and local branches", func(t *testing.T) {
		of, err := rps.ObjectFormat(ctx, &gitalypb.ObjectFormatRequest{Repository: repo})
		require.NoError(t, err)
		assert.Equal(t, gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1, of.GetFormat())
		hb, err := rps.HasLocalBranches(ctx, &gitalypb.HasLocalBranchesRequest{Repository: repo})
		require.NoError(t, err)
		assert.True(t, hb.GetValue())
	})

	t.Run("find refs by oid", func(t *testing.T) {
		head := headOf(t, src)
		refs, err := rs.FindRefsByOID(ctx, &gitalypb.FindRefsByOIDRequest{Repository: repo, Oid: head})
		require.NoError(t, err)
		assert.Contains(t, refs.GetRefs(), "refs/heads/main")
	})
}
