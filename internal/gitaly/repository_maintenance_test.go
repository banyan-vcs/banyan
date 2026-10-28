package gitaly

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	compressgzip "compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

func TestGitalyMaintenanceSurface(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()

	repoName := "proj-maint"
	head, _ := seedHistory(t, svc, repoName,
		map[string]string{"hello.txt": "hello gitaly\n", "docs/guide.md": "guide", ".gitattributes": "*.md text\n"},
		map[string]string{"docs/guide.md": "guide v2", "src/main.go": "package main"})
	c1 := c1Of(t, svc, repoName, head)
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}
	rs := gitalypb.NewRepositoryServiceClient(conn)

	// A lightweight tag broadens the checksum's reference set.
	upd, err := gitalypb.NewRefServiceClient(conn).UpdateReferences(ctx)
	require.NoError(t, err)
	require.NoError(t, upd.Send(&gitalypb.UpdateReferencesRequest{
		Repository: repo,
		Updates: []*gitalypb.UpdateReferencesRequest_Update{{
			Reference: []byte("refs/tags/lw"), NewObjectId: []byte(head.String())}}}))
	_, err = upd.CloseAndRecv()
	require.NoError(t, err)

	t.Run("calculate checksum is the xor of per-ref sha1s", func(t *testing.T) {
		out, err := rs.CalculateChecksum(ctx, &gitalypb.CalculateChecksumRequest{Repository: repo})
		require.NoError(t, err)

		// Recompute by hand from the advertised refs (HEAD, main, lw tag).
		refs := map[string]string{"HEAD": head.String(), "refs/heads/main": head.String(), "refs/tags/lw": head.String()}
		var acc []byte
		first := true
		for name, oid := range refs {
			h := sha1.Sum([]byte(oid + " " + name))
			if first {
				acc = h[:]
				first = false
				continue
			}
			for i := range acc {
				acc[i] ^= h[i]
			}
		}
		assert.Equal(t, hex.EncodeToString(acc), out.GetChecksum())
		assert.NotEqual(t, strings.Repeat("0", 40), out.GetChecksum())
	})

	t.Run("calculate checksum of an empty repository", func(t *testing.T) {
		empty := &gitalypb.Repository{StorageName: "default", RelativePath: "empty-ck"}
		_, err := rs.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: empty})
		require.NoError(t, err)
		out, err := rs.CalculateChecksum(ctx, &gitalypb.CalculateChecksumRequest{Repository: empty})
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("0", 40), out.GetChecksum())
	})

	t.Run("fsck of a healthy repository is clean", func(t *testing.T) {
		out, err := rs.Fsck(ctx, &gitalypb.FsckRequest{Repository: repo})
		require.NoError(t, err)
		assert.Empty(t, string(out.GetError()))
	})

	t.Run("repository info reports the single object plane", func(t *testing.T) {
		out, err := rs.RepositoryInfo(ctx, &gitalypb.RepositoryInfoRequest{Repository: repo})
		require.NoError(t, err)
		assert.Greater(t, out.GetSize(), uint64(0))
		require.NotNil(t, out.GetReferences())
		assert.GreaterOrEqual(t, out.GetReferences().GetLooseCount(), uint64(2))
		assert.Equal(t, gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES, out.GetReferences().GetReferenceBackend())
		require.NotNil(t, out.GetObjects())
		assert.GreaterOrEqual(t, out.GetObjects().GetPackfileCount(), uint64(1))
	})

	t.Run("housekeeping no-ops", func(t *testing.T) {
		_, err := rs.OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{Repository: repo})
		assert.NoError(t, err)
		_, err = rs.OptimizeRepository(ctx, &gitalypb.OptimizeRepositoryRequest{
			Repository: repo, Strategy: gitalypb.OptimizeRepositoryRequest_STRATEGY_OFFLOADING})
		assert.Equal(t, codes.Unimplemented, status.Code(err))
		_, err = rs.PruneUnreachableObjects(ctx, &gitalypb.PruneUnreachableObjectsRequest{Repository: repo})
		assert.NoError(t, err)
	})

	t.Run("get config is an honest empty stream", func(t *testing.T) {
		stream, err := rs.GetConfig(ctx, &gitalypb.GetConfigRequest{Repository: repo})
		require.NoError(t, err)
		assert.Empty(t, drain(t, stream))
	})

	t.Run("info attributes serves the tip .gitattributes", func(t *testing.T) {
		stream, err := rs.GetInfoAttributes(ctx, &gitalypb.GetInfoAttributesRequest{Repository: repo})
		require.NoError(t, err)
		var b []byte
		for _, resp := range drain(t, stream) {
			b = append(b, resp.GetAttributes()...)
		}
		assert.Equal(t, "*.md text\n", string(b))
	})

	t.Run("objects size over the bidi stream", func(t *testing.T) {
		stream, err := rs.ObjectsSize(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.ObjectsSizeRequest{
			Repository: repo, Revisions: [][]byte{[]byte("main")}}))
		out, err := stream.CloseAndRecv()
		require.NoError(t, err)
		assert.Greater(t, out.GetSize(), uint64(0))

		// A second repository field on a follow-up request is rejected.
		stream, err = rs.ObjectsSize(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.ObjectsSizeRequest{
			Repository: repo, Revisions: [][]byte{[]byte("main")}}))
		require.NoError(t, stream.Send(&gitalypb.ObjectsSizeRequest{
			Repository: repo, Revisions: [][]byte{[]byte(c1)}}))
		_, err = stream.CloseAndRecv()
		assert.Equal(t, codes.InvalidArgument, status.Code(err))

		stream, err = rs.ObjectsSize(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.ObjectsSizeRequest{
			Repository: repo, Revisions: [][]byte{[]byte("no-such-branch")}}))
		_, err = stream.CloseAndRecv()
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("raw changes across the first parent", func(t *testing.T) {
		stream, err := rs.GetRawChanges(ctx, &gitalypb.GetRawChangesRequest{
			Repository: repo, FromRevision: c1, ToRevision: head.String()})
		require.NoError(t, err)
		var changes []*gitalypb.GetRawChangesResponse_RawChange
		for _, resp := range drain(t, stream) {
			changes = append(changes, resp.GetRawChanges()...)
		}
		require.Len(t, changes, 2)
		byPath := map[string]*gitalypb.GetRawChangesResponse_RawChange{}
		for _, c := range changes {
			byPath[string(c.GetNewPathBytes())] = c
		}
		guide := byPath["docs/guide.md"]
		require.NotNil(t, guide)
		assert.Equal(t, gitalypb.GetRawChangesResponse_RawChange_MODIFIED, guide.GetOperation())
		assert.Equal(t, int32(0o100644), guide.GetOldMode())
		assert.Equal(t, int32(0o100644), guide.GetNewMode())
		assert.Equal(t, len("guide v2"), int(guide.GetSize()))

		src := byPath["src/main.go"]
		require.NotNil(t, src)
		assert.Equal(t, gitalypb.GetRawChangesResponse_RawChange_ADDED, src.GetOperation())
		assert.Zero(t, src.GetOldMode())

		// Root commit against the empty base: every file is an addition.
		stream, err = rs.GetRawChanges(ctx, &gitalypb.GetRawChangesRequest{
			Repository: repo, ToRevision: c1})
		require.NoError(t, err)
		var roots []*gitalypb.GetRawChangesResponse_RawChange
		for _, resp := range drain(t, stream) {
			roots = append(roots, resp.GetRawChanges()...)
		}
		require.Len(t, roots, 3)
		for _, c := range roots {
			assert.Equal(t, gitalypb.GetRawChangesResponse_RawChange_ADDED, c.GetOperation())
		}
	})

	t.Run("remove repository is total and idempotent", func(t *testing.T) {
		doomed := &gitalypb.Repository{StorageName: "default", RelativePath: "proj-doomed"}
		seedHistory(t, svc, "proj-doomed",
			map[string]string{"a.txt": "a"}, map[string]string{"a.txt": "b"})
		_, err := rs.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{Repository: doomed})
		require.NoError(t, err)
		ex, err := rs.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: doomed})
		require.NoError(t, err)
		assert.False(t, ex.GetExists())
		_, err = rs.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{Repository: doomed})
		require.NoError(t, err, "removing a missing repository succeeds")
	})
}

// tarNames lists member names of a tar stream.
func tarNames(raw []byte) []string {
	tr := tar.NewReader(bytes.NewReader(raw))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			return names
		}
		names = append(names, h.Name)
	}
}

func gzipReader(raw []byte) (*compressgzip.Reader, error) {
	return compressgzip.NewReader(bytes.NewReader(raw))
}

func zipReader(raw []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
}

func TestGitalyGetArchive(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()

	repoName := "proj-arch"
	head, _ := seedHistory(t, svc, repoName,
		map[string]string{"hello.txt": "hello gitaly\n", "docs/guide.md": "guide"},
		map[string]string{"docs/guide.md": "guide v2", "src/main.go": "package main"})
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}
	rs := gitalypb.NewRepositoryServiceClient(conn)

	fetchArchive := func(req *gitalypb.GetArchiveRequest) ([]byte, error) {
		stream, err := rs.GetArchive(ctx, req)
		if err != nil {
			return nil, err
		}
		var b []byte
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return b, nil
			}
			if err != nil {
				return nil, err
			}
			b = append(b, resp.GetData()...)
		}
	}

	t.Run("tar structure mirrors git archive", func(t *testing.T) {
		raw, err := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Prefix: "proj", Format: gitalypb.GetArchiveRequest_TAR})
		require.NoError(t, err)
		assert.Equal(t, 0, len(raw)%tarBlockSize, "records pad to 10240")

		// pax_global_header block: name, mode 0666, typeflag 'g', comment body.
		assert.Equal(t, "pax_global_header", strings.TrimRight(string(raw[0:20]), "\x00"))
		assert.Equal(t, "0000666\x00", string(raw[100:108]))
		assert.Equal(t, byte('g'), raw[156])
		body := string(raw[512 : 512+52])
		assert.True(t, strings.HasPrefix(body, "52 comment="), body)
		assert.True(t, strings.HasSuffix(body, head.String()+"\n"), body)

		names := tarNames(raw)
		assert.Equal(t, []string{"pax_global_header", "proj/", "proj/docs/", "proj/docs/guide.md",
			"proj/hello.txt", "proj/src/", "proj/src/main.go"}, names)
	})

	t.Run("tar gz wraps the identical tar payload", func(t *testing.T) {
		plain, err := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Format: gitalypb.GetArchiveRequest_TAR})
		require.NoError(t, err)
		gz, err := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Format: gitalypb.GetArchiveRequest_TAR_GZ})
		require.NoError(t, err)
		zr, err := gzipReader(gz)
		require.NoError(t, err)
		expanded, err := io.ReadAll(zr)
		require.NoError(t, err)
		assert.Equal(t, string(plain), string(expanded))
	})

	t.Run("zip carries directory and file entries", func(t *testing.T) {
		raw, err := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Prefix: "proj", Format: gitalypb.GetArchiveRequest_ZIP})
		require.NoError(t, err)
		zr, err := zipReader(raw)
		require.NoError(t, err)
		names := map[string]bool{}
		for _, f := range zr.File {
			names[f.Name] = true
		}
		assert.True(t, names["proj/"])
		assert.True(t, names["proj/hello.txt"])
		assert.True(t, names["proj/docs/guide.md"])
		assert.True(t, names["proj/src/main.go"])
	})

	t.Run("subtree archives keep or elide the path", func(t *testing.T) {
		raw, err := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Format: gitalypb.GetArchiveRequest_TAR,
			Path: []byte("docs")})
		require.NoError(t, err)
		assert.Contains(t, tarNames(raw), "docs/guide.md")

		raw, err = fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Format: gitalypb.GetArchiveRequest_TAR,
			Path: []byte("docs"), ElidePath: true})
		require.NoError(t, err)
		assert.Contains(t, tarNames(raw), "guide.md")
		assert.NotContains(t, tarNames(raw), "docs/guide.md")
	})

	t.Run("missing path answers the structured detail", func(t *testing.T) {
		_, err := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Format: gitalypb.GetArchiveRequest_TAR,
			Path: []byte("no/such/dir")})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		st := status.Convert(err)
		require.Len(t, st.Details(), 1)
	})

	t.Run("bz2 and lfs smudge refuse explicitly", func(t *testing.T) {
		_, err := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Format: gitalypb.GetArchiveRequest_TAR_BZ2})
		assert.Equal(t, codes.Unimplemented, status.Code(err))
		_, err = fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Format: gitalypb.GetArchiveRequest_TAR,
			IncludeLfsBlobs: true})
		assert.Equal(t, codes.Unimplemented, status.Code(err))
	})
}
