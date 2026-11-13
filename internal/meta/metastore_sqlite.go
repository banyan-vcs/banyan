package meta

import (
	"fmt"
	"path/filepath"
)

// sqliteDSN builds the SQLite connection string. Pragmas must ride the DSN:
// database/sql pools plain connections, and a PRAGMA issued via Exec applies
// to one pooled connection only. WAL keeps readers running while the
// single-writer CAS holds the write lock (docs/03 §2); busy_timeout bounds
// lock contention between pool connections. _txlock=immediate makes every
// transaction take the write lock at BEGIN: a DEFERRED read-then-write
// transaction whose snapshot is invalidated by a concurrent commit fails
// with SQLITE_BUSY IMMEDIATELY — busy_timeout does not retry that case
// (it only waits on held locks), so under write contention rename/push
// would surface spurious 500s. Immediate transactions serialize via
// busy_timeout instead.
func sqliteDSN(path string) string {
	return "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
}

// SQLiteDSN is sqliteDSN exported for services that open the SAME database
// file as the metadata plane (the web tier shares the gateway's sqlite in
// demo deployments): the pragmas — WAL, busy_timeout, immediate
// transactions — are the multi-process contract for that sharing.
func SQLiteDSN(path string) string { return sqliteDSN(path) }

// SQLiteMetaStore is the SQLMetaStore on the SQLite backend — the alias
// keeps the historical name at call sites; the implementation, schema and
// dialect handling live in metastore_sql.go.
type SQLiteMetaStore = SQLMetaStore

// NewSQLiteMetaStore opens (creating if needed) the SQLite database at path.
func NewSQLiteMetaStore(path string) (*SQLiteMetaStore, error) {
	s, err := NewSQLMetaStore("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite meta: %w", err)
	}
	return s, nil
}
