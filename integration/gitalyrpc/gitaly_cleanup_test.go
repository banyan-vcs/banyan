// T71 — history rewrite (docs/19). RewriteHistory strips blobs and redacts
// literals across every reachable commit, then re-points all refs onto the
// rewritten chain; ApplyBfgObjectMapStream applies an old→new map by
// dropping internal refs anchored at rewritten commits. The oracle is git
// itself: a fresh clone after the rewrite carries the rewritten history
// only, with the stripped path absent and the secret replaced.
package gitalyrpc

import (
	"context"
	"io"
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

func TestT71GitalyHistoryRewrite(t *testing.T) {
	support.NeedGit(t)
	s, meta, objects := support.NewServerPlane(t)
	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t71"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t71")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	rs := gitalypb.NewRepositoryServiceClient(conn)
	fs := gitalypb.NewRefServiceClient(conn)
	cl := gitalypb.NewCleanupServiceClient(conn)

	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, src+"/app.py", "key = \"AKIA-SECRET-123\"\nprint(key)\n")
	support.Write(t, src+"/big.bin", "oversized generated artifact\n")
	support.Git(t, src, "add", "-A")
	support.GitDated(t, src, 1700000000, "commit", "-qm", "c1 import")
	support.Write(t, src+"/app.py", "key = \"AKIA-SECRET-123-v2\"\n")
	support.Git(t, src, "add", "-A")
	support.GitDated(t, src, 1700000100, "commit", "-qm", "c2 rotate")
	support.Git(t, src, "push", "-q", s.RepoURL("t71"), "main")
	bigOID := strings.Fields(support.Git(t, src, "rev-parse", "HEAD~1:big.bin"))[0]

	stream, err := cl.RewriteHistory(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.RewriteHistoryRequest{
		Repository:  &gitalypb.Repository{StorageName: "default", RelativePath: "t71"},
		Blobs:       []string{bigOID},
		Redactions:  [][]byte{[]byte("literal:AKIA-SECRET-123-v2"), []byte("regex:AKIA-SECRET-[0-9]+")},
	}))
	_, err = stream.CloseAndRecv()
	require.NoError(t, err)

	work := t.TempDir()
	support.Git(t, work, "clone", "-q", s.RepoURL("t71"), "clean")
	repo := work + "/clean"

	// The advertised history is the rewritten chain only — disjoint, so the
	// pre-rewrite commits never reach the clone.
	count := strings.TrimSpace(support.Git(t, repo, "rev-list", "--all", "--count"))
	assert.Equal(t, "2", count, "clone must carry only the rewritten history")

	// The stripped blob is gone at every revision.
	for _, out := range strings.Split(support.Git(t, repo, "log", "--all", "--name-only", "--format="), "\n") {
		assert.NotEqual(t, "big.bin", strings.TrimSpace(out), "stripped path must not survive")
	}

	// The secret is gone from every blob in history; the redaction marker is
	// what clients see instead.
	revs := strings.Split(support.Git(t, repo, "rev-list", "--all"), "\n")
	for _, rev := range revs {
		if strings.TrimSpace(rev) == "" {
			continue
		}
		blob := support.Git(t, repo, "show", rev+":app.py")
		assert.NotContains(t, blob, "AKIA-SECRET", "rev %s", rev)
		assert.Contains(t, blob, "***REMOVED***", "rev %s", rev)
	}

	// --- ApplyBfgObjectMapStream: internal refs drop, entries stream ---
	head := strings.Fields(support.Git(t, repo, "rev-parse", "HEAD"))[0]
	_, err = rs.WriteRef(ctx, &gitalypb.WriteRefRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "t71"},
		Ref:        []byte("refs/keep-around/1"),
		Revision:   []byte(head),
	})
	require.NoError(t, err)

	bfgStream, err := cl.ApplyBfgObjectMapStream(ctx)
	require.NoError(t, err)
	require.NoError(t, bfgStream.Send(&gitalypb.ApplyBfgObjectMapStreamRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "t71"},
		ObjectMap: []byte("old                                      new\n" +
			head + " " + strings.Repeat("0", 40) + "\n"),
	}))
	require.NoError(t, bfgStream.CloseSend())
	var entries []*gitalypb.ApplyBfgObjectMapStreamResponse_Entry
	for {
		resp, rerr := bfgStream.Recv()
		if rerr == io.EOF {
			break
		}
		require.NoError(t, rerr)
		entries = append(entries, resp.GetEntries()...)
	}
	require.Len(t, entries, 1)
	assert.Equal(t, head, entries[0].GetOldOid())

	exists, err := fs.RefExists(ctx, &gitalypb.RefExistsRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "t71"},
		Ref:        []byte("refs/keep-around/1"),
	})
	require.NoError(t, err)
	assert.False(t, exists.GetValue(), "internal ref anchored at a rewritten OID is removed")
	exists, err = fs.RefExists(ctx, &gitalypb.RefExistsRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "t71"},
		Ref:        []byte("refs/heads/main"),
	})
	require.NoError(t, err)
	assert.True(t, exists.GetValue(), "branch refs survive the map application")
}
