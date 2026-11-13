package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

func TestListTreeEntries(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	commitRoot(t, s, "demo", map[string]string{
		"top.txt":  "top",
		"a/x.txt":  "x",
		"a/b/y.md": "yy",
	})

	root, err := s.ListTreeEntries(ctx, ResolveRequest{Repo: "demo"})
	require.NoError(t, err)
	require.Len(t, root, 2, "one directory, one file")
	assert.Equal(t, "a", root[0].Name)
	assert.Equal(t, "tree", root[0].Type)
	assert.Equal(t, "top.txt", root[1].Name)
	assert.Equal(t, "blob", root[1].Type)
	assert.Equal(t, 3, root[1].Size, "blob size in bytes")

	sub, err := s.ListTreeEntries(ctx, ResolveRequest{Repo: "demo", Path: "/a"})
	require.NoError(t, err)
	require.Len(t, sub, 2)
	assert.Equal(t, "b", sub[0].Name)
	assert.Equal(t, "tree", sub[0].Type)
	assert.Equal(t, "x.txt", sub[1].Name)
	assert.Equal(t, 1, sub[1].Size)

	deep, err := s.ListTreeEntries(ctx, ResolveRequest{Repo: "demo", Path: "/a/b"})
	require.NoError(t, err)
	require.Len(t, deep, 1)
	assert.Equal(t, "y.md", deep[0].Name)
	assert.Equal(t, 2, deep[0].Size)

	_, err = s.ListTreeEntries(ctx, ResolveRequest{Repo: "demo", Path: "/nope"})
	require.Error(t, err, "absent path")

	_, err = s.ListTreeEntries(ctx, ResolveRequest{Repo: "demo", Path: "/top.txt"})
	require.Error(t, err, "a blob is not a directory")
}

func TestListTreeEntriesRevAndLogRev(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	seedHead, _ := commitRoot(t, s, "demo", map[string]string{"readme": "hello"})

	// A fresh branch starts from an empty head; its first commit is that
	// branch's whole view (CreateCommit CAS: Base zero matches zero).
	putFiles(t, s, "demo", map[string]string{"f.txt": "f"})
	fres, err := s.CreateCommit(ctx, CommitRequest{
		Repo: "demo", Branch: "feature", Base: "",
		Message: "feature work",
		Changes: []Change{{Path: "f.txt", Blob: model.ObjectOID(model.BlobType, []byte("f"), model.SHA1)}},
	})
	require.NoError(t, err, "feature 分支首提交失败")

	// The structured tree follows rev: default view vs the feature view.
	defTree, err := s.ListTreeEntries(ctx, ResolveRequest{Repo: "demo"})
	require.NoError(t, err)
	require.Len(t, defTree, 1)
	assert.Equal(t, "readme", defTree[0].Name)

	featTree, err := s.ListTreeEntries(ctx, ResolveRequest{Repo: "demo", Rev: "feature"})
	require.NoError(t, err)
	require.Len(t, featTree, 1)
	assert.Equal(t, "f.txt", featTree[0].Name)

	// Log without from starts at rev's woven tip; "" keeps the default.
	defLog, usedFrom, err := s.Log(ctx, "demo", "", "", "", 20)
	require.NoError(t, err)
	require.NotEmpty(t, defLog)
	assert.Equal(t, seedHead, usedFrom, "default-branch tip")
	assert.Equal(t, string(seedHead), defLog[0].OID)

	featLog, featFrom, err := s.Log(ctx, "demo", "feature", "", "", 20)
	require.NoError(t, err)
	require.NotEmpty(t, featLog)
	assert.Equal(t, fres.Head, featFrom, "feature tip")
	assert.Equal(t, string(fres.Head), featLog[0].OID)
	assert.Equal(t, "feature work", featLog[0].Subject)

	// from still wins over rev (cursor paging from any tip).
	paged, _, err := s.Log(ctx, "demo", "feature", seedHead, "", 20)
	require.NoError(t, err)
	require.NotEmpty(t, paged)
	assert.Equal(t, string(seedHead), paged[0].OID, "from overrides rev")
}
