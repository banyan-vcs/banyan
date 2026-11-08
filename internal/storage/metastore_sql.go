package storage

// The SQL MetaStore, shared by every backend dialect (docs/03 §2, HA-2):
// SQLite for the single-binary default, MySQL and PostgreSQL for cluster
// deployments where the metadata plane's HA is the database ecosystem's to
// provide (v0.3 design: consensus stays in the control plane's store, packs
// are bytes). The dialect struct carries every backend divergence — DDL
// types, upsert spellings, placeholder rebinding, unique-violation
// detection, and the write-transaction discipline (SQLite takes the write
// lock upfront with BEGIN IMMEDIATE; the MVCC backends take row locks with
// SELECT ... FOR UPDATE and detect lost insert races by PK violation).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pure-Go PostgreSQL driver (database/sql)
	_ "modernc.org/sqlite"             // pure-Go SQLite driver, no cgo (OS-neutral, AGENTS.md)
)

const sqlChunk = 500 // rows per batched statement (safe under every driver's param limit)

type sqlDialect struct {
	name        string
	driver      string
	beginWrite  string // takes the connection's write transaction (SQLite: IMMEDIATE)
	rowLock     string // appended to CAS selects on MVCC backends (" FOR UPDATE")
	rebind      func(string) string
	hasColumn   func(ctx context.Context, db *sql.DB, table, column string) (bool, error)
	indexExists func(ctx context.Context, db *sql.DB, index string) (bool, error)
	isUnique    func(error) bool
}

// insertIgnore renders an insert-that-tolerates-existing-rows from the
// "(cols) VALUES(...)" tail shared by every backend.
func (d sqlDialect) insertIgnore(table, tail string) string {
	switch d.name {
	case "sqlite":
		return "INSERT OR IGNORE INTO " + table + tail
	case "mysql":
		return "INSERT IGNORE INTO " + table + tail
	default: // postgres
		return "INSERT INTO " + table + tail + " ON CONFLICT DO NOTHING"
	}
}

// objectsUpsertSuffix re-points object rows at the registering pack (repack:
// merged packs register while rows still name the old shards).
func (d sqlDialect) objectsUpsertSuffix() string {
	if d.name == "mysql" {
		return " ON DUPLICATE KEY UPDATE pack_oid=VALUES(pack_oid), node_id=VALUES(node_id)"
	}
	return " ON CONFLICT(repo, oid) DO UPDATE SET pack_oid=excluded.pack_oid, node_id=excluded.node_id"
}

// rebindPG rewrites ? placeholders to $N outside single-quoted strings.
func rebindPG(q string) string {
	var b strings.Builder
	n := 0
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			inStr = !inStr
			b.WriteByte(c)
		case c == '?' && !inStr:
			n++
			b.WriteString("$" + strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isUniqueSQLite(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isUniqueMySQL(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func isUniquePG(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func hasColumnSQL(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('"+table+"') WHERE name=?", column).Scan(&n)
	return n > 0, err
}

func indexExistsSQL(ctx context.Context, db *sql.DB, index string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", index).Scan(&n)
	return n > 0, err
}

func hasColumnMySQL(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?",
		table, column).Scan(&n)
	return n > 0, err
}

func indexExistsMySQL(ctx context.Context, db *sql.DB, index string) (bool, error) {
	// STATISTICS carries one row per indexed COLUMN (a two-column index
	// counts twice) — presence, not equality.
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND INDEX_NAME=?",
		index).Scan(&n)
	return n > 0, err
}

func hasColumnPG(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_NAME=$1 AND COLUMN_NAME=$2",
		table, column).Scan(&n)
	return n > 0, err
}

func indexExistsPG(ctx context.Context, db *sql.DB, index string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pg_indexes WHERE indexname=$1", index).Scan(&n)
	return n > 0, err
}

var (
	sqliteDialect = sqlDialect{
		name:        "sqlite",
		driver:      "sqlite",
		beginWrite:  "BEGIN IMMEDIATE",
		rebind:      func(q string) string { return q },
		hasColumn:   hasColumnSQL,
		indexExists: indexExistsSQL,
		isUnique:    isUniqueSQLite,
	}
	mysqlDialect = sqlDialect{
		name:        "mysql",
		driver:      "mysql",
		beginWrite:  "BEGIN",
		rowLock:     " FOR UPDATE",
		rebind:      func(q string) string { return q },
		hasColumn:   hasColumnMySQL,
		indexExists: indexExistsMySQL,
		isUnique:    isUniqueMySQL,
	}
	postgresDialect = sqlDialect{
		name:        "postgres",
		driver:      "pgx",
		beginWrite:  "BEGIN",
		rowLock:     " FOR UPDATE",
		rebind:      rebindPG,
		hasColumn:   hasColumnPG,
		indexExists: indexExistsPG,
		isUnique:    isUniquePG,
	}
)

// sqlSchema is the portable core; the dialect DDL builders below add types
// and options. Column set MUST stay identical across backends — the store
// code and the conformance suite treat them as one schema.
//
// MySQL notes: InnoDB index keys are byte-limited (3072 with utf8mb4), so
// indexed text columns get explicit VARCHAR sizes instead of TEXT; rows that
// exceed them fail at insert (strict mode) — repo names, anchors and ref
// names are admin/client-visible strings, not arbitrary blobs. Column names
// must dodge MySQL reserved words (why repo_blobs says blob_key, not key).

func ddlSQLite() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS repos(
	name           TEXT PRIMARY KEY,
	algo           INTEGER NOT NULL,
	default_branch TEXT NOT NULL,
	storage_id     TEXT NOT NULL DEFAULT '',
	force_push     INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS timelines(
	repo   TEXT NOT NULL,
	anchor TEXT NOT NULL,
	name   TEXT NOT NULL,
	head   TEXT NOT NULL,
	base   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(repo, anchor, name)
)`,
		`CREATE TABLE IF NOT EXISTS packs(
	repo     TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	PRIMARY KEY(repo, pack_oid)
)`,
		`CREATE TABLE IF NOT EXISTS pack_nodes(
	repo     TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	PRIMARY KEY(repo, pack_oid, node_id)
)`,
		`CREATE TABLE IF NOT EXISTS objects(
	repo   TEXT NOT NULL,
	oid    TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	offset   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(repo, oid)
) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_objects_pack ON objects(repo, pack_oid)`,
		`CREATE TABLE IF NOT EXISTS placement(
	repo     TEXT PRIMARY KEY,
	replicas INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS hooks(
	repo       TEXT NOT NULL,
	id         TEXT NOT NULL,
	dir        TEXT NOT NULL DEFAULT '',
	events     TEXT NOT NULL,
	url        TEXT NOT NULL,
	secret     TEXT NOT NULL DEFAULT '',
	timeout_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(repo, id)
) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS repo_blobs(
	repo TEXT NOT NULL,
	blob_key TEXT NOT NULL,
	data BLOB NOT NULL,
	PRIMARY KEY(repo, blob_key)
) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS repo_pools(
	member   TEXT NOT NULL PRIMARY KEY,
	pool     TEXT NOT NULL,
	upstream INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS rename_fence(
	frm TEXT NOT NULL PRIMARY KEY,
	dst TEXT NOT NULL,
	done INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID`,
	}
}

func ddlMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS repos(
	name           VARCHAR(190) NOT NULL PRIMARY KEY,
	algo           INTEGER NOT NULL,
	default_branch VARCHAR(120) NOT NULL,
	storage_id     VARCHAR(64) NOT NULL DEFAULT '',
	force_push     INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS timelines(
	repo   VARCHAR(190) NOT NULL,
	anchor VARCHAR(400) NOT NULL,
	name   VARCHAR(120) NOT NULL,
	head   VARCHAR(64) NOT NULL,
	base   VARCHAR(64) NOT NULL DEFAULT '',
	PRIMARY KEY(repo, anchor, name)
)`,
		`CREATE TABLE IF NOT EXISTS packs(
	repo     VARCHAR(190) NOT NULL,
	pack_oid VARCHAR(64) NOT NULL,
	PRIMARY KEY(repo, pack_oid)
)`,
		`CREATE TABLE IF NOT EXISTS pack_nodes(
	repo     VARCHAR(190) NOT NULL,
	pack_oid VARCHAR(64) NOT NULL,
	node_id  VARCHAR(64) NOT NULL,
	PRIMARY KEY(repo, pack_oid, node_id)
)`,
		`CREATE TABLE IF NOT EXISTS objects(
	repo     VARCHAR(190) NOT NULL,
	oid      VARCHAR(64) NOT NULL,
	pack_oid VARCHAR(64) NOT NULL,
	node_id  VARCHAR(64) NOT NULL,
	offset   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(repo, oid)
)`,
		`CREATE TABLE IF NOT EXISTS placement(
	repo     VARCHAR(190) NOT NULL PRIMARY KEY,
	replicas INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS hooks(
	repo       VARCHAR(190) NOT NULL,
	id         VARCHAR(120) NOT NULL,
	dir        VARCHAR(400) NOT NULL DEFAULT '',
	events     VARCHAR(255) NOT NULL,
	url        VARCHAR(512) NOT NULL,
	secret     VARCHAR(512) NOT NULL DEFAULT '',
	timeout_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(repo, id)
)`,
		`CREATE TABLE IF NOT EXISTS repo_blobs(
	repo VARCHAR(190) NOT NULL,
	blob_key VARCHAR(120) NOT NULL,
	data LONGBLOB NOT NULL,
	PRIMARY KEY(repo, blob_key)
)`,
		`CREATE TABLE IF NOT EXISTS repo_pools(
	member   VARCHAR(190) NOT NULL PRIMARY KEY,
	pool     VARCHAR(190) NOT NULL,
	upstream TINYINT NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS rename_fence(
	frm VARCHAR(190) NOT NULL PRIMARY KEY,
	dst VARCHAR(190) NOT NULL,
	done INTEGER NOT NULL DEFAULT 0
)`,
	}
}

func ddlPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS repos(
	name           TEXT PRIMARY KEY,
	algo           INTEGER NOT NULL,
	default_branch TEXT NOT NULL,
	storage_id     TEXT NOT NULL DEFAULT '',
	force_push     INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS timelines(
	repo   TEXT NOT NULL,
	anchor TEXT NOT NULL,
	name   TEXT NOT NULL,
	head   TEXT NOT NULL,
	base   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(repo, anchor, name)
)`,
		`CREATE TABLE IF NOT EXISTS packs(
	repo     TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	PRIMARY KEY(repo, pack_oid)
)`,
		`CREATE TABLE IF NOT EXISTS pack_nodes(
	repo     TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	PRIMARY KEY(repo, pack_oid, node_id)
)`,
		`CREATE TABLE IF NOT EXISTS objects(
	repo     TEXT NOT NULL,
	oid      TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	"offset" INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(repo, oid)
)`,
		`CREATE TABLE IF NOT EXISTS placement(
	repo     TEXT PRIMARY KEY,
	replicas INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS hooks(
	repo       TEXT NOT NULL,
	id         TEXT NOT NULL,
	dir        TEXT NOT NULL DEFAULT '',
	events     TEXT NOT NULL,
	url        TEXT NOT NULL,
	secret     TEXT NOT NULL DEFAULT '',
	timeout_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(repo, id)
)`,
		`CREATE TABLE IF NOT EXISTS repo_blobs(
	repo VARCHAR(190) NOT NULL,
	blob_key VARCHAR(120) NOT NULL,
	data BYTEA NOT NULL,
	PRIMARY KEY(repo, blob_key)
)`,
		`CREATE TABLE IF NOT EXISTS repo_pools(
	member   TEXT NOT NULL PRIMARY KEY,
	pool     TEXT NOT NULL,
	upstream INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS rename_fence(
	frm VARCHAR(190) NOT NULL PRIMARY KEY,
	dst VARCHAR(190) NOT NULL,
	done INTEGER NOT NULL DEFAULT 0
)`,
	}
}

// SQLMetaStore persists all MetaStore state in one SQL database (docs/03
// §2.3). Immutable tables (packs, objects) insert-ignore — first writer
// wins; the head CAS runs on a dedicated connection inside a write
// transaction, the linearization point for ref updates.
type SQLMetaStore struct {
	db *sql.DB
	d  sqlDialect
}

// q rebinds placeholders through the dialect.
func (s *SQLMetaStore) q(sql string) string { return s.d.rebind(sql) }

// fenceHitTx is the in-transaction fence probe: run as the first statement
// of a mutating transaction it takes a locking read (rowLock on the MVCC
// backends; SQLite serializes writers wholesale), so a fence installed after
// this probe cannot slip its flip in before COMMIT — and the flip's DELETE
// of the fence row blocks until this transaction settles.
func (s *SQLMetaStore) fenceHitTx(ctx context.Context, q queryer, names ...string) (bool, error) {
	var one int
	for _, n := range names {
		err := q.QueryRowContext(ctx, s.q("SELECT 1 FROM rename_fence WHERE (frm=? OR dst=?) AND done=0"+s.d.rowLock), n, n).Scan(&one)
		if err == nil {
			return true, nil
		}
		if err != sql.ErrNoRows {
			return false, err
		}
	}
	return false, nil
}

// queryer is the shared surface of *sql.Tx, *sql.Conn and *sql.DB for the
// fence probe.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// fenceRejectTx returns ErrRepoRenaming when a fence covers any name.
func (s *SQLMetaStore) fenceRejectTx(ctx context.Context, q queryer, names ...string) error {
	hit, err := s.fenceHitTx(ctx, q, names...)
	if err != nil {
		return err
	}
	if hit {
		if len(names) == 1 {
			return fmt.Errorf("%w: %s", ErrRepoRenaming, names[0])
		}
		return fmt.Errorf("%w: %v", ErrRepoRenaming, names)
	}
	return nil
}

// fenceReject is the standalone probe for single-statement writers (config
// rows); the check-to-commit gap is sub-millisecond, versus the fence's
// minutes-long life covering a storage move.
func (s *SQLMetaStore) fenceReject(ctx context.Context, names ...string) error {
	return s.fenceRejectTx(ctx, s.db, names...)
}

// NewSQLMetaStore opens (creating if needed) the metadata database on the
// given backend ("sqlite" | "mysql" | "postgres") and applies the schema.
// SQLite expects a file path; MySQL/PostgreSQL expect a driver DSN (the
// database itself must exist; the schema is created here).
func NewSQLMetaStore(backend, target string) (*SQLMetaStore, error) {
	var d sqlDialect
	var dsn string
	switch backend {
	case "sqlite":
		d = sqliteDialect
		dsn = sqliteDSN(target)
		if dir := filepath.Dir(target); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("sqlite meta: mkdir %s: %w", dir, err)
			}
		}
	case "mysql":
		d = mysqlDialect
		dsn = target
	case "postgres":
		d = postgresDialect
		dsn = target
	default:
		return nil, fmt.Errorf("sql meta: unknown backend %q", backend)
	}
	db, err := sql.Open(d.driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("%s meta: open: %w", d.name, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s meta: ping: %w", d.name, err)
	}
	s := &SQLMetaStore{db: db, d: d}
	if err := s.applySchema(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s meta: schema: %w", d.name, err)
	}
	return s, nil
}

func (s *SQLMetaStore) applySchema(ctx context.Context) error {
	for _, stmt := range s.ddl() {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", firstLine(stmt), err)
		}
	}
	if s.d.name == "mysql" {
		// MySQL has no CREATE INDEX IF NOT EXISTS; check, then create.
		ok, err := s.d.indexExists(ctx, s.db, "idx_objects_pack")
		if err != nil {
			return err
		}
		if !ok {
			if _, err := s.db.ExecContext(ctx, "CREATE INDEX idx_objects_pack ON objects(repo, pack_oid)"); err != nil {
				return err
			}
		}
	}
	if err := s.migrateColumn(ctx, "repos", "storage_id", "TEXT NOT NULL DEFAULT ''", "sqlite"); err != nil {
		return err
	}
	if err := s.migrateColumn(ctx, "repos", "force_push", "INTEGER NOT NULL DEFAULT 0", ""); err != nil {
		return err
	}
	return s.migrateColumn(ctx, "rename_fence", "done", "INTEGER NOT NULL DEFAULT 0", "")
}

func (s *SQLMetaStore) ddl() []string {
	var base []string
	switch s.d.name {
	case "mysql":
		base = ddlMySQL()
	case "postgres":
		base = ddlPostgres()
	default:
		base = ddlSQLite()
	}
	return append(base, s.tokensDDL(), s.subtreeRulesDDL())
}

// subtreeRulesDDL is the runtime directory-access-rule table (docs/07 §8)
// in this backend's dialect — column names avoid MySQL reserved words.
func (s *SQLMetaStore) subtreeRulesDDL() string {
	switch s.d.name {
	case "mysql":
		return `CREATE TABLE IF NOT EXISTS subtree_rules(
	repo  VARCHAR(190) NOT NULL,
	path  VARCHAR(400) NOT NULL,
	users TEXT NOT NULL,
	PRIMARY KEY(repo, path)
)`
	case "postgres":
		return `CREATE TABLE IF NOT EXISTS subtree_rules(
	repo  VARCHAR(190) NOT NULL,
	path  VARCHAR(400) NOT NULL,
	users TEXT NOT NULL,
	PRIMARY KEY(repo, path)
)`
	default:
		return `CREATE TABLE IF NOT EXISTS subtree_rules(
	repo  TEXT NOT NULL,
	path  TEXT NOT NULL,
	users TEXT NOT NULL,
	PRIMARY KEY(repo, path)
) WITHOUT ROWID`
	}
}

// tokensDDL is the personal-access-token table (docs/07 §6) in this
// backend's dialect — column names avoid MySQL reserved words.
func (s *SQLMetaStore) tokensDDL() string {
	switch s.d.name {
	case "mysql":
		return `CREATE TABLE IF NOT EXISTS tokens(
	token_hash CHAR(64) NOT NULL PRIMARY KEY,
	owner      VARCHAR(120) NOT NULL,
	name       VARCHAR(190) NOT NULL DEFAULT '',
	scope      VARCHAR(16) NOT NULL,
	expires_at BIGINT NOT NULL DEFAULT 0,
	revoked    INTEGER NOT NULL DEFAULT 0,
	created_at BIGINT NOT NULL,
	INDEX idx_tokens_owner (owner)
)`
	case "postgres":
		return `CREATE TABLE IF NOT EXISTS tokens(
	token_hash CHAR(64) PRIMARY KEY,
	owner      VARCHAR(120) NOT NULL,
	name       VARCHAR(190) NOT NULL DEFAULT '',
	scope      VARCHAR(16) NOT NULL,
	expires_at BIGINT NOT NULL DEFAULT 0,
	revoked    INTEGER NOT NULL DEFAULT 0,
	created_at BIGINT NOT NULL
)`
	default:
		return `CREATE TABLE IF NOT EXISTS tokens(
	token_hash TEXT PRIMARY KEY,
	owner      TEXT NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	scope      TEXT NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	revoked    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
) WITHOUT ROWID`
	}
}

// migrateColumn adds a column for databases predating it. The extraDSN
// argument is SQLite's PRAGMA-formatted storage_id backfill trigger (empty
// skips backfill); on the MVCC backends fresh databases carry the column
// from the schema, so this is future-proofing only.
func (s *SQLMetaStore) migrateColumn(ctx context.Context, table, column, colType, backfill string) error {
	ok, err := s.d.hasColumn(ctx, s.db, table, column)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.q("ALTER TABLE "+table+" ADD COLUMN "+column+" "+colType)); err != nil {
		return err
	}
	if backfill == "sqlite" && column == "storage_id" {
		rows, err := s.db.QueryContext(ctx, s.q("SELECT name FROM repos WHERE storage_id=''"))
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			names = append(names, n)
		}
		rows.Close()
		for _, n := range names {
			if _, err := s.db.ExecContext(ctx, s.q("UPDATE repos SET storage_id=? WHERE name=?"), NewStorageID(), n); err != nil {
				return err
			}
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func (s *SQLMetaStore) Close() error { return s.db.Close() }

func (s *SQLMetaStore) CreateRepo(ctx context.Context, repo string, algo model.HashAlgo) error {
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	// Rename tombstone (docs/03 §6b): a name retired by a rename must not
	// be resurrected — a late push or auto-create under the dead name would
	// ack into a fresh, disconnected repository (confirmed-but-invisible).
	var dst string
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT dst FROM rename_fence WHERE frm=? AND done=1"), repo).Scan(&dst)
	if err == nil {
		return fmt.Errorf("%w: %s was renamed to %s", ErrRepoRenamedAway, repo, dst)
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q(
		"INSERT INTO repos(name, algo, default_branch, storage_id) VALUES(?, ?, 'main', ?)"),
		repo, int(algo), NewStorageID())
	if s.d.isUnique(err) {
		return fmt.Errorf("repo %s already exists", repo)
	}
	return err
}

// BeginRename installs the fence row (docs/03 §6b): fail-fast checks —
// source exists, target free, no overlapping fence — run here so a doomed
// rename never blocks the repo.
func (s *SQLMetaStore) BeginRename(ctx context.Context, from, to string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, s.q("SELECT 1 FROM repos WHERE name=?"), from).Scan(&one); err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, from)
	} else if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, s.q("SELECT 1 FROM repos WHERE name=?"), to).Scan(&one); err == nil {
		return fmt.Errorf("repo %s already exists", to)
	} else if err != sql.ErrNoRows {
		return err
	}
	// Idempotent re-install of the SAME fence (rename re-run after a crash);
	// only a DIFFERENT overlapping rename is refused. Completed renames
	// (tombstones) never match: their source repo no longer exists.
	var dst string
	err = tx.QueryRowContext(ctx, s.q("SELECT dst FROM rename_fence WHERE frm=? AND done=0"+s.d.rowLock), from).Scan(&dst)
	if err == nil {
		if dst == to {
			return tx.Commit() // already fenced for this exact rename
		}
		return fmt.Errorf("%w: %s->%s", ErrRepoRenaming, from, to)
	}
	if err != sql.ErrNoRows {
		return err
	}
	if hit, err := s.fenceHitTx(ctx, tx, from, to); err != nil {
		return err
	} else if hit {
		return fmt.Errorf("%w: %s->%s", ErrRepoRenaming, from, to)
	}
	if _, err := tx.ExecContext(ctx, s.q(
		"INSERT INTO rename_fence(frm, dst) VALUES(?, ?)"), from, to); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLMetaStore) AbortRename(ctx context.Context, from, to string) error {
	// Active fence only — a completed rename's tombstone is not abortable
	// (the rename happened; AbortRename only cleans up interrupted ones).
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM rename_fence WHERE frm=? AND dst=? AND done=0"), from, to)
	return err
}

func (s *SQLMetaStore) GetRepo(ctx context.Context, repo string) (RepoInfo, error) {
	var info RepoInfo
	var algo int
	var force int
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT name, algo, default_branch, storage_id, force_push FROM repos WHERE name=?"), repo).
		Scan(&info.ID, &algo, &info.DefaultBranch, &info.StorageID, &force)
	if err == sql.ErrNoRows {
		return RepoInfo{}, fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	if err != nil {
		return RepoInfo{}, err
	}
	info.Algo = model.HashAlgo(algo)
	info.ForcePush = force != 0
	return info, nil
}

func (s *SQLMetaStore) SetForcePush(ctx context.Context, repo string, enabled bool) error {
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, s.q("UPDATE repos SET force_push=? WHERE name=?"), boolInt(enabled), repo)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	return nil
}

func (s *SQLMetaStore) SetDefaultBranch(ctx context.Context, repo string, branch string) error {
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	// MySQL reports an unchanged UPDATE as 0 affected rows, so existence is
	// probed explicitly rather than inferred from the write.
	var one int
	if err := s.db.QueryRowContext(ctx, s.q("SELECT 1 FROM repos WHERE name=?"), repo).Scan(&one); err != nil {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	_, err := s.db.ExecContext(ctx, s.q("UPDATE repos SET default_branch=? WHERE name=?"), branch, repo)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *SQLMetaStore) ListHooks(ctx context.Context, repo string) ([]HookInfo, error) {
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT id, dir, events, url, secret, timeout_ms FROM hooks WHERE repo=? ORDER BY id"), repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HookInfo
	for rows.Next() {
		var h HookInfo
		var events string
		if err := rows.Scan(&h.ID, &h.Dir, &events, &h.URL, &h.Secret, &h.TimeoutMS); err != nil {
			return nil, err
		}
		h.Events = strings.Split(events, ",")
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *SQLMetaStore) SetHook(ctx context.Context, repo string, h HookInfo) error {
	if err := h.validate(); err != nil {
		return err
	}
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM repos WHERE name=?"), repo).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	tail := "(repo, id, dir, events, url, secret, timeout_ms) VALUES(?,?,?,?,?,?,?)"
	var stmt string
	switch s.d.name {
	case "sqlite":
		stmt = "INSERT OR REPLACE INTO hooks" + tail
	case "mysql":
		stmt = "REPLACE INTO hooks" + tail
	default:
		stmt = "INSERT INTO hooks" + tail +
			" ON CONFLICT(repo, id) DO UPDATE SET dir=excluded.dir, events=excluded.events, url=excluded.url, secret=excluded.secret, timeout_ms=excluded.timeout_ms"
	}
	_, err := s.db.ExecContext(ctx, s.q(stmt),
		repo, h.ID, h.Dir, strings.Join(h.Events, ","), h.URL, h.Secret, h.TimeoutMS)
	return err
}

func (s *SQLMetaStore) DeleteHook(ctx context.Context, repo, id string) error {
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM hooks WHERE repo=? AND id=?"), repo, id)
	return err
}

func (s *SQLMetaStore) SetRepoBlob(ctx context.Context, repo, key string, data []byte) error {
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	suffix := " ON CONFLICT(repo, blob_key) DO UPDATE SET data=excluded.data"
	if s.d.name == "mysql" {
		suffix = " ON DUPLICATE KEY UPDATE data=VALUES(data)"
	}
	_, err := s.db.ExecContext(ctx,
		s.q("INSERT INTO repo_blobs(repo, blob_key, data) VALUES(?,?,?)"+suffix), repo, key, data)
	return err
}

func (s *SQLMetaStore) GetRepoBlob(ctx context.Context, repo, key string) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx,
		s.q("SELECT data FROM repo_blobs WHERE repo=? AND blob_key=?"), repo, key).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (s *SQLMetaStore) LinkPool(ctx context.Context, member, pool string, upstream bool) error {
	if err := s.fenceReject(ctx, member, pool); err != nil {
		return err
	}
	suffix := " ON CONFLICT(member) DO UPDATE SET pool=excluded.pool, upstream=excluded.upstream"
	if s.d.name == "mysql" {
		suffix = " ON DUPLICATE KEY UPDATE pool=VALUES(pool), upstream=VALUES(upstream)"
	}
	_, err := s.db.ExecContext(ctx,
		s.q("INSERT INTO repo_pools(member, pool, upstream) VALUES(?,?,?)"+suffix),
		member, pool, boolInt(upstream))
	return err
}

func (s *SQLMetaStore) UnlinkPool(ctx context.Context, member string) error {
	if err := s.fenceReject(ctx, member); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM repo_pools WHERE member=?"), member)
	return err
}

func (s *SQLMetaStore) PoolOf(ctx context.Context, member string) (string, bool, error) {
	var pool string
	err := s.db.QueryRowContext(ctx,
		s.q("SELECT pool FROM repo_pools WHERE member=?"), member).Scan(&pool)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return pool, true, nil
}

func (s *SQLMetaStore) ListPoolMembers(ctx context.Context) ([]PoolInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		s.q("SELECT member, pool, upstream FROM repo_pools ORDER BY member"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PoolInfo
	for rows.Next() {
		var p PoolInfo
		var upstream int
		if err := rows.Scan(&p.Member, &p.Pool, &upstream); err != nil {
			return nil, err
		}
		p.Upstream = upstream != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLMetaStore) DeletePoolRecords(ctx context.Context, pool string) error {
	if err := s.fenceReject(ctx, pool); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM repo_pools WHERE pool=?"), pool)
	return err
}

func (s *SQLMetaStore) ListRepos(ctx context.Context) ([]RepoInfo, error) {
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT name, algo, default_branch, storage_id, force_push FROM repos ORDER BY name"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoInfo
	for rows.Next() {
		var info RepoInfo
		var algo int
		var force int
		if err := rows.Scan(&info.ID, &algo, &info.DefaultBranch, &info.StorageID, &force); err != nil {
			return nil, err
		}
		info.Algo = model.HashAlgo(algo)
		info.ForcePush = force != 0
		out = append(out, info)
	}
	return out, rows.Err()
}

// RenameRepo moves every repo-keyed row in one transaction (docs/03 §6).
// The storage_id column rides along untouched — hashed addressing keys stay
// stable across renames by construction.
func (s *SQLMetaStore) RenameRepo(ctx context.Context, from, to string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, s.q("SELECT 1 FROM repos WHERE name=?"), from).Scan(&one); err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, from)
	} else if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, s.q("SELECT 1 FROM repos WHERE name=?"), to).Scan(&one); err == nil {
		return fmt.Errorf("repo %s already exists", to)
	} else if err != sql.ErrNoRows {
		return err
	}
	for _, table := range []string{"timelines", "packs", "pack_nodes", "objects", "placement", "hooks", "repo_blobs", "subtree_rules"} {
		if _, err := tx.ExecContext(ctx, s.q("UPDATE "+table+" SET repo=? WHERE repo=?"), to, from); err != nil {
			return err
		}
	}
	// repo_pools keys by member, not repo: rename the membership side; the
	// pool side keeps pointing at the (unchanged) pool repository.
	if _, err := tx.ExecContext(ctx, s.q("UPDATE repo_pools SET member=? WHERE member=?"), to, from); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q("UPDATE repos SET name=? WHERE name=?"), to, from); err != nil {
		return err
	}
	// The flip retires the fence into a tombstone inside the same
	// transaction: mutations under `to` are legal the moment the rename
	// commits, while re-creating `from` stays refused (a late push under the
	// dead name must not ack into a disconnected fresh repo). Tombstones
	// pointing at the old name retarget to the new one — the hazard chain
	// follows the data.
	if _, err := tx.ExecContext(ctx, s.q(
		"UPDATE rename_fence SET done=1 WHERE frm=?"), from); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q(
		"UPDATE rename_fence SET dst=? WHERE dst=? AND done=1"), to, from); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteRepo drops every repo-keyed row in one transaction. Missing = no-op
// (idempotent). Storage bytes are reclaimed via ObjectService.DeleteRepo.
func (s *SQLMetaStore) DeleteRepo(ctx context.Context, repo string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.fenceRejectTx(ctx, tx, repo); err != nil {
		return err
	}
	for _, table := range []string{"objects", "pack_nodes", "packs", "timelines", "placement", "hooks", "repo_blobs", "subtree_rules"} {
		if _, err := tx.ExecContext(ctx, s.q("DELETE FROM "+table+" WHERE repo=?"), repo); err != nil {
			return err
		}
	}
	// The repo leaves its pool (member row) and, when it was a pool, every
	// membership record pointing at it goes too.
	if _, err := tx.ExecContext(ctx, s.q("DELETE FROM repo_pools WHERE member=? OR pool=?"), repo, repo); err != nil {
		return err
	}
	// Deletion retires the name for real: tombstones blocking older names
	// (retired into this repo) lift — the recycled name is creatable again.
	if _, err := tx.ExecContext(ctx, s.q("DELETE FROM rename_fence WHERE frm=? OR dst=?"), repo, repo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q("DELETE FROM repos WHERE name=?"), repo); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLMetaStore) PutPack(ctx context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error {
	if len(nodes) == 0 {
		return fmt.Errorf("putpack %s/%s: no nodes", repo, packOID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.fenceRejectTx(ctx, tx, repo); err != nil {
		return err
	}

	var one string
	err = tx.QueryRowContext(ctx, s.q("SELECT 1 FROM repos WHERE name=?"), repo).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q(s.d.insertIgnore("packs", "(repo, pack_oid) VALUES(?, ?)")),
		repo, string(packOID)); err != nil {
		return err
	}
	for _, node := range nodes {
		if _, err := tx.ExecContext(ctx, s.q(s.d.insertIgnore("pack_nodes", "(repo, pack_oid, node_id) VALUES(?, ?, ?)")),
			repo, string(packOID), node); err != nil {
			return err
		}
	}
	primary := nodes[0]
	for start := 0; start < len(objects); start += sqlChunk {
		end := start + sqlChunk
		if end > len(objects) {
			end = len(objects)
		}
		var b strings.Builder
		// UPSERT (not insert-ignore): repack registers a merged pack while
		// object rows still point at the old shards — they must re-point to
		// the newly registered pack, or the subsequent DropPack deletes them.
		b.WriteString("INSERT INTO objects(repo, oid, pack_oid, node_id) VALUES")
		args := make([]any, 0, (end-start)*4)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteString("(?, ?, ?, ?)")
			args = append(args, repo, string(objects[i]), string(packOID), primary)
		}
		if _, err := tx.ExecContext(ctx, s.q(b.String()+s.d.objectsUpsertSuffix()), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLMetaStore) GetPack(ctx context.Context, repo string, packOID model.OID) (PackInfo, error) {
	var one string
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT 1 FROM packs WHERE repo=? AND pack_oid=?"), repo, string(packOID)).Scan(&one)
	if err == sql.ErrNoRows {
		return PackInfo{}, fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	if err != nil {
		return PackInfo{}, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT node_id FROM pack_nodes WHERE repo=? AND pack_oid=? ORDER BY node_id"), repo, string(packOID))
	if err != nil {
		return PackInfo{}, err
	}
	defer rows.Close()
	var nodes []string
	for rows.Next() {
		var node string
		if err := rows.Scan(&node); err != nil {
			return PackInfo{}, err
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return PackInfo{}, err
	}
	return PackInfo{PackOID: packOID, Nodes: nodes}, nil
}

func (s *SQLMetaStore) ListPacks(ctx context.Context, repo string) ([]PackInfo, error) {
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT p.pack_oid, n.node_id FROM packs p JOIN pack_nodes n ON p.repo=n.repo AND p.pack_oid=n.pack_oid WHERE p.repo=?"),
		repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byOID := map[model.OID][]string{}
	for rows.Next() {
		var packOID, node string
		if err := rows.Scan(&packOID, &node); err != nil {
			return nil, err
		}
		byOID[model.OID(packOID)] = append(byOID[model.OID(packOID)], node)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]PackInfo, 0, len(byOID))
	for packOID, nodes := range byOID {
		out = append(out, PackInfo{PackOID: packOID, Nodes: nodes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PackOID < out[j].PackOID })
	return out, nil
}

func (s *SQLMetaStore) PackObjects(ctx context.Context, repo string, packOID model.OID) ([]model.OID, error) {
	var one string
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT 1 FROM packs WHERE repo=? AND pack_oid=?"), repo, string(packOID)).Scan(&one)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT oid FROM objects WHERE repo=? AND pack_oid=?"), repo, string(packOID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.OID
	for rows.Next() {
		var oid string
		if err := rows.Scan(&oid); err != nil {
			return nil, err
		}
		out = append(out, model.OID(oid))
	}
	return out, rows.Err()
}

// beginWriteTx opens the write transaction on a dedicated connection (the
// store's linearization discipline) and returns a commit/rollback pair.
func (s *SQLMetaStore) beginWriteTx(ctx context.Context) (*sql.Conn, func(error) error, func() error, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, s.d.beginWrite); err != nil {
		conn.Close()
		return nil, nil, nil, err
	}
	fail := func(err error) error {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), s.d.rebind("ROLLBACK"))
		return err
	}
	done := func() error {
		_, err := conn.ExecContext(ctx, s.d.rebind("COMMIT"))
		return err
	}
	return conn, fail, done, nil
}

// DropPack unregisters the pack and its object locations in one transaction.
// Missing-object rows are tolerated (they may have been re-registered to the
// replacement pack already); a missing pack row is ErrPackNotFound.
func (s *SQLMetaStore) DropPack(ctx context.Context, repo string, packOID model.OID) error {
	var one string
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT 1 FROM packs WHERE repo=? AND pack_oid=?"), repo, string(packOID)).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	if err != nil {
		return err
	}
	conn, fail, done, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := s.fenceRejectTx(ctx, conn, repo); err != nil {
		return fail(err)
	}
	// Object rows pointing at this pack are deleted only when they still do:
	// the replacement pack's registration may have already re-pointed them.
	if _, err := conn.ExecContext(ctx, s.q(
		"DELETE FROM objects WHERE repo=? AND pack_oid=?"), repo, string(packOID)); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx, s.q(
		"DELETE FROM pack_nodes WHERE repo=? AND pack_oid=?"), repo, string(packOID)); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx, s.q(
		"DELETE FROM packs WHERE repo=? AND pack_oid=?"), repo, string(packOID)); err != nil {
		return fail(err)
	}
	return done()
}

// SetPackNodes atomically replaces the holder set (reconciler swap, docs/10 §8.3).
func (s *SQLMetaStore) SetPackNodes(ctx context.Context, repo string, packOID model.OID, nodes []string) error {
	var one string
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT 1 FROM packs WHERE repo=? AND pack_oid=?"), repo, string(packOID)).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	if err != nil {
		return err
	}
	conn, fail, done, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := s.fenceRejectTx(ctx, conn, repo); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx, s.q(
		"DELETE FROM pack_nodes WHERE repo=? AND pack_oid=?"), repo, string(packOID)); err != nil {
		return fail(err)
	}
	for _, n := range nodes {
		if _, err := conn.ExecContext(ctx, s.q(
			"INSERT INTO pack_nodes(repo, pack_oid, node_id) VALUES(?,?,?)"), repo, string(packOID), n); err != nil {
			return fail(err)
		}
	}
	if len(nodes) > 0 {
		if _, err := conn.ExecContext(ctx, s.q(
			"UPDATE objects SET node_id=? WHERE repo=? AND pack_oid=?"), nodes[0], repo, string(packOID)); err != nil {
			return fail(err)
		}
	}
	return done()
}

func (s *SQLMetaStore) SetPlacement(ctx context.Context, repo string, replicas int) error {
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	if _, err := s.GetRepo(ctx, repo); err != nil {
		return err
	}
	stmt := "INSERT INTO placement(repo, replicas) VALUES(?,?) ON CONFLICT(repo) DO UPDATE SET replicas=excluded.replicas"
	if s.d.name == "mysql" {
		stmt = "INSERT INTO placement(repo, replicas) VALUES(?,?) ON DUPLICATE KEY UPDATE replicas=VALUES(replicas)"
	}
	_, err := s.db.ExecContext(ctx, s.q(stmt), repo, replicas)
	return err
}

func (s *SQLMetaStore) GetPlacement(ctx context.Context, repo string) (int, error) {
	if _, err := s.GetRepo(ctx, repo); err != nil {
		return 0, err
	}
	var replicas int
	err := s.db.QueryRowContext(ctx, s.q("SELECT replicas FROM placement WHERE repo=?"), repo).Scan(&replicas)
	if err == sql.ErrNoRows {
		return 0, nil // no row = cluster default
	}
	if err != nil {
		return 0, err
	}
	return replicas, nil
}

func (s *SQLMetaStore) LocateObjects(ctx context.Context, repo string, oids []model.OID) (map[model.OID]ObjectLocation, error) {
	out := map[model.OID]ObjectLocation{}
	keys := make([]model.OID, 0, len(oids))
	seen := map[model.OID]bool{}
	for _, oid := range oids {
		if !seen[oid] {
			seen[oid] = true
			keys = append(keys, oid)
		}
	}
	for start := 0; start < len(keys); start += sqlChunk {
		end := start + sqlChunk
		if end > len(keys) {
			end = len(keys)
		}
		var b strings.Builder
		b.WriteString("SELECT oid, pack_oid, node_id FROM objects WHERE repo=? AND oid IN(")
		args := make([]any, 0, end-start+1)
		args = append(args, repo)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteByte('?')
			args = append(args, string(keys[i]))
		}
		b.WriteByte(')')
		rows, err := s.db.QueryContext(ctx, s.q(b.String()), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var oid, packOID, node string
			if err := rows.Scan(&oid, &packOID, &node); err != nil {
				rows.Close()
				return nil, err
			}
			out[model.OID(oid)] = ObjectLocation{PackOID: model.OID(packOID), NodeID: node}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *SQLMetaStore) Timeline(ctx context.Context, tl model.Timeline) (model.TimelineState, error) {
	var st model.TimelineState
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT head, base FROM timelines WHERE repo=? AND anchor=? AND name=?"),
		tl.Repo, string(tl.Anchor), tl.Name).Scan(&st.Head, &st.Base)
	if err == sql.ErrNoRows {
		return model.TimelineState{}, ErrTimelineUnknown
	}
	if err != nil {
		return model.TimelineState{}, err
	}
	return st, nil
}

// CASHead runs the compare-and-set inside one write transaction on a
// dedicated connection — the linearization point for ref updates. SQLite:
// BEGIN IMMEDIATE takes the write lock before the read; MySQL/PostgreSQL:
// the row is locked with SELECT ... FOR UPDATE, and a lost insert race on a
// timeline that did not exist surfaces as the PK unique violation it is.
func (s *SQLMetaStore) CASHead(ctx context.Context, tl model.Timeline, expected, next model.OID) error {
	expected = expected.NormalizeZero()
	next = next.NormalizeZero()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, s.d.beginWrite); err != nil {
		return err
	}
	fail := func(err error) error {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), s.d.rebind("ROLLBACK"))
		return err
	}
	// Fence probe inside the write transaction: a rename whose fence went in
	// after this CAS began cannot slip its flip in before COMMIT.
	if err := s.fenceRejectTx(ctx, conn, tl.Repo); err != nil {
		return fail(err)
	}
	var head, base string
	err = conn.QueryRowContext(ctx, s.q(
		"SELECT head, base FROM timelines WHERE repo=? AND anchor=? AND name=?"+s.d.rowLock),
		tl.Repo, string(tl.Anchor), tl.Name).Scan(&head, &base)
	if err == sql.ErrNoRows {
		if expected != "" {
			mCASConflicts.With().Inc()
			return fail(ErrNonFastForward) // creating requires expected=zero
		}
		if next == "" {
			return fail(nil) // delete of a missing timeline is a no-op
		}
		_, err = conn.ExecContext(ctx, s.q(
			"INSERT INTO timelines(repo, anchor, name, head, base) VALUES(?, ?, ?, ?, '')"),
			tl.Repo, string(tl.Anchor), tl.Name, string(next))
		if err != nil {
			if s.d.isUnique(err) {
				mCASConflicts.With().Inc()
				return fail(ErrNonFastForward) // another writer created it first
			}
			return fail(err)
		}
		_, err = conn.ExecContext(ctx, s.d.rebind("COMMIT"))
		return err
	}
	if err != nil {
		return fail(err)
	}
	if model.OID(head).NormalizeZero() != expected {
		mCASConflicts.With().Inc()
		return fail(ErrNonFastForward)
	}
	if next == "" {
		// ref deletion: zeroing the head removes the timeline entry.
		if _, err := conn.ExecContext(ctx, s.q(
			"DELETE FROM timelines WHERE repo=? AND anchor=? AND name=?"),
			tl.Repo, string(tl.Anchor), tl.Name); err != nil {
			return fail(err)
		}
	} else if _, err := conn.ExecContext(ctx, s.q(
		"UPDATE timelines SET head=? WHERE repo=? AND anchor=? AND name=?"),
		string(next), tl.Repo, string(tl.Anchor), tl.Name); err != nil {
		return fail(err)
	}
	_, err = conn.ExecContext(ctx, s.d.rebind("COMMIT"))
	return err
}

func (s *SQLMetaStore) EnsureTimeline(ctx context.Context, tl model.Timeline, head, base model.OID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.fenceRejectTx(ctx, tx, tl.Repo); err != nil {
		return err
	}
	var one string
	err = tx.QueryRowContext(ctx, s.q(
		"SELECT 1 FROM timelines WHERE repo=? AND anchor=? AND name=?"),
		tl.Repo, string(tl.Anchor), tl.Name).Scan(&one)
	if err == nil {
		return nil // already exists (incl. locally committed): don't overwrite
	}
	if err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q(
		"INSERT INTO timelines(repo, anchor, name, head, base) VALUES(?, ?, ?, ?, ?)"),
		tl.Repo, string(tl.Anchor), tl.Name, string(head), string(base)); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTimelineBase advances an existing timeline's fork point (explicit-rebase
// bookkeeping, docs/02 §12). RowsAffected distinguishes the unknown-timeline
// rejection across all three dialects.
func (s *SQLMetaStore) SetTimelineBase(ctx context.Context, tl model.Timeline, base model.OID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.fenceRejectTx(ctx, tx, tl.Repo); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.q(
		"UPDATE timelines SET base=? WHERE repo=? AND anchor=? AND name=?"),
		string(base), tl.Repo, string(tl.Anchor), tl.Name)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTimelineUnknown
	}
	return tx.Commit()
}

func (s *SQLMetaStore) ListTimelines(ctx context.Context, repo string, anchor model.AnchorPath) ([]model.Timeline, error) {
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT anchor, name FROM timelines WHERE repo=? AND anchor=? ORDER BY name"),
		repo, string(anchor))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Timeline
	for rows.Next() {
		var anchorStr, name string
		if err := rows.Scan(&anchorStr, &name); err != nil {
			return nil, err
		}
		out = append(out, model.Timeline{Repo: repo, Anchor: model.AnchorPath(anchorStr), Name: name})
	}
	return out, rows.Err()
}

func (s *SQLMetaStore) ListAllTimelines(ctx context.Context, repo string) ([]model.Timeline, error) {
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT anchor, name FROM timelines WHERE repo=? ORDER BY anchor, name"), repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Timeline
	for rows.Next() {
		var anchor, name string
		if err := rows.Scan(&anchor, &name); err != nil {
			return nil, err
		}
		out = append(out, model.Timeline{Repo: repo, Anchor: model.AnchorPath(anchor), Name: name})
	}
	return out, rows.Err()
}

// Backup writes a consistent snapshot of a SQLite metadata database to path
// (VACUUM INTO — online, readers and the writer keep running). MySQL and
// PostgreSQL deployments take their backups with the database ecosystem's
// own tooling — that HA is theirs to provide (docs/10).
func (s *SQLMetaStore) Backup(path string) error {
	if s.d.name != "sqlite" {
		return fmt.Errorf("meta backup: backend %s — use the database's own backup tooling", s.d.name)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("meta backup: %s already exists", path)
	}
	_, err := s.db.Exec("VACUUM INTO ?", path)
	return err
}
