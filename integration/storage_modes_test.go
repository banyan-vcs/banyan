// storage_modes_test.go — T40: the two storage addressing modes (docs/03
// §6) end to end on the in-process cluster with the real git CLI. Named
// addressing keeps readable on-disk repo directories but a rename must move
// data on every holder before the registry flips; hashed addressing stores
// under opaque @hashed/… keys derived from the immutable storage id, so a
// rename is registry-only and the storage layer never learns a name.
package integration

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/storage"
)

// dirNames lists the direct children of dir ("" when absent).
func dirNames(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// walkNames snapshots every path under root (relative, slash-separated) —
// the byte-level "storage did not move" assertion of hashed mode.
func walkNames(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	return out
}

// hashedPackKeys walks every node root and returns the set of storage keys
// that actually hold packs ("@hashed/<xx>/<yy>/<digest>" — the packs dir's
// ancestor three levels up).
func hashedPackKeys(t *testing.T, roots map[string]string) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() || d.Name() != "packs" {
				return err
			}
			// packs sits at <root>/<key>/packs — the key IS its parent.
			rel, rerr := filepath.Rel(root, filepath.Dir(p))
			if rerr == nil {
				keys[filepath.ToSlash(rel)] = true
			}
			return nil
		})
	}
	return keys
}

// renameRepo drives the public rename endpoint through the gateway.
func renameRepo(t *testing.T, gw, from, to string) {
	t.Helper()
	resp, err := http.Post(gw+"/api/v1/repos/"+from+"/rename",
		"application/json", strings.NewReader(fmt.Sprintf(`{"to":%q}`, to)))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "rename %s→%s 应成功:\n%s", from, to, body)
}

func TestStorageModes(t *testing.T) {
	needGit(t)
	t.Run("named", func(t *testing.T) {
		c := startHACluster(t, 4)
		src := t.TempDir()
		git(t, src, "init", "-q", "-b", "main", ".")
		write(t, filepath.Join(src, "README.md"), "named mode\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "seed")
		git(t, src, "push", "-q", c.GW.URL+"/r/alpha.git", "main")

		// Readable directories: some node holds <root>/alpha/packs.
		var holders []string
		for id, root := range c.Roots {
			if _, err := os.Stat(filepath.Join(root, "alpha", "packs")); err == nil {
				holders = append(holders, id)
			}
		}
		require.NotEmpty(t, holders, "named 模式磁盘上应有可读的 alpha 目录")

		// Rename: data moves on every holder, then the registry flips.
		renameRepo(t, c.GW.URL, "alpha", "beta")
		for id, root := range c.Roots {
			if _, err := os.Stat(filepath.Join(root, "alpha")); err == nil {
				t.Errorf("节点 %s 的 alpha 目录应已改名", id)
			}
		}
		nBeta := 0
		for _, root := range c.Roots {
			if _, err := os.Stat(filepath.Join(root, "beta", "packs")); err == nil {
				nBeta++
			}
		}
		assert.Equal(t, len(holders), nBeta, "改名后持有集不变，目录改为 beta")

		home := cloneRepo(t, &server{Server: c.GW}, c.GW.URL+"/r/beta.git")
		assert.Equal(t, "named mode\n", readFile(t, filepath.Join(home, "README.md")))
		_, err := gitTry("", "ls-remote", c.GW.URL+"/r/alpha.git")
		require.Error(t, err, "旧名应不存在")
	})

	t.Run("hashed", func(t *testing.T) {
		c := startHAClusterMode(t, 4, "hashed")
		src := t.TempDir()
		git(t, src, "init", "-q", "-b", "main", ".")
		write(t, filepath.Join(src, "README.md"), "hashed mode\n")
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "seed")
		git(t, src, "push", "-q", c.GW.URL+"/r/gamma.git", "main")

		// No node learns the name: holders store under opaque
		// @hashed/<xx>/<yy>/<digest> fanout dirs; non-holders stay empty.
		anyHashed := false
		for id, root := range c.Roots {
			ents := dirNames(root)
			for _, e := range ents {
				if strings.HasPrefix(e, "@hashed") {
					anyHashed = true
				}
			}
			assert.NotContains(t, ents, "gamma", "节点 %s 不应出现仓库名目录", id)
		}
		require.True(t, anyHashed, "持有节点应以 @hashed 目录存储")
		// The storage id is served with the repo info (gateway mapping).
		info, err := c.Meta.GetRepo(context.Background(), "gamma")
		require.NoError(t, err)
		assert.NotEmpty(t, info.StorageID, "仓库应有不可变 storage id")
		assert.Equal(t, map[string]bool{storage.HashedRepoPath(info.StorageID): true},
			hashedPackKeys(t, c.Roots), "磁盘键应为 storageID 的 fanout 哈希")

		// Rename: the registry flips; not one byte moves on any node.
		before := map[string][]string{}
		for id, root := range c.Roots {
			before[id] = walkNames(t, root)
		}
		renameRepo(t, c.GW.URL, "gamma", "delta")
		for id, root := range c.Roots {
			assert.Equal(t, before[id], walkNames(t, root), "hashed 模式 rename 不得触碰节点 %s 磁盘", id)
		}

		home := cloneRepo(t, &server{Server: c.GW}, c.GW.URL+"/r/delta.git")
		assert.Equal(t, "hashed mode\n", readFile(t, filepath.Join(home, "README.md")))
		_, err = gitTry("", "ls-remote", c.GW.URL+"/r/gamma.git")
		require.Error(t, err, "旧名应不存在")

		// Push AFTER the rename lands under the SAME opaque key (the storage
		// id is immutable), not a fresh hash of the new name.
		appendLine(t, filepath.Join(src, "README.md"), "after rename\n")
		git(t, src, "commit", "-qam", "post-rename")
		git(t, src, "push", "-q", c.GW.URL+"/r/delta.git", "main")
		for id, root := range c.Roots {
			for _, e := range dirNames(root) {
				assert.True(t, strings.HasPrefix(e, "@hashed"), "节点 %s 不得出现 delta 名目录", id)
			}
		}
		home2 := cloneRepo(t, &server{Server: c.GW}, c.GW.URL+"/r/delta.git")
		assert.Contains(t, readFile(t, filepath.Join(home2, "README.md")), "after rename")
	})
}
