package meta

// The legacy-upgrade path: a database shaped before the migration ledger
// (pre-storage_id repos, pre-done fences) replays history as no-ops-plus-
// guarded-ALTERs on open — columns appear, the sqlite storage_id backfill
// fills, the indexes land, and the store serves the hierarchy API on top.
import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
)

func TestLegacyShapeUpgradesOnOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	// The very first shape: repos without storage_id/force_push/parent,
	// rename_fence without done. A repo row and a completed-rename
	// tombstone stand in for live data.
	raw, err := sql.Open("sqlite", sqliteDSN(path))
	require.NoError(t, err)
	for _, stmt := range []string{
		`CREATE TABLE repos(name TEXT PRIMARY KEY, algo INTEGER NOT NULL, default_branch TEXT NOT NULL)`,
		`CREATE TABLE rename_fence(frm TEXT NOT NULL PRIMARY KEY, dst TEXT NOT NULL)`,
		`INSERT INTO repos(name, algo, default_branch) VALUES ('legacy', 0, 'main')`,
		`INSERT INTO rename_fence(frm, dst) VALUES ('old-name', 'legacy')`,
	} {
		_, err = raw.Exec(stmt)
		require.NoError(t, err)
	}
	require.NoError(t, raw.Close())

	// Open through the store: the migrations run, adding the missing
	// columns and the query-driven indexes, backfilling storage_id.
	s, err := NewSQLiteMetaStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	info, err := s.GetRepo(ctx, "legacy")
	require.NoError(t, err)
	assert.NotEmpty(t, info.StorageID, "the sqlite backfill filled the hashed-addressing identity")
	assert.Equal(t, "", info.Parent, "the hierarchy column arrived defaulted")
	assert.Equal(t, "main", info.DefaultBranch)

	// A fence row predating the done column reads as IN-FLIGHT (the
	// historical default 0 — a fence without the marker was an active one),
	// so the retired-looking name is fenced, not resurrectable.
	err = s.CreateRepo(ctx, "old-name", model.SHA1)
	assert.ErrorIs(t, err, ErrRepoRenaming)

	// The hierarchy works on the upgraded shape.
	require.NoError(t, s.CreateRepo(ctx, "child", model.SHA1))
	require.NoError(t, s.SetRepoParent(ctx, "child", "legacy"))
	repos, err := s.ListRepos(ctx)
	require.NoError(t, err)
	byID := map[string]RepoInfo{}
	for _, r := range repos {
		byID[r.ID] = r
	}
	assert.Equal(t, "legacy", byID["child"].Parent)

	// The indexes landed (existence probes over the store's own pool).
	orm := s.orm
	for _, idx := range []string{"idx_objects_pack", "idx_repo_pools_pool", "idx_tokens_owner"} {
		var n int
		require.NoError(t, orm.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", idx).Row().Scan(&n), idx)
		assert.Equal(t, 1, n, "%s present", idx)
	}

	// Reopen: the ledger says everything is applied — the legacy row keeps
	// its backfilled identity, no re-run churn.
	require.NoError(t, s.Close())
	s2, err := NewSQLiteMetaStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	info2, err := s2.GetRepo(ctx, "legacy")
	require.NoError(t, err)
	assert.Equal(t, info.StorageID, info2.StorageID, "the backfilled identity is stable across reopens")
	var ledger int
	require.NoError(t, s2.orm.Raw("SELECT COUNT(*) FROM meta_migrations").Row().Scan(&ledger))
	assert.Equal(t, len(metaSteps), ledger, "one ledger row per step, nothing re-ran")
}
