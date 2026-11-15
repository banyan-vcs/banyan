package meta

// The metadata plane's ordered migrations on the shared ddl.Runner
// (docs/storage/storage.md §2.4): every step replays cleanly — IF NOT
// EXISTS tables, existence-guarded ALTERs, probe-guarded indexes — so a
// database predating the ledger replays history as no-ops and lands on the
// new tail without baseline stamping. Fresh databases get the LATEST shape
// from step 0001; the guarded steps behind it are the historical column
// additions, kept verbatim. Append-only: never edit a shipped step.

import (
	"strconv"

	"github.com/banyan-vcs/banyan/internal/ddl"
)

// metaSteps is the migration history (ledger: meta_migrations).
var metaSteps = []ddl.Step{
	{Name: "0001-tables", Up: func(q ddl.Q) error {
		for _, stmt := range tableDDL(q.Dialect()) {
			if err := q.Exec(stmt); err != nil {
				return err
			}
		}
		return nil
	}},
	// The historical column additions (formerly migrateColumn calls): fresh
	// databases already carry these in 0001's shape; the guards make replay
	// a no-op for them and a real ALTER for pre-existing deployments.
	{Name: "0002-repos-storage-id", Up: func(q ddl.Q) error {
		if err := addColumn(q, "repos", "storage_id", varchar(q, 64)); err != nil {
			return err
		}
		if q.Dialect() != "sqlite" {
			return nil // the MVCC backends never shipped without the column
		}
		// Backfill the hashed-addressing identity for rows predating it.
		names, err := q.Names("SELECT name FROM repos WHERE storage_id=''")
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := q.Exec("UPDATE repos SET storage_id=? WHERE name=?", NewStorageID(), n); err != nil {
				return err
			}
		}
		return nil
	}},
	{Name: "0003-repos-force-push", Up: func(q ddl.Q) error {
		return addColumn(q, "repos", "force_push", "INTEGER NOT NULL DEFAULT 0")
	}},
	{Name: "0004-fence-done", Up: func(q ddl.Q) error {
		return addColumn(q, "rename_fence", "done", "INTEGER NOT NULL DEFAULT 0")
	}},
	{Name: "0005-repos-parent", Up: func(q ddl.Q) error {
		// The hierarchy link (docs/design/data-model.md §14). MySQL can't DEFAULT a TEXT
		// column, so the ALTER spells a VARCHAR like the schema.
		return addColumn(q, "repos", "parent", varchar(q, 190))
	}},
	// Query-driven indexes (see docs/storage/storage.md §2.4 for the
	// rationale, including the deliberately un-indexed small tables).
	{Name: "0006-indexes", Up: func(q ddl.Q) error {
		if err := q.EnsureIndex("idx_objects_pack",
			"CREATE INDEX idx_objects_pack ON objects(repo, pack_oid)"); err != nil {
			return err
		}
		// DeletePoolRecords / the pool side of DeleteRepo delete by pool.
		if err := q.EnsureIndex("idx_repo_pools_pool",
			"CREATE INDEX idx_repo_pools_pool ON repo_pools(pool)"); err != nil {
			return err
		}
		// ListTokens(user) filters owner and sorts by created_at.
		return q.EnsureIndex("idx_tokens_owner",
			"CREATE INDEX idx_tokens_owner ON tokens(owner, created_at)")
	}},
}

// addColumn is the guarded ALTER every column step is written against.
func addColumn(q ddl.Q, table, column, colType string) error {
	ok, err := q.HasColumn(table, column)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return q.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + colType)
}

// varchar spells a defaulted string column per dialect (MySQL cannot
// DEFAULT a TEXT column; the others take TEXT).
func varchar(q ddl.Q, size int) string {
	if q.Dialect() == "mysql" {
		return "VARCHAR(" + strconv.Itoa(size) + ") NOT NULL DEFAULT ''"
	}
	return "TEXT NOT NULL DEFAULT ''"
}

// tableDDL answers the dialect's full CURRENT table set — tables only;
// indexes live in the index step so every dialect gets the same
// existence-guarded treatment (MySQL has no CREATE INDEX IF NOT EXISTS).
func tableDDL(dialect string) []string {
	var base []string
	switch dialect {
	case "mysql":
		base = ddlMySQL()
	case "postgres":
		base = ddlPostgres()
	default:
		base = ddlSQLite()
	}
	return append(base, tokensDDL(dialect), subtreeRulesDDL(dialect))
}
