package gitaly

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// recvStream abstracts a generated client stream so tests can drain it.
type recvStream[T any] interface {
	Recv() (T, error)
}

// drain collects a server stream until EOF, failing the test on mid-stream
// errors (stricter than the loose break-on-any-error loops elsewhere).
func drain[T any](t *testing.T, s recvStream[T]) []T {
	t.Helper()
	var out []T
	for {
		resp, err := s.Recv()
		if err == io.EOF {
			return out
		}
		require.NoError(t, err)
		out = append(out, resp)
	}
}

// TestGitalyEnumerationSurfaces covers the implemented RPCs that lacked any
// direct test invocation (coverage audit 2026-10): server disk stats, object
// directory size, ref/commit enumeration and the remaining diff surface.
func TestGitalyEnumerationSurfaces(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()

	repoName := "proj-gaps"
	head, _ := seedHistory(t, svc, repoName,
		map[string]string{"hello.txt": "hello gitaly\n", "docs/guide.md": "guide"},
		map[string]string{"docs/guide.md": "guide v2", "src/main.go": "package main"})
	c1 := c1Of(t, svc, repoName, head)
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}

	rs := gitalypb.NewRefServiceClient(conn)
	cs := gitalypb.NewCommitServiceClient(conn)
	ds := gitalypb.NewDiffServiceClient(conn)

	guideV1 := model.ObjectOID(model.BlobType, []byte("guide"), model.SHA1).String()
	guideV2 := model.ObjectOID(model.BlobType, []byte("guide v2"), model.SHA1).String()

	// Fixture refs: a topic branch at the root commit, a lightweight tag at
	// head and an annotated tag — the three shapes enumeration must tell apart.
	_, err = gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
		Repository: repo, Ref: []byte("refs/heads/topic"), Revision: []byte(c1)})
	require.NoError(t, err)
	upd, err := rs.UpdateReferences(ctx)
	require.NoError(t, err)
	require.NoError(t, upd.Send(&gitalypb.UpdateReferencesRequest{
		Repository: repo,
		Updates: []*gitalypb.UpdateReferencesRequest_Update{{
			Reference: []byte("refs/tags/lw"), NewObjectId: []byte(head.String())}}}))
	_, err = upd.CloseAndRecv()
	require.NoError(t, err)
	_, err = gitalypb.NewOperationServiceClient(conn).UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
		Repository: repo, TagName: []byte("v1.0"), TargetRevision: []byte("main"),
		Message: []byte("release\n"), User: &gitalypb.User{
			GlId: "user-1", Name: []byte("Dev Eloper"), Email: []byte("dev@example.com")},
		Timestamp: timestamppb.New(time.Unix(1700000000, 0))})
	require.NoError(t, err)

	t.Run("disk statistics", func(t *testing.T) {
		out, err := gitalypb.NewServerServiceClient(conn).DiskStatistics(ctx, &gitalypb.DiskStatisticsRequest{})
		require.NoError(t, err)
		require.Len(t, out.GetStorageStatuses(), 1)
		assert.Equal(t, "default", out.GetStorageStatuses()[0].GetStorageName())
	})

	t.Run("object directory size mirrors repository size", func(t *testing.T) {
		rsClient := gitalypb.NewRepositoryServiceClient(conn)
		objDir, err := rsClient.GetObjectDirectorySize(ctx, &gitalypb.GetObjectDirectorySizeRequest{Repository: repo})
		require.NoError(t, err)
		total, err := rsClient.RepositorySize(ctx, &gitalypb.RepositorySizeRequest{Repository: repo})
		require.NoError(t, err)
		assert.Greater(t, objDir.GetSize(), int64(0))
		assert.Equal(t, total.GetSize(), objDir.GetSize(), "one object plane, not split storage")
	})

	t.Run("find all branches lists heads", func(t *testing.T) {
		stream, err := rs.FindAllBranches(ctx, &gitalypb.FindAllBranchesRequest{Repository: repo})
		require.NoError(t, err)
		branches := drain(t, stream)
		tips := map[string]string{}
		for _, resp := range branches {
			for _, b := range resp.GetBranches() {
				tips[string(b.GetName())] = b.GetTarget().GetId()
			}
		}
		assert.Equal(t, map[string]string{
			"refs/heads/main":  head.String(),
			"refs/heads/topic": c1,
		}, tips)
	})

	t.Run("find all tags distinguishes shapes", func(t *testing.T) {
		stream, err := rs.FindAllTags(ctx, &gitalypb.FindAllTagsRequest{Repository: repo})
		require.NoError(t, err)
		tags := map[string]*gitalypb.Tag{}
		for _, resp := range drain(t, stream) {
			for _, tag := range resp.GetTags() {
				tags[string(tag.GetName())] = tag
			}
		}
		require.Len(t, tags, 2)
		assert.Equal(t, head.String(), tags["lw"].GetId(), "lightweight tag id is the commit")
		assert.NotEqual(t, head.String(), tags["v1.0"].GetId(), "annotated tag id is the tag object")
		for _, tag := range tags {
			assert.Equal(t, head.String(), tag.GetTargetCommit().GetId())
		}
	})

	t.Run("find all remote branches is empty", func(t *testing.T) {
		stream, err := rs.FindAllRemoteBranches(ctx, &gitalypb.FindAllRemoteBranchesRequest{Repository: repo})
		require.NoError(t, err)
		assert.Empty(t, drain(t, stream), "no remote-tracking refs: the honest answer")
	})

	t.Run("branch names containing commit", func(t *testing.T) {
		stream, err := rs.ListBranchNamesContainingCommit(ctx, &gitalypb.ListBranchNamesContainingCommitRequest{
			Repository: repo, CommitId: c1})
		require.NoError(t, err)
		names := map[string]bool{}
		for _, resp := range drain(t, stream) {
			for _, n := range resp.GetBranchNames() {
				names[string(n)] = true
			}
		}
		assert.Equal(t, map[string]bool{"main": true, "topic": true}, names)

		stream, err = rs.ListBranchNamesContainingCommit(ctx, &gitalypb.ListBranchNamesContainingCommitRequest{
			Repository: repo, CommitId: head.String()})
		require.NoError(t, err)
		names = map[string]bool{}
		for _, resp := range drain(t, stream) {
			for _, n := range resp.GetBranchNames() {
				names[string(n)] = true
			}
		}
		assert.Equal(t, map[string]bool{"main": true}, names, "topic stopped at the root commit")
	})

	t.Run("tag names containing commit", func(t *testing.T) {
		stream, err := rs.ListTagNamesContainingCommit(ctx, &gitalypb.ListTagNamesContainingCommitRequest{
			Repository: repo, CommitId: c1})
		require.NoError(t, err)
		names := map[string]bool{}
		for _, resp := range drain(t, stream) {
			for _, n := range resp.GetTagNames() {
				names[string(n)] = true
			}
		}
		assert.Equal(t, map[string]bool{"lw": true, "v1.0": true}, names,
			"both tags peel to head, which contains the root commit")
	})

	t.Run("list all commits with pagination", func(t *testing.T) {
		stream, err := cs.ListAllCommits(ctx, &gitalypb.ListAllCommitsRequest{Repository: repo})
		require.NoError(t, err)
		var ids []string
		for _, resp := range drain(t, stream) {
			for _, c := range resp.GetCommits() {
				ids = append(ids, c.GetId())
			}
		}
		assert.Equal(t, []string{head.String(), c1}, ids, "branch heads union, tip first")

		stream, err = cs.ListAllCommits(ctx, &gitalypb.ListAllCommitsRequest{
			Repository: repo, PaginationParams: &gitalypb.PaginationParameter{Limit: 1}})
		require.NoError(t, err)
		var paged []string
		for _, resp := range drain(t, stream) {
			for _, c := range resp.GetCommits() {
				paged = append(paged, c.GetId())
			}
		}
		assert.Equal(t, []string{head.String()}, paged)
	})

	t.Run("find all branches merged_only filters divergent", func(t *testing.T) {
		// A commit off the mainline whose tip no merged branch contains: the
		// branch must exist (ref at c1) before committing onto its timeline.
		_, err := gitalypb.NewRepositoryServiceClient(conn).WriteRef(ctx, &gitalypb.WriteRefRequest{
			Repository: repo, Ref: []byte("refs/heads/divergent"), Revision: []byte(c1)})
		require.NoError(t, err)
		alpha := "off mainline"
		require.NoError(t, svc.PutObjects(ctx, repoName, []service.PutObject{{
			OID: model.ObjectOID(model.BlobType, []byte(alpha), model.SHA1), Type: "blob", Data: []byte(alpha)}}))
		divergent, err := svc.CreateCommit(ctx, service.CommitRequest{
			Repo: repoName, Branch: "divergent", Base: model.OID(c1), Message: "divergent",
			Changes: []service.Change{{Path: "alpha.txt",
				Blob: model.ObjectOID(model.BlobType, []byte(alpha), model.SHA1)}}})
		require.NoError(t, err)
		require.NotEqual(t, head.String(), divergent.Head.String())

		stream, err := rs.FindAllBranches(ctx, &gitalypb.FindAllBranchesRequest{
			Repository: repo, MergedOnly: true, MergedBranches: [][]byte{[]byte("main")}})
		require.NoError(t, err)
		names := map[string]bool{}
		for _, resp := range drain(t, stream) {
			for _, b := range resp.GetBranches() {
				names[string(b.GetName())] = true
			}
		}
		assert.Equal(t, map[string]bool{"refs/heads/main": true, "refs/heads/topic": true}, names,
			"topic is merged (its tip is an ancestor of main); divergent is not")
	})

	t.Run("list files", func(t *testing.T) {
		stream, err := cs.ListFiles(ctx, &gitalypb.ListFilesRequest{Repository: repo, Revision: []byte("main")})
		require.NoError(t, err)
		files := map[string]bool{}
		for _, resp := range drain(t, stream) {
			for _, p := range resp.GetPaths() {
				files[string(p)] = true
			}
		}
		assert.Equal(t, map[string]bool{
			"hello.txt": true, "docs/guide.md": true, "src/main.go": true}, files)
	})

	t.Run("list commits by oid skips absent", func(t *testing.T) {
		stream, err := cs.ListCommitsByOid(ctx, &gitalypb.ListCommitsByOidRequest{
			Repository: repo, Oid: []string{head.String(), "deadbeef", c1}})
		require.NoError(t, err)
		var ids []string
		for _, resp := range drain(t, stream) {
			for _, c := range resp.GetCommits() {
				ids = append(ids, c.GetId())
			}
		}
		assert.Equal(t, []string{head.String(), c1}, ids)
	})

	t.Run("get commit messages skips absent", func(t *testing.T) {
		stream, err := cs.GetCommitMessages(ctx, &gitalypb.GetCommitMessagesRequest{
			Repository: repo, CommitIds: []string{head.String(), c1, "deadbeef"}})
		require.NoError(t, err)
		msgs := map[string]string{}
		for _, resp := range drain(t, stream) {
			msgs[resp.GetCommitId()] = string(resp.GetMessage())
		}
		require.Len(t, msgs, 2)
		assert.Equal(t, "second\n", msgs[head.String()])
		assert.Equal(t, "first\n", msgs[c1])
	})

	t.Run("check objects exist over the bidi stream", func(t *testing.T) {
		stream, err := cs.CheckObjectsExist(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.CheckObjectsExistRequest{
			Repository: repo,
			Revisions:  [][]byte{[]byte("main"), []byte("refs/heads/ghost"), []byte("deadbeef")}}))
		require.NoError(t, stream.CloseSend())
		var got []*gitalypb.CheckObjectsExistResponse_RevisionExistence
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			got = append(got, resp.GetRevisions()...)
		}
		require.Len(t, got, 3)
		assert.Equal(t, "main", string(got[0].GetName()))
		assert.True(t, got[0].GetExists())
		assert.False(t, got[1].GetExists())
		assert.False(t, got[2].GetExists())
	})

	t.Run("list last commits for tree", func(t *testing.T) {
		stream, err := cs.ListLastCommitsForTree(ctx, &gitalypb.ListLastCommitsForTreeRequest{
			Repository: repo, Revision: "main"})
		require.NoError(t, err)
		last := map[string]string{}
		for _, resp := range drain(t, stream) {
			for _, c := range resp.GetCommits() {
				last[string(c.GetPathBytes())] = c.GetCommit().GetId()
			}
		}
		assert.Equal(t, map[string]string{
			"hello.txt": c1,
			"docs":      head.String(),
			"src":       head.String(),
		}, last, "hello.txt last moved in the root commit; docs and src in head")

		stream, err = cs.ListLastCommitsForTree(ctx, &gitalypb.ListLastCommitsForTreeRequest{
			Repository: repo, Revision: "main", Limit: 1})
		require.NoError(t, err)
		one := drain(t, stream)
		require.Len(t, one, 1)
		require.Len(t, one[0].GetCommits(), 1)
		assert.Equal(t, "docs", string(one[0].GetCommits()[0].GetPathBytes()))
	})

	t.Run("commit diff carries blob ids and line stats", func(t *testing.T) {
		stream, err := ds.CommitDiff(ctx, &gitalypb.CommitDiffRequest{
			Repository: repo, LeftCommitId: c1, RightCommitId: head.String()})
		require.NoError(t, err)
		byPath := map[string]*gitalypb.CommitDiffResponse{}
		for _, resp := range drain(t, stream) {
			byPath[string(resp.GetToPath())] = resp
		}
		require.Len(t, byPath, 2)

		guide := byPath["docs/guide.md"]
		assert.Equal(t, guideV1, guide.GetFromId())
		assert.Equal(t, guideV2, guide.GetToId())
		assert.Equal(t, int32(1), guide.GetLinesAdded())
		assert.Equal(t, int32(1), guide.GetLinesRemoved())
		assert.False(t, guide.GetBinary())
		assert.True(t, guide.GetEndOfPatch())

		mainGo := byPath["src/main.go"]
		assert.Equal(t, int32(1), mainGo.GetLinesAdded())
		assert.Equal(t, int32(0), mainGo.GetLinesRemoved())
		assert.NotEmpty(t, mainGo.GetToId())
	})

	t.Run("raw patch mirrors raw diff", func(t *testing.T) {
		rawDiff, err := ds.RawDiff(ctx, &gitalypb.RawDiffRequest{
			Repository: repo, LeftCommitId: c1, RightCommitId: head.String()})
		require.NoError(t, err)
		var diffBytes []byte
		for _, resp := range drain(t, rawDiff) {
			diffBytes = append(diffBytes, resp.GetData()...)
		}

		rawPatch, err := ds.RawPatch(ctx, &gitalypb.RawPatchRequest{
			Repository: repo, LeftCommitId: c1, RightCommitId: head.String()})
		require.NoError(t, err)
		var patchBytes []byte
		for _, resp := range drain(t, rawPatch) {
			patchBytes = append(patchBytes, resp.GetData()...)
		}

		require.NotEmpty(t, diffBytes)
		assert.Equal(t, string(diffBytes), string(patchBytes),
			"the format-packet headers are a caller concern; bodies are identical")
	})

	t.Run("diff blobs pairs directly", func(t *testing.T) {
		stream, err := ds.DiffBlobs(ctx, &gitalypb.DiffBlobsRequest{
			Repository: repo,
			BlobPairs: []*gitalypb.DiffBlobsRequest_BlobPair{
				{LeftBlob: []byte("hello\n"), RightBlob: []byte("hella\n")},
				{LeftBlob: []byte{0x00, 0x01}, RightBlob: []byte{0x00, 0x02}},
			}})
		require.NoError(t, err)
		resps := drain(t, stream)
		require.Len(t, resps, 2)

		text := resps[0]
		assert.False(t, text.GetBinary())
		assert.Equal(t, int32(1), text.GetLinesAdded())
		assert.Equal(t, int32(1), text.GetLinesRemoved())
		assert.Contains(t, string(text.GetPatch()), "@@")

		assert.True(t, resps[1].GetBinary(), "NUL bytes mark the pair binary")
		assert.Empty(t, resps[1].GetPatch())
	})
}
