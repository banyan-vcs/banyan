// Package ddl is the ordered-migration runner the three SQL-backed stores
// share (metadata plane, web tier, console). Mechanism, not policy: each
// store declares its replayable step list; the runner keeps a per-store
// ledger table and applies the pending tail at open.
//
// Posture (docs/storage/storage.md §2.3): steps are append-only history and
// every step must be REPLAYABLE — CREATE ... IF NOT EXISTS, guarded ALTERs,
// existence-checked indexes. A database that predates the ledger replays
// the whole list as no-ops before reaching the new tail (no baseline
// stamping); a failed step stays unrecorded and is retried on the next
// boot, which is safe exactly because steps replay cleanly. SQLite and
// PostgreSQL run DDL inside the step's transaction; MySQL DDL auto-commits,
// so a mid-step failure there can leave a partial step — replayability, not
// the transaction, is the recovery contract.
package ddl

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Q is the surface a step sees: the dialect name, statement execution, and
// the schema probes guarded steps are written against.
type Q interface {
	Dialect() string // "sqlite" | "mysql" | "postgres"
	Exec(stmt string, args ...any) error
	HasColumn(table, column string) (bool, error)
	IndexExists(name string) (bool, error)
	// Names answers a single-column string query — the row lists data
	// backfills iterate (e.g. the rows missing a column to fill).
	Names(stmt string, args ...any) ([]string, error)
	// EnsureIndex creates the named index when missing (MySQL has no CREATE
	// INDEX IF NOT EXISTS; the existence probe guards every dialect).
	EnsureIndex(name, stmt string) error
}

// Step is one migration. Name is the ledger key (stable once shipped —
// never rename a shipped step).
type Step struct {
	Name string
	Up   func(q Q) error
}

// Runner applies a step list, recording progress in the ledger table
// (per-store named — the web tier shares a sqlite file with the metadata
// plane, so ledgers must not collide).
type Runner struct {
	orm      *gorm.DB
	ledger   string
	snapshot func(q Q) error // full-schema snapshot for FRESH databases
	sentinel string          // table whose absence marks a database fresh
}

func New(orm *gorm.DB, ledger string) *Runner { return &Runner{orm: orm, ledger: ledger} }

// Baseline installs the authoritative full-schema snapshot for FRESH
// databases — the LATEST.sql posture: the sentinel table missing AND no
// ledger means the database was never shaped, so it takes the snapshot in
// one shot and stamps every step applied; history is never replayed. A
// database with the sentinel but no ledger is LEGACY (predates the
// runner): it keeps the incremental path — the snapshot's IF NOT EXISTS
// cannot ALTER old shapes into the current one.
func (r *Runner) Baseline(snapshot func(q Q) error, sentinel string) *Runner {
	r.snapshot, r.sentinel = snapshot, sentinel
	return r
}

// Run creates the ledger if needed and applies every unrecorded step in
// order. Concurrent boots converge benignly: steps replay, and ledger
// inserts are ignore-duplicate (spelled per dialect — GORM's OnConflict
// DoNothing renders a broken empty ON DUPLICATE KEY UPDATE on MySQL).
func (r *Runner) Run(steps []Step) error {
	if r.snapshot != nil {
		ledgerThere, err := qOf(r.orm).tableExists(r.ledger)
		if err != nil {
			return err
		}
		if !ledgerThere {
			sentinelThere, err := qOf(r.orm).tableExists(r.sentinel)
			if err != nil {
				return err
			}
			if !sentinelThere {
				return r.runBaseline(steps)
			}
		}
	}
	if err := r.orm.Exec(
		"CREATE TABLE IF NOT EXISTS " + r.ledger + " (name VARCHAR(190) PRIMARY KEY, applied_at BIGINT NOT NULL DEFAULT 0)",
	).Error; err != nil {
		return fmt.Errorf("ddl: ledger %s: %w", r.ledger, err)
	}
	applied, err := r.applied()
	if err != nil {
		return err
	}
	for _, s := range steps {
		if applied[s.Name] {
			continue
		}
		err := r.orm.Transaction(func(tx *gorm.DB) error {
			if err := s.Up(qOf(tx)); err != nil {
				return fmt.Errorf("ddl: step %s: %w", s.Name, err)
			}
			return tx.Exec(r.insertIgnoreStmt(), s.Name, time.Now().Unix()).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// runBaseline shapes a fresh database from the snapshot and stamps the
// whole history in one transaction.
func (r *Runner) runBaseline(steps []Step) error {
	if err := r.orm.Exec(
		"CREATE TABLE IF NOT EXISTS " + r.ledger + " (name VARCHAR(190) PRIMARY KEY, applied_at BIGINT NOT NULL DEFAULT 0)",
	).Error; err != nil {
		return fmt.Errorf("ddl: ledger %s: %w", r.ledger, err)
	}
	return r.orm.Transaction(func(tx *gorm.DB) error {
		if err := r.snapshot(qOf(tx)); err != nil {
			return fmt.Errorf("ddl: baseline snapshot: %w", err)
		}
		for _, s := range steps {
			if err := tx.Exec(r.insertIgnoreStmt(), s.Name, time.Now().Unix()).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// insertIgnoreStmt is the dialect-spelled ledger insert.
func (r *Runner) insertIgnoreStmt() string {
	stmt := "INSERT INTO " + r.ledger + " (name, applied_at) VALUES (?, ?)"
	switch r.orm.Dialector.Name() {
	case "mysql":
		return "INSERT IGNORE" + stmt[len("INSERT"):]
	case "sqlite":
		return "INSERT OR IGNORE" + stmt[len("INSERT"):]
	default: // postgres
		return stmt + " ON CONFLICT DO NOTHING"
	}
}

func (r *Runner) applied() (map[string]bool, error) {
	rows, err := r.orm.Raw("SELECT name FROM " + r.ledger).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// --- the Q implementation ---

type q struct {
	tx      *gorm.DB
	dialect string
}

// qOf returns the concrete probe (it implements Q and carries the
// unexported tableExists for the runner's fresh/legacy discriminator).
func qOf(tx *gorm.DB) q { return q{tx: tx, dialect: tx.Dialector.Name()} }

func (q q) Dialect() string { return q.dialect }

func (q q) Exec(stmt string, args ...any) error { return q.tx.Exec(stmt, args...).Error }

// HasColumn reports whether table already carries column — the guard that
// makes historical ALTER steps replayable. Placeholders are rebound per
// dialect by the ORM.
func (q q) HasColumn(table, column string) (bool, error) {
	var n int
	var stmt string
	var args []any
	switch q.dialect {
	case "sqlite": // one placeholder: the column name (table is interpolated)
		stmt = "SELECT COUNT(*) FROM pragma_table_info('" + table + "') WHERE name=?"
		args = []any{column}
	case "mysql":
		stmt = "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?"
		args = []any{table, column}
	default: // postgres
		stmt = "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_NAME=? AND COLUMN_NAME=?"
		args = []any{table, column}
	}
	if err := q.tx.Raw(stmt, args...).Row().Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return n > 0, nil
}

// IndexExists reports whether the index name is present.
func (q q) IndexExists(name string) (bool, error) {
	var n int
	var stmt string
	switch q.dialect {
	case "sqlite":
		stmt = "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?"
	case "mysql":
		// STATISTICS carries one row per indexed COLUMN — presence, not equality.
		stmt = "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND INDEX_NAME=?"
	default: // postgres
		stmt = "SELECT COUNT(*) FROM pg_indexes WHERE indexname=?"
	}
	if err := q.tx.Raw(stmt, name).Row().Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return n > 0, nil
}

// EnsureIndex creates the index when missing — MySQL has no CREATE INDEX
// IF NOT EXISTS, so the guard is the existence probe on every dialect (the
// IF NOT EXISTS clause in stmt stays harmless where supported).
func (q q) EnsureIndex(name, stmt string) error {
	ok, err := q.IndexExists(name)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return q.Exec(stmt)
}

// Names runs a single-column string query.
func (q q) Names(stmt string, args ...any) ([]string, error) {
	rows, err := q.tx.Raw(stmt, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// tableExists is the fresh/legacy discriminator for the baseline path.
func (q q) tableExists(name string) (bool, error) {
	var n int
	var stmt string
	switch q.dialect {
	case "sqlite":
		stmt = "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?"
	case "mysql":
		stmt = "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?"
	default: // postgres
		stmt = "SELECT COUNT(*) FROM information_schema.tables WHERE table_name=?"
	}
	if err := q.tx.Raw(stmt, name).Row().Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return n > 0, nil
}

// AssertModelColumns pins every row model's RESOLVED column names to the
// live schema: GORM's naming strategy is an implicit convention (the
// registered trap: OID snake-cases to "o_id", not "oid"), and a mismatch
// surfaces only at query time. This turns it into an open-time test
// failure: each field's dialect-resolved DBName must exist as a column of
// the model's table. Every store runs it over its models.
func AssertModelColumns(orm *gorm.DB, models ...any) error {
	for _, m := range models {
		sch, err := schema.Parse(m, &sync.Map{}, orm.Config.NamingStrategy)
		if err != nil {
			return fmt.Errorf("ddl: parse %T: %w", m, err)
		}
		probe := qOf(orm.Session(&gorm.Session{NewDB: true}))
		for _, f := range sch.Fields {
			if f.DBName == "" {
				continue // non-column fields (ignored, associations)
			}
			ok, err := probe.HasColumn(sch.Table, f.DBName)
			if err != nil {
				return fmt.Errorf("ddl: probe %s.%s: %w", sch.Table, f.DBName, err)
			}
			if !ok {
				return fmt.Errorf("ddl: model %T field %s resolves to %s.%s — no such column (naming-strategy mismatch? explicit gorm:\"column:...\" tag fixes it)", m, f.Name, sch.Table, f.DBName)
			}
		}
	}
	return nil
}
