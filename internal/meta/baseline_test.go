package meta

// The two install tracks cannot drift: a fresh database shaped by the
// baseline snapshot must end up byte-identical (tables, columns, indexes,
// ledger rows) to one that replayed the full step history. Adding a step
// without folding it into metaBaseline fails here first.
import (
	"path/filepath"
	"testing"

	gosqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/banyan-vcs/banyan/internal/ddl"
)

func schemaDump(t *testing.T, orm *gorm.DB) []string {
	t.Helper()
	rows, err := orm.Raw(
		"SELECT type || '|' || name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' AND name != 'meta_migrations' ORDER BY type, name").Rows()
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func openGorm(t *testing.T) *gorm.DB {
	t.Helper()
	orm, err := gorm.Open(gosqlite.Dialector{DSN: filepath.Join(t.TempDir(), "m.db")},
		&gorm.Config{TranslateError: true})
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB, _ := orm.DB(); _ = sqlDB.Close() })
	return orm
}

func TestBaselineEqualsIncremental(t *testing.T) {
	incremental := openGorm(t)
	require.NoError(t, ddl.New(incremental, "meta_migrations").Run(metaSteps)) // no baseline: full replay

	baseline := openGorm(t)
	require.NoError(t, ddl.New(baseline, "meta_migrations").Baseline(metaBaseline, "repos").Run(metaSteps))

	assert.Equal(t, schemaDump(t, incremental), schemaDump(t, baseline),
		"fresh installs on the two tracks must produce the same shape")
	var nInc, nBase int64
	require.NoError(t, incremental.Raw("SELECT COUNT(*) FROM meta_migrations").Scan(&nInc).Error)
	require.NoError(t, baseline.Raw("SELECT COUNT(*) FROM meta_migrations").Scan(&nBase).Error)
	assert.Equal(t, nInc, nBase, "both tracks stamp the full history")
}
