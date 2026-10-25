package merge

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

func TestBlobsClean(t *testing.T) {
	cases := []struct {
		name               string
		base, ours, theirs string
		want               string
	}{
		{"两端不同行编辑", "a\nb\nc\n", "A\nb\nc\n", "a\nb\nC\n", "A\nb\nC\n"},
		{"仅一侧改动", "a\nb\n", "a\nb\n", "a\nB\n", "a\nB\n"},
		{"相同改动取一次", "a\n", "a\nx\n", "a\nx\n", "a\nx\n"},
		{"中间编辑与尾部追加不重叠", "a\nb\nc\nd\n", "A\nb\nc\nd\n", "a\nb\nc\nd\nY\n", "A\nb\nc\nd\nY\n"},
		{"空基 add/add 相同", "", "same\n", "same\n", "same\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, clean := Blobs([]byte(c.base), []byte(c.ours), []byte(c.theirs), Options{})
			assert.True(t, clean, "应干净合并:\n%s", got)
			assert.Equal(t, c.want, string(got))
		})
	}
}

func TestBlobsConflict(t *testing.T) {
	// Same line changed differently → conflict markers around both sides.
	got, clean := Blobs([]byte("a\nb\nc\n"), []byte("a\nOURS\nc\n"), []byte("a\nTHEIRS\nc\n"), Options{OursLabel: "ours", TheirsLabel: "theirs"})
	assert.False(t, clean)
	assert.Equal(t, "a\n<<<<<<< ours\nOURS\n=======\nTHEIRS\n>>>>>>> theirs\nc\n", string(got))

	// Both append at the same point → conflict (git merge-file semantics,
	// verified against the real binary).
	got, clean = Blobs([]byte("a\n"), []byte("a\nb\n"), []byte("a\nc\n"), Options{OursLabel: "ours", TheirsLabel: "theirs"})
	assert.False(t, clean)
	assert.Equal(t, "a\n<<<<<<< ours\nb\n=======\nc\n>>>>>>> theirs\n", string(got))

	// Both insert different content at the same point → conflict.
	got, clean = Blobs([]byte("x\n"), []byte("x\none\n"), []byte("x\ntwo\n"), Options{})
	assert.False(t, clean)
	assert.Contains(t, string(got), "<<<<<<<")

	// Binary input is never merged.
	bin := []byte{0x00, 0x01}
	got, clean = Blobs(bin, []byte("x"), []byte("y"), Options{})
	assert.False(t, clean)
	assert.Equal(t, []byte("x"), got)
}

// oracleCase: clean merges must byte-match `git merge-file -p`.
func TestBlobsGitOracle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cases := []struct{ base, ours, theirs string }{
		{"a\nb\nc\n", "A\nb\nc\n", "a\nb\nC\n"},
		{"a\nb\nc\nd\ne\n", "a\nX\nc\nd\ne\n", "a\nb\nc\nY\ne\n"},
		{"l1\nl2\nl3\nl4\nl5\nl6\n", "l1\nl2\nO1\nl4\nl5\nl6\n", "l1\nl2\nl3\nl4\nT5\nl6\n"},
		{"x\n", "x\nadd-ours\n", "x\nadd-theirs\n"}, // same-point insert → conflict both in git and here
	}
	for i, c := range cases {
		dir := t.TempDir()
		fp := func(name, content string) string {
			p := filepath.Join(dir, name)
			require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
			return p
		}
		base, ours, theirs := fp("base", c.base), fp("ours", c.ours), fp("theirs", c.theirs)
		cmd := exec.Command("git", "merge-file", "-p", "-L", "ours", "-L", "base", "-L", "theirs", ours, base, theirs)
		out, err := cmd.Output()
		gitExit := 0
		if ee, ok := err.(*exec.ExitError); ok {
			gitExit = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("git merge-file: %v", err)
		}
		got, clean := Blobs([]byte(c.base), []byte(c.ours), []byte(c.theirs), Options{OursLabel: "ours", TheirsLabel: "theirs"})
		assert.Equal(t, gitExit != 0, !clean, "case %d: 冲突判定应与 git 一致 (git exit=%d)", i, gitExit)
		assert.Equal(t, string(out), string(got), "case %d: 输出应与 git merge-file 逐字节一致", i)
	}
}

// memStore: in-memory Store for tree-merge tests.
type memStore struct {
	model.MemoryLookup
	blobs map[model.OID][]byte
	trees map[model.OID]*model.Tree
}

func newMemStore() *memStore {
	return &memStore{
		MemoryLookup: *model.NewMemoryLookup(model.SHA1),
		blobs:        map[model.OID][]byte{},
		trees:        map[model.OID]*model.Tree{},
	}
}

func (m *memStore) PutBlob(data []byte) model.OID {
	b := &model.Blob{Content: data}
	oid := b.OID(model.SHA1)
	m.blobs[oid] = data
	m.MemoryLookup.Blobs[oid] = data
	return oid
}

func (m *memStore) PutTree(tr *model.Tree) model.OID {
	oid := tr.OID(model.SHA1)
	m.trees[oid] = tr
	return oid
}

func (m *memStore) Tree(oid model.OID) (*model.Tree, error) {
	if t, ok := m.trees[oid]; ok {
		return t, nil
	}
	return m.MemoryLookup.Tree(oid)
}

func blobEntry(m *memStore, content string) model.TreeEntry {
	return model.TreeEntry{Name: "", Mode: "100644", Target: m.PutBlob([]byte(content))}
}

func mkTree(m *memStore, files map[string]string) *model.Tree {
	tr := &model.Tree{}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sortStrings(names)
	for _, n := range names {
		e := blobEntry(m, files[n])
		e.Name = n
		tr.Entries = append(tr.Entries, e)
	}
	return tr
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestTreesMerge(t *testing.T) {
	m := newMemStore()
	base := m.PutTree(mkTree(m, map[string]string{"shared": "s\n", "gone": "g\n"}))
	ours := m.PutTree(mkTree(m, map[string]string{"shared": "s\n", "gone": "g\n", "ours.txt": "o\n"}))
	theirs := m.PutTree(mkTree(m, map[string]string{"shared": "s\n", "gone": "g\n", "theirs.txt": "t\n"}))

	merged, conflicts, err := Trees(m, model.SHA1, base, ours, theirs, "", Options{})
	require.NoError(t, err)
	assert.Empty(t, conflicts)
	mt, err := m.Tree(merged)
	require.NoError(t, err)
	var contents []string
	for _, e := range mt.Entries {
		b, _ := m.Blob(e.Target)
		contents = append(contents, e.Name+"="+string(b))
	}
	assert.ElementsMatch(t, []string{"shared=s\n", "gone=g\n", "ours.txt=o\n", "theirs.txt=t\n"}, contents)
}

func TestTreesConflictSamePath(t *testing.T) {
	m := newMemStore()
	base := m.PutTree(mkTree(m, map[string]string{"f": "base\n"}))
	ours := m.PutTree(mkTree(m, map[string]string{"f": "ours\n"}))
	theirs := m.PutTree(mkTree(m, map[string]string{"f": "theirs\n"}))

	_, conflicts, err := Trees(m, model.SHA1, base, ours, theirs, "", Options{})
	require.NoError(t, err)
	assert.Equal(t, []string{"f"}, conflicts, "同文件双改应冲突")
}

func TestTreesLineMergeAcrossSides(t *testing.T) {
	// Same file, different lines → blob merge resolves inside the tree merge.
	m := newMemStore()
	base := m.PutTree(mkTree(m, map[string]string{"f": "a\nb\nc\n"}))
	ours := m.PutTree(mkTree(m, map[string]string{"f": "A\nb\nc\n"}))
	theirs := m.PutTree(mkTree(m, map[string]string{"f": "a\nb\nC\n"}))

	merged, conflicts, err := Trees(m, model.SHA1, base, ours, theirs, "", Options{})
	require.NoError(t, err)
	assert.Empty(t, conflicts)
	mt, err := m.Tree(merged)
	require.NoError(t, err)
	b, err := m.Blob(mt.Entries[0].Target)
	require.NoError(t, err)
	assert.Equal(t, "A\nb\nC\n", string(b))
}
