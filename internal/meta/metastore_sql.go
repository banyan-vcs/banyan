package meta

// The SQL MetaStore, shared by every backend dialect (docs/storage/storage.md §2, HA-2):
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

	_ "github.com/glebarez/go-sqlite" // pure-Go SQLite driver (modernc engine), no cgo
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pure-Go PostgreSQL driver (database/sql)
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	force_push     INTEGER NOT NULL DEFAULT 0,
	parent         TEXT NOT NULL DEFAULT ''
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
	force_push     INTEGER NOT NULL DEFAULT 0,
	parent         VARCHAR(190) NOT NULL DEFAULT ''
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
	force_push     INTEGER NOT NULL DEFAULT 0,
	parent         TEXT NOT NULL DEFAULT ''
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

// SQLMetaStore persists all MetaStore state in one SQL database (docs/storage/storage.md
// §2.3). Immutable tables (packs, objects) insert-ignore — first writer
// wins; the head CAS runs on a dedicated connection inside a write
// transaction, the linearization point for ref updates.
//
// Data operations go through orm (GORM mounted on the same pool as db —
// gormstore.go); db stays the handle for raw DDL (applySchema,
// migrateColumn) and Backup.
type SQLMetaStore struct {
	db  *sql.DB
	orm *gorm.DB
	d   sqlDialect
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
	orm, err := gormOpen(backend, db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("%s meta: gorm: %w", d.name, err)
	}
	s := &SQLMetaStore{db: db, orm: orm, d: d}
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
	// The hierarchy link: MySQL can't DEFAULT a TEXT column, so the ALTER
	// spells a VARCHAR like the schema (the column is repo names, not blobs).
	parentType := "TEXT NOT NULL DEFAULT ''"
	if s.d.name == "mysql" {
		parentType = "VARCHAR(190) NOT NULL DEFAULT ''"
	}
	if err := s.migrateColumn(ctx, "repos", "parent", parentType, ""); err != nil {
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

// subtreeRulesDDL is the runtime directory-access-rule table (docs/gateway/authorization.md §8)
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

// tokensDDL is the personal-access-token table (docs/gateway/authorization.md §6) in this
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
	orm := s.orm.WithContext(ctx)
	// Rename tombstone (docs/storage/storage.md §6b): a name retired by a rename must not
	// be resurrected — a late push or auto-create under the dead name would
	// ack into a fresh, disconnected repository.
	var dst string
	err := orm.Model(&renameFenceRow{}).Select("dst").
		Where("frm = ? AND done = 1", repo).Limit(1).Row().Scan(&dst)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s was renamed to %s", ErrRepoRenamedAway, repo, dst)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	err = orm.Create(&repoRow{Name: repo, Algo: int(algo), DefaultBranch: "main", StorageID: NewStorageID()}).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("repo %s already exists", repo)
	}
	return err
}

// BeginRename installs the fence row (docs/storage/storage.md §6b): fail-fast checks —
// source exists, target free, no overlapping fence — run here so a doomed
// rename never blocks the repo.
func (s *SQLMetaStore) BeginRename(ctx context.Context, from, to string) error {
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var one int
		switch err := txq(tx).Model(&repoRow{}).Select("1").Where("name = ?", from).Limit(1).Row().Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrRepoNotFound, from)
		case err != nil:
			return err
		}
		switch err := txq(tx).Model(&repoRow{}).Select("1").Where("name = ?", to).Limit(1).Row().Scan(&one); {
		case err == nil:
			return fmt.Errorf("repo %s already exists", to)
		case errors.Is(err, sql.ErrNoRows):
			// target free
		default:
			return err
		}
		// Idempotent re-install of the SAME fence (rename re-run after a crash);
		// only a DIFFERENT overlapping rename is refused. Completed renames
		// (tombstones) never match: their source repo no longer exists.
		q := txq(tx).Model(&renameFenceRow{}).Select("dst").Where("frm = ? AND done = 0", from).Limit(1)
		if s.d.rowLock != "" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var dst string
		switch err := q.Row().Scan(&dst); {
		case err == nil:
			if dst == to {
				return nil // already fenced for this exact rename (commits)
			}
			return fmt.Errorf("%w: %s->%s", ErrRepoRenaming, from, to)
		case errors.Is(err, sql.ErrNoRows):
			// no fence row — continue
		default:
			return err
		}
		if hit, err := s.ormFenceHit(tx, from, to); err != nil {
			return err
		} else if hit {
			return fmt.Errorf("%w: %s->%s", ErrRepoRenaming, from, to)
		}
		return txq(tx).Create(&renameFenceRow{Frm: from, Dst: to}).Error
	})
}

func (s *SQLMetaStore) AbortRename(ctx context.Context, from, to string) error {
	// Active fence only — a completed rename's tombstone is not abortable
	// (the rename happened; AbortRename only cleans up interrupted ones).
	return s.orm.WithContext(ctx).
		Where("frm = ? AND dst = ? AND done = 0", from, to).
		Delete(&renameFenceRow{}).Error
}

func (s *SQLMetaStore) GetRepo(ctx context.Context, repo string) (RepoInfo, error) {
	var r repoRow
	err := s.orm.WithContext(ctx).Where("name = ?", repo).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RepoInfo{}, fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	if err != nil {
		return RepoInfo{}, err
	}
	return r.info(), nil
}

func (s *SQLMetaStore) SetForcePush(ctx context.Context, repo string, enabled bool) error {
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	res := s.orm.WithContext(ctx).Model(&repoRow{}).
		Where("name = ?", repo).Update("force_push", boolInt(enabled))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
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
	err := s.orm.WithContext(ctx).Model(&repoRow{}).Select("1").
		Where("name = ?", repo).Limit(1).Row().Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	case err != nil:
		return err
	}
	return s.orm.WithContext(ctx).Model(&repoRow{}).
		Where("name = ?", repo).Update("default_branch", branch).Error
}

// SetRepoParent re-homes a repo in the hierarchy (docs/design/data-model.md §14). The
// ancestor walk and the UPDATE share one transaction whose chain reads take
// row locks on the MVCC backends: two opposite-direction links racing each
// other deadlock on the row locks instead of committing a loop (one side
// aborts with the error). SQLite serializes writers on the file lock, so a
// deferred tx is equally safe there.
func (s *SQLMetaStore) SetRepoParent(ctx context.Context, repo string, parent string) error {
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ormFenceReject(tx, repo); err != nil {
			return err
		}
		exists := func(name string) error {
			q := txq(tx).Model(&repoRow{}).Select("1").Where("name = ?", name).Limit(1)
			if s.d.rowLock != "" {
				q = q.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var one int
			switch err := q.Row().Scan(&one); {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("%w: %s", ErrRepoNotFound, name)
			default:
				return err
			}
		}
		if err := exists(repo); err != nil {
			return err
		}
		var walkErr error
		if err := ValidateReparent(repo, parent, func(name string) (string, bool) {
			q := txq(tx).Model(&repoRow{}).Select("parent").Where("name = ?", name).Limit(1)
			if s.d.rowLock != "" {
				q = q.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var p string
			switch err := q.Row().Scan(&p); {
			case err == nil:
				return p, true
			case errors.Is(err, sql.ErrNoRows):
				return "", false
			default:
				walkErr = err // a real query failure must not read as "no such repo"
				return "", false
			}
		}); err != nil {
			if walkErr != nil {
				return walkErr
			}
			return err
		}
		return txq(tx).Model(&repoRow{}).Where("name = ?", repo).Update("parent", parent).Error
	})
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
	var rows []repoRow
	if err := s.orm.WithContext(ctx).Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RepoInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.info())
	}
	return out, nil
}

// RenameRepo moves every repo-keyed row in one transaction (docs/storage/storage.md §6).
// The storage_id column rides along untouched — hashed addressing keys stay
// stable across renames by construction.
func (s *SQLMetaStore) RenameRepo(ctx context.Context, from, to string) error {
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var one int
		switch err := txq(tx).Model(&repoRow{}).Select("1").Where("name = ?", from).Limit(1).Row().Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrRepoNotFound, from)
		case err != nil:
			return err
		}
		switch err := txq(tx).Model(&repoRow{}).Select("1").Where("name = ?", to).Limit(1).Row().Scan(&one); {
		case err == nil:
			return fmt.Errorf("repo %s already exists", to)
		case errors.Is(err, sql.ErrNoRows):
			// target free
		default:
			return err
		}
		// The per-table moves are bulk statements over admin-paced tables —
		// plain Exec through the transaction; builder-shaped SQL adds nothing.
		for _, table := range []string{"timelines", "packs", "pack_nodes", "objects", "placement", "hooks", "repo_blobs", "subtree_rules"} {
			if err := txq(tx).Exec("UPDATE "+table+" SET repo = ? WHERE repo = ?", to, from).Error; err != nil {
				return err
			}
		}
		// repo_pools keys by member, not repo: rename the membership side; the
		// pool side keeps pointing at the (unchanged) pool repository.
		if err := txq(tx).Model(&repoPoolRow{}).
			Where("member = ?", from).Update("member", to).Error; err != nil {
			return err
		}
		// Children follow the renamed parent — the hierarchy link is name-keyed,
		// and `to` was just confirmed absent, so no child can already point at it.
		if err := txq(tx).Model(&repoRow{}).
			Where("parent = ?", from).Update("parent", to).Error; err != nil {
			return err
		}
		if err := txq(tx).Model(&repoRow{}).
			Where("name = ?", from).Update("name", to).Error; err != nil {
			return err
		}
		// The flip retires the fence into a tombstone inside the same
		// transaction: mutations under `to` are legal the moment the rename
		// commits, while re-creating `from` stays refused (a late push under the
		// dead name must not ack into a disconnected fresh repo). Tombstones
		// pointing at the old name retarget to the new one — the hazard chain
		// follows the data.
		if err := txq(tx).Model(&renameFenceRow{}).
			Where("frm = ?", from).Update("done", 1).Error; err != nil {
			return err
		}
		return txq(tx).Model(&renameFenceRow{}).
			Where("dst = ? AND done = 1", from).Update("dst", to).Error
	})
}

// DeleteRepo drops every repo-keyed row in one transaction. Missing = no-op
// (idempotent). Storage bytes are reclaimed via ObjectService.DeleteRepo.
func (s *SQLMetaStore) DeleteRepo(ctx context.Context, repo string) error {
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ormFenceReject(tx, repo); err != nil {
			return err
		}
		// Deleting a parent re-homes its children one level up (to the deleted
		// repo's own parent) — directory semantics, the tree stays connected
		// instead of sprouting roots that point at a vanished name. A missing
		// repo keeps deletion idempotent: the deletes below are no-ops anyway.
		var orphanTo string
		if err := txq(tx).Model(&repoRow{}).Select("parent").
			Where("name = ?", repo).Limit(1).Row().Scan(&orphanTo); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, table := range []string{"objects", "pack_nodes", "packs", "timelines", "placement", "hooks", "repo_blobs", "subtree_rules"} {
			if err := txq(tx).Exec("DELETE FROM "+table+" WHERE repo = ?", repo).Error; err != nil {
				return err
			}
		}
		// The repo leaves its pool (member row) and, when it was a pool, every
		// membership record pointing at it goes too.
		if err := txq(tx).Exec("DELETE FROM repo_pools WHERE member = ? OR pool = ?", repo, repo).Error; err != nil {
			return err
		}
		// Deletion retires the name for real: tombstones blocking older names
		// (retired into this repo) lift — the recycled name is creatable again.
		if err := txq(tx).Exec("DELETE FROM rename_fence WHERE frm = ? OR dst = ?", repo, repo).Error; err != nil {
			return err
		}
		if err := txq(tx).Where("name = ?", repo).Delete(&repoRow{}).Error; err != nil {
			return err
		}
		// Children re-home to the deleted repo's parent.
		return txq(tx).Model(&repoRow{}).
			Where("parent = ?", repo).Update("parent", orphanTo).Error
	})
}

func (s *SQLMetaStore) PutPack(ctx context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error {
	if len(nodes) == 0 {
		return fmt.Errorf("putpack %s/%s: no nodes", repo, packOID)
	}
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ormFenceReject(tx, repo); err != nil {
			return err
		}
		var one int
		switch err := txq(tx).Model(&repoRow{}).Select("1").Where("name = ?", repo).Limit(1).Row().Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
		case err != nil:
			return err
		}
		// Immutable shards: registration is INSERT-ONLY and idempotent —
		// first writer wins (OnConflict DoNothing is the dialect-neutral
		// insert-ignore).
		if err := txq(tx).Clauses(clause.OnConflict{DoNothing: true}).
			Create(&packRow{Repo: repo, PackOID: string(packOID)}).Error; err != nil {
			return err
		}
		holderRows := make([]packNodeRow, len(nodes))
		for i, n := range nodes {
			holderRows[i] = packNodeRow{Repo: repo, PackOID: string(packOID), NodeID: n}
		}
		if err := txq(tx).Clauses(clause.OnConflict{DoNothing: true}).
			CreateInBatches(holderRows, sqlChunk).Error; err != nil {
			return err
		}
		// UPSERT (not insert-ignore): repack registers a merged pack while
		// object rows still point at the old shards — they must re-point to
		// the newly registered pack, or the subsequent DropPack deletes them.
		primary := nodes[0]
		objRows := make([]objectRow, len(objects))
		for i, o := range objects {
			objRows[i] = objectRow{Repo: repo, OID: string(o), PackOID: string(packOID), NodeID: primary}
		}
		return txq(tx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "repo"}, {Name: "oid"}},
			DoUpdates: clause.Assignments(map[string]any{"pack_oid": string(packOID), "node_id": primary}),
		}).CreateInBatches(objRows, sqlChunk).Error
	})
}

func (s *SQLMetaStore) GetPack(ctx context.Context, repo string, packOID model.OID) (PackInfo, error) {
	orm := s.orm.WithContext(ctx)
	var one int
	switch err := orm.Model(&packRow{}).Select("1").
		Where("repo = ? AND pack_oid = ?", repo, string(packOID)).Limit(1).Row().Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return PackInfo{}, fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	case err != nil:
		return PackInfo{}, err
	}
	var nodes []string
	if err := orm.Model(&packNodeRow{}).
		Where("repo = ? AND pack_oid = ?", repo, string(packOID)).
		Order("node_id").Pluck("node_id", &nodes).Error; err != nil {
		return PackInfo{}, err
	}
	return PackInfo{PackOID: packOID, Nodes: nodes}, nil
}

func (s *SQLMetaStore) ListPacks(ctx context.Context, repo string) ([]PackInfo, error) {
	var rows []struct {
		PackOID string `gorm:"column:pack_oid"`
		NodeID  string `gorm:"column:node_id"`
	}
	err := s.orm.WithContext(ctx).Raw(
		"SELECT p.pack_oid, n.node_id FROM packs p JOIN pack_nodes n ON p.repo = n.repo AND p.pack_oid = n.pack_oid WHERE p.repo = ?",
		repo).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	byOID := map[model.OID][]string{}
	for _, r := range rows {
		byOID[model.OID(r.PackOID)] = append(byOID[model.OID(r.PackOID)], r.NodeID)
	}
	out := make([]PackInfo, 0, len(byOID))
	for packOID, nodes := range byOID {
		out = append(out, PackInfo{PackOID: packOID, Nodes: nodes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PackOID < out[j].PackOID })
	return out, nil
}

func (s *SQLMetaStore) PackObjects(ctx context.Context, repo string, packOID model.OID) ([]model.OID, error) {
	orm := s.orm.WithContext(ctx)
	var one int
	switch err := orm.Model(&packRow{}).Select("1").
		Where("repo = ? AND pack_oid = ?", repo, string(packOID)).Limit(1).Row().Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	case err != nil:
		return nil, err
	}
	var oids []string
	if err := orm.Model(&objectRow{}).
		Where("repo = ? AND pack_oid = ?", repo, string(packOID)).
		Pluck("oid", &oids).Error; err != nil {
		return nil, err
	}
	out := make([]model.OID, 0, len(oids))
	for _, o := range oids {
		out = append(out, model.OID(o))
	}
	return out, nil
}

// DropPack unregisters the pack and its object locations in one transaction.
// Missing-object rows are tolerated (they may have been re-registered to the
// replacement pack already); a missing pack row is ErrPackNotFound.
func (s *SQLMetaStore) DropPack(ctx context.Context, repo string, packOID model.OID) error {
	var one int
	switch err := s.orm.WithContext(ctx).Model(&packRow{}).Select("1").
		Where("repo = ? AND pack_oid = ?", repo, string(packOID)).Limit(1).Row().Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	case err != nil:
		return err
	}
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ormFenceReject(tx, repo); err != nil {
			return err
		}
		// Object rows pointing at this pack are deleted only when they still do:
		// the replacement pack's registration may have already re-pointed them.
		for _, m := range []any{&objectRow{}, &packNodeRow{}, &packRow{}} {
			if err := txq(tx).Where("repo = ? AND pack_oid = ?", repo, string(packOID)).Delete(m).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// SetPackNodes atomically replaces the holder set (reconciler swap, docs/storage/ha.md §8.3).
func (s *SQLMetaStore) SetPackNodes(ctx context.Context, repo string, packOID model.OID, nodes []string) error {
	var one int
	switch err := s.orm.WithContext(ctx).Model(&packRow{}).Select("1").
		Where("repo = ? AND pack_oid = ?", repo, string(packOID)).Limit(1).Row().Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	case err != nil:
		return err
	}
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ormFenceReject(tx, repo); err != nil {
			return err
		}
		if err := txq(tx).Where("repo = ? AND pack_oid = ?", repo, string(packOID)).
			Delete(&packNodeRow{}).Error; err != nil {
			return err
		}
		holderRows := make([]packNodeRow, len(nodes))
		for i, n := range nodes {
			holderRows[i] = packNodeRow{Repo: repo, PackOID: string(packOID), NodeID: n}
		}
		if len(holderRows) > 0 {
			// Plain insert (not insert-ignore): a duplicate node in the
			// replacement set is a caller bug that should surface.
			if err := txq(tx).CreateInBatches(holderRows, sqlChunk).Error; err != nil {
				return err
			}
		}
		if len(nodes) > 0 {
			return txq(tx).Model(&objectRow{}).
				Where("repo = ? AND pack_oid = ?", repo, string(packOID)).
				Update("node_id", nodes[0]).Error
		}
		return nil
	})
}

func (s *SQLMetaStore) SetPlacement(ctx context.Context, repo string, replicas int) error {
	if err := s.ormFenceReject(s.orm.WithContext(ctx), repo); err != nil {
		return err
	}
	if _, err := s.GetRepo(ctx, repo); err != nil {
		return err
	}
	// The dialect-neutral upsert (ON CONFLICT / ON DUPLICATE KEY — GORM
	// renders per backend) replaces the per-dialect spellings.
	return s.orm.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo"}},
		DoUpdates: clause.AssignmentColumns([]string{"replicas"}),
	}).Create(&placementRow{Repo: repo, Replicas: replicas}).Error
}

func (s *SQLMetaStore) GetPlacement(ctx context.Context, repo string) (int, error) {
	if _, err := s.GetRepo(ctx, repo); err != nil {
		return 0, err
	}
	var p placementRow
	err := s.orm.WithContext(ctx).Where("repo = ?", repo).Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil // no row = cluster default
	}
	if err != nil {
		return 0, err
	}
	return p.Replicas, nil
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
		var rows []objectRow
		// A fresh chain per chunk: a reused chain would accumulate one
		// `oid IN` clause per iteration (the txq discipline, non-tx flavor).
		if err := s.orm.WithContext(ctx).Model(&objectRow{}).
			Where("repo = ?", repo).
			Where("oid IN ?", keys[start:end]).
			Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[model.OID(r.OID)] = ObjectLocation{PackOID: model.OID(r.PackOID), NodeID: r.NodeID}
		}
	}
	return out, nil
}

func (s *SQLMetaStore) Timeline(ctx context.Context, tl model.Timeline) (model.TimelineState, error) {
	var row timelineRow
	err := s.orm.WithContext(ctx).
		Where("repo = ? AND anchor = ? AND name = ?", tl.Repo, string(tl.Anchor), tl.Name).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.TimelineState{}, ErrTimelineUnknown
	}
	if err != nil {
		return model.TimelineState{}, err
	}
	return model.TimelineState{Head: model.OID(row.Head), Base: model.OID(row.Base)}, nil
}

// CASHead runs the compare-and-set inside one write transaction — the
// linearization point for ref updates. SQLite: every BEGIN is IMMEDIATE
// (_txlock=immediate in the DSN), so the write lock precedes the read;
// MySQL/PostgreSQL: the row is locked with SELECT ... FOR UPDATE, and a lost
// insert race on a timeline that did not exist surfaces as the PK unique
// violation it is (translated to gorm.ErrDuplicatedKey).
func (s *SQLMetaStore) CASHead(ctx context.Context, tl model.Timeline, expected, next model.OID) error {
	expected = expected.NormalizeZero()
	next = next.NormalizeZero()
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Fence probe inside the write transaction: a rename whose fence went in
		// after this CAS began cannot slip its flip in before COMMIT.
		if err := s.ormFenceReject(tx, tl.Repo); err != nil {
			return err
		}
		q := txq(tx).Model(&timelineRow{}).
			Where("repo = ? AND anchor = ? AND name = ?", tl.Repo, string(tl.Anchor), tl.Name)
		if s.d.rowLock != "" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var row timelineRow
		err := q.Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if expected != "" {
				mCASConflicts.With().Inc()
				return ErrNonFastForward // creating requires expected=zero
			}
			if next == "" {
				return nil // delete of a missing timeline is a no-op
			}
			err = txq(tx).Create(&timelineRow{
				Repo: tl.Repo, Anchor: string(tl.Anchor), Name: tl.Name, Head: string(next),
			}).Error
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				mCASConflicts.With().Inc()
				return ErrNonFastForward // another writer created it first
			}
			return err
		}
		if err != nil {
			return err
		}
		if model.OID(row.Head).NormalizeZero() != expected {
			mCASConflicts.With().Inc()
			return ErrNonFastForward
		}
		if next == "" {
			// ref deletion: zeroing the head removes the timeline entry.
			return txq(tx).
				Where("repo = ? AND anchor = ? AND name = ?", tl.Repo, string(tl.Anchor), tl.Name).
				Delete(&timelineRow{}).Error
		}
		return txq(tx).Model(&timelineRow{}).
			Where("repo = ? AND anchor = ? AND name = ?", tl.Repo, string(tl.Anchor), tl.Name).
			Update("head", string(next)).Error
	})
}

func (s *SQLMetaStore) EnsureTimeline(ctx context.Context, tl model.Timeline, head, base model.OID) error {
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ormFenceReject(tx, tl.Repo); err != nil {
			return err
		}
		var one int
		switch err := txq(tx).Model(&timelineRow{}).Select("1").
			Where("repo = ? AND anchor = ? AND name = ?", tl.Repo, string(tl.Anchor), tl.Name).
			Limit(1).Row().Scan(&one); {
		case err == nil:
			return nil // already exists (incl. locally committed): don't overwrite
		case errors.Is(err, sql.ErrNoRows):
			return txq(tx).Create(&timelineRow{
				Repo: tl.Repo, Anchor: string(tl.Anchor), Name: tl.Name,
				Head: string(head), Base: string(base),
			}).Error
		default:
			return err
		}
	})
}

// SetTimelineBase advances an existing timeline's fork point (explicit-rebase
// bookkeeping, docs/design/data-model.md §12). RowsAffected distinguishes the unknown-timeline
// rejection across all three dialects.
func (s *SQLMetaStore) SetTimelineBase(ctx context.Context, tl model.Timeline, base model.OID) error {
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.ormFenceReject(tx, tl.Repo); err != nil {
			return err
		}
		res := txq(tx).Model(&timelineRow{}).
			Where("repo = ? AND anchor = ? AND name = ?", tl.Repo, string(tl.Anchor), tl.Name).
			Update("base", string(base))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrTimelineUnknown
		}
		return nil
	})
}

func (s *SQLMetaStore) ListTimelines(ctx context.Context, repo string, anchor model.AnchorPath) ([]model.Timeline, error) {
	var rows []timelineRow
	err := s.orm.WithContext(ctx).
		Where("repo = ? AND anchor = ?", repo, string(anchor)).
		Order("name").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]model.Timeline, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.Timeline{Repo: repo, Anchor: model.AnchorPath(r.Anchor), Name: r.Name})
	}
	return out, nil
}

func (s *SQLMetaStore) ListAllTimelines(ctx context.Context, repo string) ([]model.Timeline, error) {
	var rows []timelineRow
	err := s.orm.WithContext(ctx).
		Where("repo = ?", repo).
		Order("anchor, name").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]model.Timeline, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.Timeline{Repo: repo, Anchor: model.AnchorPath(r.Anchor), Name: r.Name})
	}
	return out, nil
}

// Backup writes a consistent snapshot of a SQLite metadata database to path
// (VACUUM INTO — online, readers and the writer keep running). MySQL and
// PostgreSQL deployments take their backups with the database ecosystem's
// own tooling — that HA is theirs to provide (docs/storage/ha.md).
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
