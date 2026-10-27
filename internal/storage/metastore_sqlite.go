package storage

import (
	"fmt"
	"path/filepath"
)

// sqliteDSN builds the SQLite connection string. Pragmas must ride the DSN:
// database/sql pools plain connections, and a PRAGMA issued via Exec applies
// to one pooled connection only. WAL keeps readers running while the
// single-writer CAS holds the write lock (docs/03 §2); busy_timeout bounds
// lock contention between pool connections.
func sqliteDSN(path string) string {
	return "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
}

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
