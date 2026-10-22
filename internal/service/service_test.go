package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// newService builds a full in-memory stack (meta + local shards + views).
func newService(t *testing.T) *Service {
	t.Helper()
	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	objects := storage.NewObjectService(meta, local, []storage.Node{{ID: "n1", Local: true}}, model.SHA1)
	return New(meta, objects, model.SHA1)
}

// commitRoot creates the repo, uploads the blobs and lands the first commit
// (path → content), returning the commit OID and its tree.
func commitRoot(t *testing.T, s *Service, repo string, files map[string]string) (model.OID, model.OID) {
	t.Helper()
	require.NoError(t, s.Meta.CreateRepo(context.Background(), repo, model.SHA1))
	putFiles(t, s, repo, files)
	res, err := s.CreateCommit(context.Background(), CommitRequest{
		Repo: repo, Base: "", Message: "seed",
		Changes: changesFor(files),
	})
	require.NoError(t, err, "seed commit 失败")
	return res.Head, res.Tree
}

func changesFor(files map[string]string) []Change {
	var cs []Change
	for path, content := range files {
		cs = append(cs, Change{Path: path, Blob: model.ObjectOID(model.BlobType, []byte(content), model.SHA1)})
	}
	return cs
}

// putFiles uploads the blobs referenced by a changes list.
func putFiles(t *testing.T, s *Service, repo string, files map[string]string) {
	t.Helper()
	objs := make([]PutObject, 0, len(files))
	for _, content := range files {
		raw := []byte(content)
		objs = append(objs, PutObject{OID: model.ObjectOID(model.BlobType, raw, model.SHA1), Type: "blob", Data: raw})
	}
	require.NoError(t, s.PutObjects(context.Background(), repo, objs))
}

// treeFiles walks a tree into path → blob content (test oracle).
func treeFiles(t *testing.T, s *Service, repo string, tree model.OID) map[string]string {
	t.Helper()
	out := map[string]string{}
	lookup := s.Objects.Lookup(repo)
	var walk func(oid model.OID, prefix string)
	walk = func(oid model.OID, prefix string) {
		tr, err := lookup.Tree(oid)
		require.NoError(t, err)
		for _, e := range tr.Entries {
			rel := e.Name
			if prefix != "" {
				rel = prefix + "/" + e.Name
			}
			if e.IsDir {
				walk(e.Target, rel)
				continue
			}
			b, err := lookup.Blob(e.Target)
			require.NoError(t, err)
			out[rel] = string(b)
		}
	}
	walk(tree, "")
	return out
}

func TestCreateCommitTreeRebuild(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	head1, tree := commitRoot(t, s, "r1", map[string]string{
		"README":            "readme-v1",
		"lib/a.txt":         "a-v1",
		"lib/nested/SHARED": "shared-v1",
		"docs/guide.md":     "guide",
	})

	// v2: modify, add (new nested dir), delete → empty dirs pruned (docs/ vanishes).
	putFiles(t, s, "r1", map[string]string{
		"lib/a.txt":     "a-v2",
		"top.txt":       "top",
		"new/dir/f.txt": "fresh",
	})
	head2, err := s.CreateCommit(ctx, CommitRequest{
		Repo: "r1", Base: head1, Message: "v2",
		Changes: []Change{
			{Path: "lib/a.txt", Blob: model.ObjectOID(model.BlobType, []byte("a-v2"), model.SHA1)},
			{Path: "top.txt", Blob: model.ObjectOID(model.BlobType, []byte("top"), model.SHA1)},
			{Path: "new/dir/f.txt", Blob: model.ObjectOID(model.BlobType, []byte("fresh"), model.SHA1)},
			{Path: "docs/guide.md", Delete: true},
		},
	})
	require.NoError(t, err, "第二次提交失败")

	assert.Equal(t, map[string]string{
		"README":            "readme-v1",
		"lib/a.txt":         "a-v2",
		"lib/nested/SHARED": "shared-v1",
		"top.txt":           "top",
		"new/dir/f.txt":     "fresh",
	}, treeFiles(t, s, "r1", head2.Tree), "重建树内容不符合预期（docs 空目录未剪枝或增删未生效）")

	// Parents/order: the commit's parent is the previous head, tree reachable.
	ci, err := s.Objects.CommitInfo("r1", head2.Head)
	require.NoError(t, err)
	require.Len(t, ci.Parents, 1)
	assert.Equal(t, head1, ci.Parents[0], "父提交应为 v1 head")
	assert.NotEqual(t, tree, head2.Tree, "树应已更新")
	_ = tree
}

func mustHead(t *testing.T, s *Service, repo string) model.OID {
	t.Helper()
	head, _, _, err := s.viewHead(context.Background(), repo, model.RootAnchor, "")
	require.NoError(t, err)
	return head
}

func TestFetchTreesDiff(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	files := map[string]string{"README": "v1", "lib/a.txt": "a", "lib/nested/s.txt": "s"}
	head1, tree1 := commitRoot(t, s, "r2", files)

	full, err := s.FetchTrees(ctx, FetchRequest{Repo: "r2"})
	require.NoError(t, err, "全量 fetch 失败")
	assert.Equal(t, head1, full.Head)
	assert.Equal(t, tree1, full.Tree)
	assert.Len(t, full.Blobs, 3, "全量应内联 3 个 blob")
	assert.GreaterOrEqual(t, len(full.Trees), 3, "README 根树 + lib + nested")

	// Change one file: diff fetch returns only the touched chain.
	putFiles(t, s, "r2", map[string]string{"lib/nested/s.txt": "s2"})
	head2, err := s.CreateCommit(ctx, CommitRequest{
		Repo: "r2", Base: head1, Message: "touch nested",
		Changes: []Change{{Path: "lib/nested/s.txt", Blob: model.ObjectOID(model.BlobType, []byte("s2"), model.SHA1)}},
	})
	require.NoError(t, err)

	diff, err := s.FetchTrees(ctx, FetchRequest{Repo: "r2", HaveTree: tree1})
	require.NoError(t, err, "增量 fetch 失败")
	assert.Equal(t, head2.Head, diff.Head)
	assert.False(t, diff.Full, "旧树可读时不应退化为全量")
	assert.Len(t, diff.Blobs, 1, "增量只应带 1 个新 blob")
	assert.Contains(t, diff.Blobs, string(model.ObjectOID(model.BlobType, []byte("s2"), model.SHA1)))
	assert.Len(t, diff.Trees, 3, "增量树 = 根 + lib + nested（README 链不变）")

	// Degradation: an unknown haveTree falls back to a full walk.
	weird, err := s.FetchTrees(ctx, FetchRequest{Repo: "r2", HaveTree: model.OID("0123456789012345678901234567890123456789")})
	require.NoError(t, err)
	assert.True(t, weird.Full, "未知 haveTree 应退化为全量")
	assert.Len(t, weird.Blobs, 3)
}

func TestCreateCommitBaseMoved(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	files := map[string]string{"a": "1"}
	head1, _ := commitRoot(t, s, "r3", files)

	// A peer commits first; the stale client's base no longer matches.
	putFiles(t, s, "r3", map[string]string{"a": "2"})
	_, err := s.CreateCommit(ctx, CommitRequest{
		Repo: "r3", Base: head1, Message: "peer",
		Changes: []Change{{Path: "a", Blob: model.ObjectOID(model.BlobType, []byte("2"), model.SHA1)}},
	})
	require.NoError(t, err)

	putFiles(t, s, "r3", map[string]string{"a": "3"})
	_, err = s.CreateCommit(ctx, CommitRequest{
		Repo: "r3", Base: head1, Message: "stale",
		Changes: []Change{{Path: "a", Blob: model.ObjectOID(model.BlobType, []byte("3"), model.SHA1)}},
	})
	require.Error(t, err, "过期 base 必须被拒绝")
	var moved *ErrBaseMoved
	require.ErrorAs(t, err, &moved, "错误应为 ErrBaseMoved（CLI 据此映射 409/退出码 1）")

	code, _ := Translate(err)
	assert.Equal(t, "CONFLICT_BASE_MOVED", code)
}

func TestPutObjectsHashVerify(t *testing.T) {
	s := newService(t)
	require.NoError(t, s.Meta.CreateRepo(context.Background(), "r4", model.SHA1))
	oid := model.ObjectOID(model.BlobType, []byte("x"), model.SHA1)
	err := s.PutObjects(context.Background(), "r4", []PutObject{{OID: oid, Type: "blob", Data: []byte("y")}})
	require.Error(t, err, "内容与 OID 不符必须拒绝")
	code, _ := Translate(err)
	assert.Equal(t, "INVALID_ARG", code)
}
