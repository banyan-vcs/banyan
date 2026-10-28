package gitaly

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

const mitLicense = `MIT License

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction.
`

func TestGitalySearchSurface(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()

	repoName := "proj-search"
	_, _ = seedHistory(t, svc, repoName,
		map[string]string{
			"hello.txt":       "hello gitaly\n",
			"docs/guide.md":   "alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\ntheta\niota\nkappa\n",
			"LICENSE":         mitLicense,
			".gitattributes":  "*.md text\n*.go -merge\nbuild/** binary\n",
			"src/main.go":     "package main\n",
		},
		map[string]string{"src/main.go": "package main\n\nfunc main() {}\n"})
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}
	rs := gitalypb.NewRepositoryServiceClient(conn)

	t.Run("search files by name with pathspec filter and paging", func(t *testing.T) {
		stream, err := rs.SearchFilesByName(ctx, &gitalypb.SearchFilesByNameRequest{
			Repository: repo, Ref: []byte("main"), Query: "*.md"})
		require.NoError(t, err)
		resp := drain(t, stream)
		require.Len(t, resp, 1)
		assert.Equal(t, [][]byte{[]byte("docs/guide.md")}, resp[0].GetFiles())

		stream, err = rs.SearchFilesByName(ctx, &gitalypb.SearchFilesByNameRequest{
			Repository: repo, Ref: []byte("main"), Query: "*", Filter: "^src/"})
		require.NoError(t, err)
		resp = drain(t, stream)
		assert.Equal(t, [][]byte{[]byte("src/main.go")}, resp[0].GetFiles())

		stream, err = rs.SearchFilesByName(ctx, &gitalypb.SearchFilesByNameRequest{
			Repository: repo, Ref: []byte("main"), Query: "*", Limit: 2, Offset: 1})
		require.NoError(t, err)
		resp = drain(t, stream)
		require.Len(t, resp, 1)
		require.Len(t, resp[0].GetFiles(), 2, "offset then limit")

		// Validation errors surface on Recv for server-streaming RPCs.
		stream, err = rs.SearchFilesByName(ctx, &gitalypb.SearchFilesByNameRequest{
			Repository: repo, Ref: []byte("main"), Query: "", Filter: "["})
		require.NoError(t, err)
		_, err = stream.Recv()
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "empty query rejected before filter compile")

		stream, err = rs.SearchFilesByName(ctx, &gitalypb.SearchFilesByNameRequest{
			Repository: repo, Ref: []byte("main"), Query: "x", Filter: "["})
		require.NoError(t, err)
		_, err = stream.Recv()
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "bad filter regex rejected")
	})

	t.Run("search by content streams git-grep blocks", func(t *testing.T) {
		stream, err := rs.SearchFilesByContent(ctx, &gitalypb.SearchFilesByContentRequest{
			Repository: repo, Ref: []byte("main"), Query: "Kappa"})
		require.NoError(t, err)
		type block struct {
			data []byte
			end  bool
		}
		var blocks []block
		var cur []byte
		for _, resp := range drain(t, stream) {
			cur = append(cur, resp.GetMatchData()...)
			if resp.GetEndOfMatch() {
				blocks = append(blocks, block{data: cur, end: true})
				cur = nil
			}
		}
		require.Len(t, blocks, 1)
		text := string(blocks[0].data)
		assert.True(t, strings.HasPrefix(text, "docs/guide.md\x008-theta"), text)
		assert.Contains(t, text, "docs/guide.md\x009-iota")
		assert.Contains(t, text, "docs/guide.md\x0010:kappa")
		assert.NotContains(t, text, "docs/guide.md\x001-", "context never crosses into match form")

		// No hit: empty stream.
		stream, err = rs.SearchFilesByContent(ctx, &gitalypb.SearchFilesByContentRequest{
			Repository: repo, Ref: []byte("main"), Query: "no-such-token"})
		require.NoError(t, err)
		assert.Empty(t, drain(t, stream))
	})

	t.Run("get file attributes resolves rules", func(t *testing.T) {
		out, err := rs.GetFileAttributes(ctx, &gitalypb.GetFileAttributesRequest{
			Repository: repo, Revision: []byte("main"),
			Paths:      []string{"docs/guide.md", "src/main.go", "build/out.bin", "readme"},
			Attributes: []string{"text", "merge", "binary", "unsetattr"},
		})
		require.NoError(t, err)
		values := map[string]map[string]string{}
		for _, info := range out.GetAttributeInfos() {
			if values[info.GetPath()] == nil {
				values[info.GetPath()] = map[string]string{}
			}
			values[info.GetPath()][info.GetAttribute()] = info.GetValue()
		}
		assert.Equal(t, "set", values["docs/guide.md"]["text"])
		assert.Equal(t, "unset", values["src/main.go"]["merge"])
		assert.Equal(t, "unspecified", values["src/main.go"]["text"])
		assert.Equal(t, "set", values["build/out.bin"]["binary"])
		assert.Equal(t, "unspecified", values["build/out.bin"]["text"], "build/** is path-scoped")
		assert.Equal(t, "unspecified", values["readme"]["unsetattr"])
	})

	t.Run("find license fingerprints MIT", func(t *testing.T) {
		out, err := rs.FindLicense(ctx, &gitalypb.FindLicenseRequest{Repository: repo})
		require.NoError(t, err)
		assert.Equal(t, "mit", out.GetLicenseShortName())
		assert.Equal(t, "LICENSE", out.GetLicensePath())
	})

	t.Run("find license without candidates answers empty", func(t *testing.T) {
		bare := &gitalypb.Repository{StorageName: "default", RelativePath: "proj-nolicense"}
		seedHistory(t, svc, "proj-nolicense",
			map[string]string{"a.txt": "a"}, map[string]string{"a.txt": "b"})
		out, err := rs.FindLicense(ctx, &gitalypb.FindLicenseRequest{Repository: bare})
		require.NoError(t, err)
		assert.Empty(t, out.GetLicenseShortName())
	})
}

func TestGitalyCustomHooksRoundTrip(t *testing.T) {
	_, _, _, svc, addr := startServer(t)
	ctx := context.Background()
	conn, err := Dial(addr, testToken)
	require.NoError(t, err)
	defer conn.Close()

	seedHistory(t, svc, "proj-hooks", map[string]string{"a.txt": "a"}, map[string]string{"a.txt": "b"})
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "proj-hooks"}
	rs := gitalypb.NewRepositoryServiceClient(conn)

	// No hooks yet: an empty stream is the honest answer.
	stream, err := rs.GetCustomHooks(ctx, &gitalypb.GetCustomHooksRequest{Repository: repo})
	require.NoError(t, err)
	assert.Empty(t, drain(t, stream))

	tarball := []byte("custom_hooks/\x00custom_hooks/pre-receive\x00some shell script\x00")
	setStream, err := rs.SetCustomHooks(ctx)
	require.NoError(t, err)
	require.NoError(t, setStream.Send(&gitalypb.SetCustomHooksRequest{Repository: repo}))
	require.NoError(t, setStream.Send(&gitalypb.SetCustomHooksRequest{Data: tarball[:10]}))
	require.NoError(t, setStream.Send(&gitalypb.SetCustomHooksRequest{Data: tarball[10:]}))
	_, err = setStream.CloseAndRecv()
	require.NoError(t, err)

	stream, err = rs.GetCustomHooks(ctx, &gitalypb.GetCustomHooksRequest{Repository: repo})
	require.NoError(t, err)
	var got []byte
	for _, resp := range drain(t, stream) {
		got = append(got, resp.GetData()...)
	}
	assert.Equal(t, string(tarball), string(got))
}
