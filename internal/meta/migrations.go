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

// metaTablesUp is step 0001 (and the table half of the baseline snapshot).
func metaTablesUp(q ddl.Q) error {
	for _, stmt := range tableDDL(q.Dialect()) {
		if err := q.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// metaIndexesUp is step 0006 (and the index half of the baseline snapshot).
func metaIndexesUp(q ddl.Q) error {
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
	if err := q.EnsureIndex("idx_tokens_owner",
		"CREATE INDEX idx_tokens_owner ON tokens(owner, created_at)"); err != nil {
		return err
	}
	// ListMRs(repo, state) filters and orders by IID.
	return q.EnsureIndex("idx_merge_requests_state",
		"CREATE INDEX idx_merge_requests_state ON merge_requests(repo, state, iid)")
}

// metaBaseline is the authoritative full shape for FRESH databases — tables
// AND indexes, stamped with the whole history, never replayed. It shares
// the step closures so the two tracks cannot drift (pinned by
// TestBaselineEqualsIncremental).
func metaBaseline(q ddl.Q) error {
	if err := metaTablesUp(q); err != nil {
		return err
	}
	return metaIndexesUp(q)
}

// metaSteps is the migration history (ledger: meta_migrations).
var metaSteps = []ddl.Step{
	{Name: "0001-tables", Up: metaTablesUp},
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
	{Name: "0006-indexes", Up: metaIndexesUp},
	// The merge-request tables (docs/design/merge-requests.md). Fresh
	// databases already carry them in 0001's living tableDDL; this step
	// lands them on pre-MR deployments (IF NOT EXISTS makes replay a no-op).
	{Name: "0007-mr-tables", Up: mrTablesUp},
	// The group tables (docs/design/data-model.md §15) — same baseline/
	// incremental split as the MR tables.
	{Name: "0008-group-tables", Up: groupTablesUp},
	// The repository display one-liner (the list page shows it truncated).
	{Name: "0009-repos-description", Up: func(q ddl.Q) error {
		if q.Dialect() == "mysql" {
			return addColumn(q, "repos", "description", "VARCHAR(500) NOT NULL DEFAULT ''")
		}
		return addColumn(q, "repos", "description", "TEXT NOT NULL DEFAULT ''")
	}},
}

// groupTablesUp is step 0008: the groups and repo_groups tables.
func groupTablesUp(q ddl.Q) error {
	for _, stmt := range groupsDDL(q.Dialect()) {
		if err := q.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// mrTablesUp is step 0007: the three MR tables plus the ListMRs index
// (existence-guarded — metaIndexesUp carries the same index for the
// baseline track, pinned equal by TestBaselineEqualsIncremental).
func mrTablesUp(q ddl.Q) error {
	for _, stmt := range mrsDDL(q.Dialect()) {
		if err := q.Exec(stmt); err != nil {
			return err
		}
	}
	return q.EnsureIndex("idx_merge_requests_state",
		"CREATE INDEX idx_merge_requests_state ON merge_requests(repo, state, iid)")
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
	return append(append(append(base, tokensDDL(dialect), subtreeRulesDDL(dialect)), mrsDDL(dialect)...), groupsDDL(dialect)...)
}
