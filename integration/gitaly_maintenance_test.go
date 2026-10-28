// T67 — RepositoryService maintenance slice (docs/19 slice 8). Git is the
// oracle: GetArchive answers byte-identical tar/tar.gz streams against
// `git archive`, checksums recompute from `git show-ref --head`, raw
// changes match `git diff --raw`, and a removed repository stops existing.
package integration

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"math/big"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// gitDated commits with pinned author/committer timestamps so archive mtimes
// are deterministic on both sides of the oracle.
func gitDated(t *testing.T, dir string, unix int64, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "user.name=it", "-c", "user.email=it@banyan.dev",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	stamp := strconv.FormatInt(unix, 10) + " +0000"
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_DATE="+stamp)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	require.NoError(t, cmd.Run(), "git %v: %s", args, buf.String())
	return buf.String()
}

func TestT67GitalyMaintenanceSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t67"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t67")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	rs := gitalypb.NewRepositoryServiceClient(conn)

	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/hello.txt", "hello archive\n")
	write(t, src+"/docs/guide.md", "guide\n")
	write(t, src+"/run.sh", "#!/bin/sh\necho hi\n")
	gitDated(t, src, 1700000001, "add", "-A")
	gitDated(t, src, 1700000001, "update-index", "--chmod=+x", "run.sh")
	gitDated(t, src, 1700000001, "commit", "-qm", "c1 seed")
	write(t, src+"/docs/guide.md", "guide v2\n")
	write(t, src+"/src/main.go", "package main\n")
	gitDated(t, src, 1700000011, "add", "-A")
	gitDated(t, src, 1700000011, "commit", "-qm", "c2 evolve")
	git(t, src, "push", "-q", s.repoURL("t67"), "main")

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "t67"}
	fetchArchive := func(req *gitalypb.GetArchiveRequest) []byte {
		stream, err := rs.GetArchive(ctx, req)
		require.NoError(t, err)
		var b []byte
		for {
			resp, rerr := stream.Recv()
			if rerr == io.EOF {
				return b
			}
			require.NoError(t, rerr)
			b = append(b, resp.GetData()...)
		}
	}
	oracle := func(args ...string) []byte {
		out, err := exec.Command("git", append([]string{
			"-C", src, "-c", "core.autocrlf=false"}, args...)...).Output()
		require.NoError(t, err)
		return out
	}

	t.Run("tar is byte-identical to git archive", func(t *testing.T) {
		got := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Prefix: "proj-1",
			Format: gitalypb.GetArchiveRequest_TAR})
		want := oracle("archive", "--format=tar", "--prefix=proj-1/", "HEAD")
		assert.True(t, bytes.Equal(want, got),
			"tar streams differ: git %d bytes, banyan %d bytes\nfirst diff at %d",
			len(want), len(got), firstDiff(want, got))
	})

	t.Run("tar.gz decompresses to the git archive payload", func(t *testing.T) {
		got := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Prefix: "proj-1",
			Format: gitalypb.GetArchiveRequest_TAR_GZ})
		want := oracle("archive", "--format=tar", "--prefix=proj-1/", "HEAD")
		// Deflate streams differ between zlib and Go; the gzip container
		// must be header-clean (gzip -n: no name, no mtime) and the
		// payload identical.
		zr, err := gzip.NewReader(bytes.NewReader(got))
		require.NoError(t, err)
		assert.Empty(t, zr.Header.Name, "gzip -n carries no name")
		assert.True(t, zr.Header.ModTime.IsZero(), "gzip -n carries no mtime")
		expanded, err := io.ReadAll(zr)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(want, expanded),
			"tar.gz payload differs from git archive: git %d bytes, banyan %d bytes\nfirst diff at %d",
			len(want), len(expanded), firstDiff(want, expanded))
	})

	t.Run("zip entries match git archive", func(t *testing.T) {
		got := fetchArchive(&gitalypb.GetArchiveRequest{
			Repository: repo, CommitId: "main", Prefix: "proj-1",
			Format: gitalypb.GetArchiveRequest_ZIP})
		want := oracle("archive", "--format=zip", "--prefix=proj-1/", "HEAD")
		gz, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
		require.NoError(t, err)
		wz, err := zip.NewReader(bytes.NewReader(want), int64(len(want)))
		require.NoError(t, err)
		require.Equal(t, len(wz.File), len(gz.File), "entry counts")
		for i := range wz.File {
			wf, gf := wz.File[i], gz.File[i]
			assert.Equal(t, wf.Name, gf.Name, "entry %d name", i)
			assert.Equal(t, wf.UncompressedSize64, gf.UncompressedSize64, "entry %s size", wf.Name)
			assert.Equal(t, wf.Modified.Unix(), gf.Modified.Unix(), "entry %s mtime", wf.Name)
			wd := readZipBody(t, wf)
			gd := readZipBody(t, gf)
			assert.Equal(t, string(wd), string(gd), "entry %s body", wf.Name)
		}
	})

	t.Run("checksum recomputes from git show-ref", func(t *testing.T) {
		out, err := rs.CalculateChecksum(ctx, &gitalypb.CalculateChecksumRequest{Repository: repo})
		require.NoError(t, err)

		var sum big.Int
		for _, line := range strings.Split(string(oracle("show-ref", "--head")), "\n") {
			if line == "" {
				continue
			}
			h := sha1.Sum([]byte(line))
			var one big.Int
			one.SetBytes(h[:])
			if sum.BitLen() == 0 {
				sum.Set(&one)
			} else {
				sum.Xor(&sum, &one)
			}
		}
		assert.Equal(t, hex.EncodeToString(sum.Bytes()), out.GetChecksum())
	})

	t.Run("fsck is clean", func(t *testing.T) {
		out, err := rs.Fsck(ctx, &gitalypb.FsckRequest{Repository: repo})
		require.NoError(t, err)
		assert.Empty(t, string(out.GetError()))
	})

	t.Run("raw changes match git diff --raw", func(t *testing.T) {
		stream, err := rs.GetRawChanges(ctx, &gitalypb.GetRawChangesRequest{
			Repository: repo, FromRevision: "HEAD~1", ToRevision: "HEAD"})
		require.NoError(t, err)
		changes := map[string]*gitalypb.GetRawChangesResponse_RawChange{}
		for {
			resp, rerr := stream.Recv()
			if rerr == io.EOF {
				break
			}
			require.NoError(t, rerr)
			for _, c := range resp.GetRawChanges() {
				changes[string(c.GetNewPathBytes())] = c
			}
		}
		require.Len(t, changes, 2)
		guide := changes["docs/guide.md"]
		require.NotNil(t, guide)
		assert.Equal(t, int32(0o100644), guide.GetOldMode())
		wantOID := strings.TrimSpace(git(t, src, "rev-parse", "HEAD:docs/guide.md"))
		assert.Equal(t, wantOID, guide.GetBlobId())
		assert.Equal(t, int64(len("guide v2\n")), guide.GetSize())
		require.NotNil(t, changes["src/main.go"])
	})

	t.Run("remove repository retires the clone", func(t *testing.T) {
		git(t, src, "push", "-q", s.repoURL("t67-doomed"),
			"main:refs/heads/main")
		doomed := &gitalypb.Repository{StorageName: "default", RelativePath: "t67-doomed"}
		_, err := rs.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{Repository: doomed})
		require.NoError(t, err)
		ex, err := rs.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: doomed})
		require.NoError(t, err)
		assert.False(t, ex.GetExists())
		_, err = gitTry(src, "ls-remote", s.repoURL("t67-doomed"))
		assert.Error(t, err, "the retired repository must stop answering")
		_, err = rs.RemoveRepository(ctx, &gitalypb.RemoveRepositoryRequest{Repository: doomed})
		require.NoError(t, err, "removal is idempotent")
	})
}

func readZipBody(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	require.NoError(t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return b
}

// firstDiff reports the offset of the first byte difference (len of the
// shorter prefix when one stream is a prefix of the other).
func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
