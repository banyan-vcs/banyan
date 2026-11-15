package ddl

// The runner's contract: fresh ledger applies in order, applied steps never
// re-run, guarded steps replay cleanly, a failed step stays unrecorded and
// resurfaces, and an appended step lands on the next run.
import (
	"path/filepath"
	"testing"

	gosqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openORM(t *testing.T) *gorm.DB {
	t.Helper()
	orm, err := gorm.Open(gosqlite.Dialector{DSN: filepath.Join(t.TempDir(), "ddl.db")},
		&gorm.Config{TranslateError: true})
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB, _ := orm.DB(); _ = sqlDB.Close() })
	return orm
}

func ledgerCount(t *testing.T, orm *gorm.DB) int {
	t.Helper()
	var n int64
	require.NoError(t, orm.Raw("SELECT COUNT(*) FROM t_migrations").Scan(&n).Error)
	return int(n)
}

func TestRunnerAppliesAndSkips(t *testing.T) {
	orm := openORM(t)
	ran := map[string]int{}
	steps := []Step{
		{Name: "one", Up: func(q Q) error { ran["one"]++; return q.Exec("CREATE TABLE IF NOT EXISTS a(x TEXT)") }},
		{Name: "two", Up: func(q Q) error { ran["two"]++; return nil }},
	}
	require.NoError(t, New(orm, "t_migrations").Run(steps))
	assert.Equal(t, 1, ran["one"], "applied once")
	assert.Equal(t, 1, ran["two"])
	assert.Equal(t, 2, ledgerCount(t, orm))

	// Re-run: nothing re-applies, ledger unchanged.
	require.NoError(t, New(orm, "t_migrations").Run(steps))
	assert.Equal(t, 1, ran["one"])
	assert.Equal(t, 2, ledgerCount(t, orm))

	// An appended step lands; history stays.
	steps = append(steps, Step{Name: "three", Up: func(q Q) error { ran["three"]++; return nil }})
	require.NoError(t, New(orm, "t_migrations").Run(steps))
	assert.Equal(t, 1, ran["three"], "only the new tail ran")
	assert.Equal(t, 3, ledgerCount(t, orm))
}

func TestRunnerGuardedStepsReplay(t *testing.T) {
	orm := openORM(t)
	steps := []Step{
		{Name: "tables", Up: func(q Q) error { return q.Exec("CREATE TABLE IF NOT EXISTS b(k TEXT PRIMARY KEY)") }},
		{Name: "col", Up: func(q Q) error {
			ok, err := q.HasColumn("b", "extra")
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			return q.Exec("ALTER TABLE b ADD COLUMN extra TEXT NOT NULL DEFAULT ''")
		}},
		{Name: "idx", Up: func(q Q) error {
			return q.EnsureIndex("idx_b_extra", "CREATE INDEX IF NOT EXISTS idx_b_extra ON b(extra)")
		}},
	}
	require.NoError(t, New(orm, "t_migrations").Run(steps))

	// A database that predates the ledger replays the SAME list as no-ops:
	// drop only the ledger, keep the shape, run again.
	require.NoError(t, orm.Exec("DROP TABLE t_migrations").Error)
	require.NoError(t, New(orm, "t_migrations").Run(steps))
	require.NoError(t, orm.Exec("INSERT INTO b(k) VALUES ('x')").Error)
}

func TestRunnerFailedStepUnrecorded(t *testing.T) {
	orm := openORM(t)
	boom := Step{Name: "boom", Up: func(q Q) error { return assert.AnError }}
	err := New(orm, "t_migrations").Run([]Step{boom})
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, 0, ledgerCount(t, orm), "the failed step is not recorded")

	// The same run applies cleanly once the fault is gone (replay contract).
	ok := Step{Name: "boom", Up: func(q Q) error { return nil }}
	require.NoError(t, New(orm, "t_migrations").Run([]Step{ok}))
	assert.Equal(t, 1, ledgerCount(t, orm))
}

type pinRow struct {
	K   string `gorm:"column:k;primaryKey"`
	X   string `gorm:"column:x"`
	OID string // deliberately untagged: the naming strategy resolves o_id
}

func (pinRow) TableName() string { return "pin" }

func TestAssertModelColumnsCatchesMismatch(t *testing.T) {
	orm := openORM(t)
	require.NoError(t, orm.Exec("CREATE TABLE pin(k TEXT PRIMARY KEY, x TEXT, oid TEXT)").Error)
	// The table HAS the intended column — the model's resolution is what's
	// wrong (o_id vs oid), and the guard must say so by name.
	err := AssertModelColumns(orm, pinRow{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "o_id", "the failure names the mis-resolved column")
}

func TestBaselineFreshVsLegacy(t *testing.T) {
	// FRESH: no ledger, no sentinel → the snapshot runs once, every step is
	// stamped, and the step bodies never execute.
	ormA := openORM(t)
	ran := 0
	steps := []Step{
		{Name: "s1", Up: func(q Q) error { ran++; return q.Exec("CREATE TABLE IF NOT EXISTS sentinel_t(x TEXT)") }},
		{Name: "s2", Up: func(q Q) error { ran++; return nil }},
	}
	snapshot := func(q Q) error { return q.Exec("CREATE TABLE IF NOT EXISTS sentinel_t(x TEXT)") }
	require.NoError(t, New(ormA, "t_migrations").Baseline(snapshot, "sentinel_t").Run(steps))
	assert.Equal(t, 0, ran, "fresh databases never replay step bodies")
	assert.Equal(t, 2, ledgerCount(t, ormA))
	ok, err := qOf(ormA).tableExists("sentinel_t")
	require.NoError(t, err)
	assert.True(t, ok, "the snapshot shaped the database")

	// LEGACY: the sentinel exists but no ledger (a database predating the
	// runner) → the incremental path replays, guarded steps no-op cleanly.
	ormB := openORM(t)
	require.NoError(t, ormB.Exec("CREATE TABLE sentinel_t(x TEXT)").Error)
	require.NoError(t, New(ormB, "t_migrations").Baseline(snapshot, "sentinel_t").Run(steps))
	assert.Equal(t, 2, ran, "the legacy database replayed both steps")
	assert.Equal(t, 2, ledgerCount(t, ormB))
}
