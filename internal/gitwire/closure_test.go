package gitwire

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Unit coverage for the streaming-serve primitives (docs/03 §3/§3a): the
// type-directed closure enumeration, the serve-rung counter classification,
// and the two tryStreamingServe pre-conditions that decide the rung before
// any byte moves (response cache first; small-repo cutoff). The multi-pack
// serving over the wire is T55/T57 (integration).

// zooRepo is a type-zoo source repo built with real git and packed into one
// pack: two commits, a nested directory, an executable entry, a symlink
// entry, a gitlink to a commit in another repo, and an annotated tag at the
// tip. Every OID below is ground truth from git itself.
type zooRepo struct {
	dir                         string
	commit1, commit2, tag       model.OID
	tree1, tree2, subTree       model.OID
	file1, nested, script, link model.OID
	gitlink                     model.OID
	pack                        []byte
}

func zooWrite(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func zooGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "core.autocrlf=false",
		"-c", "user.name=it", "-c", "user.email=it@banyan.dev",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	require.NoError(t, cmd.Run(), "git %v (dir=%s) failed\n%s", args, dir, buf.String())
	return buf.String()
}

func zooRev(t *testing.T, dir, rev string) model.OID {
	t.Helper()
	return model.OID(strings.TrimSpace(zooGit(t, dir, "rev-parse", rev)))
}

func zooHashObject(t *testing.T, dir, content string) model.OID {
	t.Helper()
	cmd := exec.Command("git", "-c", "core.autocrlf=false", "hash-object", "-w", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	require.NoError(t, err)
	return model.OID(strings.TrimSpace(string(out)))
}

func buildZooRepo(t *testing.T) *zooRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI not available")
	}
	z := &zooRepo{dir: t.TempDir()}
	g := func(args ...string) string { return zooGit(t, z.dir, args...) }

	g("init", "-q", "-b", "main", ".")
	zooWrite(t, filepath.Join(z.dir, "file1.txt"), "root file\n")
	zooWrite(t, filepath.Join(z.dir, "sub", "nested.txt"), "nested file\n")
	g("add", ".")
	g("commit", "-q", "-m", "zoo base")
	z.commit1 = zooRev(t, z.dir, "HEAD")
	z.tree1 = zooRev(t, z.dir, "HEAD^{tree}")

	zooWrite(t, filepath.Join(z.dir, "script.sh"), "#!/bin/sh\necho zoo\n")
	g("add", "script.sh")
	g("update-index", "--chmod=+x", "script.sh")
	z.link = zooHashObject(t, z.dir, "file1.txt") // symlink blob: target path as content
	g("update-index", "--add", "--cacheinfo", "120000,"+string(z.link)+",link")

	scratch := t.TempDir()
	zooGit(t, scratch, "init", "-q", "-b", "main", ".")
	zooWrite(t, filepath.Join(scratch, "f.txt"), "external repo content\n")
	zooGit(t, scratch, "add", ".")
	zooGit(t, scratch, "commit", "-q", "-m", "external")
	z.gitlink = zooRev(t, scratch, "HEAD")
	g("update-index", "--add", "--cacheinfo", "160000,"+string(z.gitlink)+",vendor")

	g("commit", "-q", "-m", "zoo: exec + symlink + gitlink")
	z.commit2 = zooRev(t, z.dir, "HEAD")
	z.tree2 = zooRev(t, z.dir, "HEAD^{tree}")
	z.subTree = zooRev(t, z.dir, "HEAD:sub")
	z.file1 = zooRev(t, z.dir, "HEAD:file1.txt")
	z.nested = zooRev(t, z.dir, "HEAD:sub/nested.txt")
	z.script = zooRev(t, z.dir, "HEAD:script.sh")

	g("tag", "-a", "v1", "-m", "zoo tag")
	z.tag = zooRev(t, z.dir, "v1")

	g("repack", "-a", "-d", "-q")
	packs, err := filepath.Glob(filepath.Join(z.dir, ".git", "objects", "pack", "*.pack"))
	require.NoError(t, err)
	require.Len(t, packs, 1, "repack -a -d must leave exactly one pack")
	z.pack, err = os.ReadFile(packs[0])
	require.NoError(t, err)
	return z
}

// ingestZoo packs the zoo repo into a fresh ObjectService over the local
// backend and returns a Server addressing it (the enumeration and streaming
// rung only need s.Objects — no HTTP).
func ingestZoo(t *testing.T) (*zooRepo, *Server) {
	t.Helper()
	z := buildZooRepo(t)
	meta := storage.NewMemoryMetaStore()
	require.NoError(t, meta.CreateRepo(t.Context(), "r", model.SHA1))
	local := storage.NewLocalShardStore(t.TempDir())
	svc := storage.NewObjectService(meta, local, []storage.Node{{ID: "n1", Local: true}}, model.SHA1)
	_, err := svc.IngestPack(t.Context(), "r", z.pack)
	require.NoError(t, err)
	return z, &Server{Objects: svc}
}

func TestEnumerateClosureTypeZoo(t *testing.T) {
	z, srv := ingestZoo(t)
	vr := VirtualRepo{Repo: "r"}

	typed, err := srv.enumerateClosure(t.Context(), vr, []model.OID{z.tag})
	require.NoError(t, err)

	// The annotated tag itself belongs to the served set (git clones the
	// tag object), peeled to its commit.
	assert.Equal(t, model.TagType, typed[z.tag])
	assert.Equal(t, model.CommitType, typed[z.commit2])
	assert.Equal(t, model.CommitType, typed[z.commit1], "the walk covers parents")
	assert.Equal(t, model.TreeType, typed[z.tree1])
	assert.Equal(t, model.TreeType, typed[z.tree2])
	assert.Equal(t, model.TreeType, typed[z.subTree])
	assert.Equal(t, model.BlobType, typed[z.file1])
	assert.Equal(t, model.BlobType, typed[z.nested])
	assert.Equal(t, model.BlobType, typed[z.script], "executable entries are blob leaves")
	assert.Equal(t, model.BlobType, typed[z.link], "symlink entries are blob leaves")
	assert.Len(t, typed, 10, "2 commits + 3 trees + 4 blobs + 1 tag")
	_, hasGitlink := typed[z.gitlink]
	assert.False(t, hasGitlink, "gitlink points into another repo — never part of this store's packs")

	// A branch-shaped want carries no tag object.
	typed, err = srv.enumerateClosure(t.Context(), vr, []model.OID{z.commit2})
	require.NoError(t, err)
	_, hasTag := typed[z.tag]
	assert.False(t, hasTag)
	assert.Len(t, typed, 9)
}

func TestCountServeRungClassification(t *testing.T) {
	before := map[string]int64{
		"thin":          fetchThin.Value(),
		"closure-cache": fetchClosureCache.Value(),
		"verbatim":      fetchVerbatimPack.Value(),
		"encoded":       fetchEncoded.Value(),
		"streamed":      fetchStreamed.Value(),
		"accepted":      pushAccepted.Value(),
	}
	countServeRung("thin")
	countServeRung("thin-error")
	countServeRung("closure-cache")
	countServeRung("exact-pack-stream 0123456789012345678901234567890123456789")
	countServeRung("some-unknown-rung")
	assert.Equal(t, before["thin"]+2, fetchThin.Value())
	assert.Equal(t, before["closure-cache"]+1, fetchClosureCache.Value())
	assert.Equal(t, before["verbatim"]+1, fetchVerbatimPack.Value())
	assert.Equal(t, before["encoded"]+1, fetchEncoded.Value(), "unknown rungs count as the ladder's end state")
	assert.Equal(t, before["streamed"], fetchStreamed.Value())
	assert.Equal(t, before["accepted"], pushAccepted.Value())
}

func TestStreamingServeSmallRepoFallsBack(t *testing.T) {
	z, srv := ingestZoo(t)
	var out bytes.Buffer
	assert.False(t, srv.tryStreamingServe(t.Context(), &out, VirtualRepo{Repo: "r"}, []model.OID{z.commit2}, false),
		"small pack-group keeps the closure ladder (docs/03 §3a cutoff)")
	assert.Zero(t, out.Len(), "no bytes may move before the fallback decision")
}

func TestStreamingServeResponseCacheFirst(t *testing.T) {
	z, srv := ingestZoo(t)
	wants := []model.OID{z.commit2}
	sentinel := []byte("PACK-cached-response-sentinel")
	srv.Objects.StoreResponsePack("r", "", wants, false, sentinel)

	var out bytes.Buffer
	assert.True(t, srv.tryStreamingServe(t.Context(), &out, VirtualRepo{Repo: "r"}, wants, false))
	assert.Equal(t, sentinel, out.Bytes(), "a repeat clone answers from the response cache, enumeration-free")

	// A different want set misses the cache; on this small repo that means
	// falling back to the closure ladder — cache keys include the wants.
	out.Reset()
	assert.False(t, srv.tryStreamingServe(t.Context(), &out, VirtualRepo{Repo: "r"}, []model.OID{z.commit1}, false))
}
