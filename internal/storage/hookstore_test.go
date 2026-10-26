package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

// TestHookRegistrySQLite pins the hooks table round trip + validation, the
// persistence under multi-gateway deployments (every gateway reads the same
// registry through the metadata plane).
func TestHookRegistrySQLite(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hooks.db")
	meta, err := NewSQLiteMetaStore(dbPath)
	require.NoError(t, err)
	defer meta.Close()
	require.NoError(t, meta.CreateRepo(ctx, "r", model.SHA1))

	h := HookInfo{
		ID: "gate", Dir: "lib/nested", Events: []string{"pre-receive", "update"},
		URL: "http://hooks.internal/gate", Secret: "s", TimeoutMS: 2500,
	}
	require.NoError(t, meta.SetHook(ctx, "r", h))

	// Reopen: registrations survive restarts like every other meta row.
	meta.Close()
	meta2, err := NewSQLiteMetaStore(dbPath)
	require.NoError(t, err)
	defer meta2.Close()
	hooks, err := meta2.ListHooks(ctx, "r")
	require.NoError(t, err)
	require.Len(t, hooks, 1)
	assert.Equal(t, h, hooks[0])

	require.NoError(t, meta2.SetHook(ctx, "r", HookInfo{ID: "gate", Events: []string{"post-receive"}, URL: "http://hooks.internal/gate"}))
	hooks, err = meta2.ListHooks(ctx, "r")
	require.NoError(t, err)
	require.Len(t, hooks, 1, "同 ID upsert 覆盖")
	assert.Equal(t, []string{"post-receive"}, hooks[0].Events)

	require.NoError(t, meta2.DeleteHook(ctx, "r", "gate"))
	require.NoError(t, meta2.DeleteHook(ctx, "r", "gate"), "删除幂等")
	hooks, err = meta2.ListHooks(ctx, "r")
	require.NoError(t, err)
	assert.Empty(t, hooks)

	// Validation: bad event, bad URL, unknown repo.
	assert.Error(t, meta2.SetHook(ctx, "r", HookInfo{ID: "x", Events: []string{"nope"}, URL: "http://x/"}))
	assert.Error(t, meta2.SetHook(ctx, "r", HookInfo{ID: "x", Events: []string{"update"}, URL: "ftp://x/"}))
	assert.Error(t, meta2.SetHook(ctx, "missing", HookInfo{ID: "x", Events: []string{"update"}, URL: "http://x/"}))
}
