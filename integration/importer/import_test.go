// T94 — the importer engine (internal/importer): a server-side fetch from
// one banyan's smart-HTTP face into another, bootstrap-style. The same
// engine serves the gitaly FetchRemote surface (T69) and the web tier's
// repo-import API, so this pins the parts T69 does not reach: the streamed
// pack spool, the symref default-branch probe and the ref count.
package importer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/integration/support"
	bimporter "github.com/banyan-vcs/banyan/internal/importer"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// serviceFor builds the service layer around a test plane, the same wiring
// rpc.NewServer performs internally.
func serviceFor(t *testing.T, ms meta.MetaStore, objects *storage.ObjectService) *service.Service {
	t.Helper()
	return service.New(ms, objects, model.SHA1)
}

func TestT94ImportFromSiblingServer(t *testing.T) {
	support.NeedGit(t)
	src, _, _ := support.NewServerPlane(t)

	// A source repo with a non-main default branch and a tag: the import
	// must land both and adopt the remote HEAD.
	root := t.TempDir()
	support.Git(t, root, "init", "-q", "-b", "trunk", ".")
	support.Write(t, root+"/readme.md", "import me\n")
	support.Git(t, root, "add", "-A")
	support.Git(t, root, "commit", "-qm", "c1")
	support.Git(t, root, "tag", "v1")
	support.Git(t, root, "push", "-q", src.RepoURL("t91-src"), "trunk", "--tags")

	dst, dstMeta, dstObjects := support.NewServerPlane(t)
	require.NoError(t, dstMeta.CreateRepo(context.Background(), "t91-copy", model.SHA1))
	svc := serviceFor(t, dstMeta, dstObjects)

	res, err := bimporter.Fetch(context.Background(), dstObjects, svc, "t91-copy", src.RepoURL("t91-src"), "", true)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Refs, "trunk + tag")
	assert.Equal(t, "trunk", res.DefaultBranch, "symref HEAD adopted")
	// The engine reports the remote HEAD; adopting it is the caller's call
	// (the import API does this right after Fetch).
	require.NoError(t, dstMeta.SetDefaultBranch(context.Background(), "t91-copy", res.DefaultBranch))

	// The imported repo is a first-class banyan repo: clone it back and
	// compare the checkout; the pack spool path (no in-memory buffering)
	// is exercised by the same round trip.
	out := t.TempDir()
	support.Git(t, out, "clone", "-q", dst.RepoURL("t91-copy"), "c")
	assert.Equal(t, "import me\n", support.ReadFile(t, out+"/c/readme.md"))
	assert.Equal(t, "trunk", strings.TrimSpace(support.Git(t, out+"/c", "branch", "--show-current")))
	assert.Contains(t, support.Git(t, out+"/c", "tag"), "v1")
	support.FsckClean(t, out+"/c")
}

func TestT94ImportRejectsSSHRemotes(t *testing.T) {
	support.NeedGit(t)
	_, meta, objects := support.NewServerPlane(t)
	svc := serviceFor(t, meta, objects)
	_, err := bimporter.Fetch(context.Background(), objects, svc, "x", "git@github.com:example/repo.git", "", true)
	assert.ErrorIs(t, err, bimporter.ErrSSHRemote)
}
