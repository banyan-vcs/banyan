// T84 — subtree projection refs (docs/20 §7, the ref-namespace projection):
// every top-level directory of the default branch is addressable as a repo
// of its own through refs/banyan/tree/<dir> — the same subtree view the
// /tree/<dir> face serves, inside the parent's ref space, so whole-repo
// front ends (GitLab in gitaly mode) get subtree clone/push with one URL,
// one credential, one permission surface.
package subtree

import (
	"github.com/banyan-vcs/banyan/integration/support"

	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lsRemoteRef returns the OID advertised for one ref ("" when absent).
func lsRemoteRef(t *testing.T, url, ref string) string {
	t.Helper()
	out := support.Git(t, "", "ls-remote", url)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 2 && f[1] == ref {
			return f[0]
		}
	}
	return ""
}

func TestT84SubtreeProjectionRefs(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)

	// Seed a repo with two top-level directories (lib, app) plus a
	// nested one under lib (the projection set is top-level only).
	src := t.TempDir()
	support.Git(t, src, "init", "-q", "-b", "main", ".")
	support.Write(t, filepath.Join(src, "README.md"), "readme-v1\n")
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-v1\n")
	support.Write(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "base\n")
	support.Write(t, filepath.Join(src, "app", "x.go"), "package app\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "seed")
	support.Git(t, src, "push", "-q", s.RepoURL("t84"), "main")

	// ① The projection set: top-level dirs of the default branch — no
	// blobs, no nested scopes.
	out := support.Git(t, "", "ls-remote", s.RepoURL("t84"))
	assert.Contains(t, out, "refs/banyan/tree/lib\n", "lib projected")
	assert.Contains(t, out, "refs/banyan/tree/app\n", "app projected")
	assert.NotContains(t, out, "refs/banyan/tree/lib/nested", "top-level only")
	assert.NotContains(t, out, "refs/banyan/tree/README.md", "dirs only")

	// ② The projection OID is exactly what the /tree/<dir> face serves.
	projTip := lsRemoteRef(t, s.RepoURL("t84"), "refs/banyan/tree/lib")
	subTip := lsRemoteRef(t, s.RepoURL("t84")+"/tree/lib", "refs/heads/main")
	require.NotEmpty(t, projTip)
	assert.Equal(t, subTip, projTip, "projection ref == subtree face view")

	// ③ Materialize the projection through fetch refspecs (git's clone -b
	// only accepts branch/tag short names — find_remote_branch never looks
	// past refs/heads|tags — so the client recipe is clone + one fetch;
	// the server side serves either): the working tree IS the directory,
	// history scoped, fsck clean.
	dst := t.TempDir()
	support.Git(t, "", "clone", "-q", s.RepoURL("t84"), dst)
	support.Git(t, dst, "fetch", "-q", "origin", "refs/banyan/tree/lib:refs/heads/lib")
	support.Git(t, dst, "checkout", "-q", "lib")
	assert.Equal(t, "a-v1\n", support.ReadFile(t, filepath.Join(dst, "a.txt")))
	assert.NoFileExists(t, filepath.Join(dst, "README.md"), "root files stay in the root repo")
	support.FsckClean(t, dst)
	mainBefore := lsRemoteRef(t, s.RepoURL("t84"), "refs/heads/main")

	// ④ Push through the projection: lands in lib's timeline and
	// assembles upward — the root view advances with it.
	support.AppendLine(t, filepath.Join(dst, "a.txt"), "a-v2\n")
	support.Git(t, dst, "commit", "-qam", "lib change via projection ref")
	support.Git(t, dst, "push", "-q", "origin", "HEAD:refs/banyan/tree/lib")

	root := support.CloneRepo(t, s, s.RepoURL("t84"))
	assert.Equal(t, "a-v1\na-v2\n", support.ReadFile(t, filepath.Join(root, "lib", "a.txt")),
		"projection push assembles into the root view")
	mainAfter := lsRemoteRef(t, s.RepoURL("t84"), "refs/heads/main")
	assert.NotEqual(t, mainBefore, mainAfter, "root tip advanced")
	assert.NotEqual(t, projTip, lsRemoteRef(t, s.RepoURL("t84"), "refs/banyan/tree/lib"),
		"projection ref advanced too")

	// ⑤ Mixed push: a root-branch ref and a projection ref in ONE
	// receive-pack — each routes by its own namespace. Subtree work rides
	// the projection lineage (the client-side fast-forward precondition and
	// the server's rollback guard both demand it: a root-clone chain is a
	// different lineage than the projection's history).
	work := support.CloneRepo(t, s, s.RepoURL("t84"))
	support.Git(t, work, "fetch", "-q", "origin", "refs/banyan/tree/app:refs/heads/app-proj")
	support.Git(t, work, "checkout", "-q", "app-proj")
	support.AppendLine(t, filepath.Join(work, "x.go"), "// touch\n")
	support.Git(t, work, "commit", "-qam", "app via projection")
	support.Git(t, work, "checkout", "-q", "main")
	support.AppendLine(t, filepath.Join(work, "README.md"), "readme-v2\n")
	support.Git(t, work, "commit", "-qam", "root change")
	out, err := support.GitTry(work, "push", "-q", "origin", "app-proj:refs/banyan/tree/app", "main")
	require.NoError(t, err, "mixed push must land both refs:\n%s", out)
	fresh := support.CloneRepo(t, s, s.RepoURL("t84"))
	assert.Contains(t, support.ReadFile(t, filepath.Join(fresh, "app", "x.go")), "touch")
	assert.Contains(t, support.ReadFile(t, filepath.Join(fresh, "README.md")), "readme-v2")

	// ⑥ The declines: deletions and nested scopes carry the /tree/ face's
	// semantics, spelled out on the wire.
	out, err = support.GitTry(work, "push", "-q", "origin", ":refs/banyan/tree/lib")
	require.Error(t, err, "projection deletion must fail")
	assert.Contains(t, out, "deletion in subtree view not supported")

	out, err = support.GitTry(work, "push", "-q", "origin", "HEAD:refs/banyan/tree/lib/nested")
	require.Error(t, err, "nested projection must fail")
	assert.Contains(t, out, "top-level directory")
}
