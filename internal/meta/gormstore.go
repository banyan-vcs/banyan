package meta

// The GORM face of SQLMetaStore: row models (one per table), the open path,
// and the ORM-side fence probe. Design lines:
//
//   - ONE connection pool. gormOpen mounts GORM on the *sql.DB that
//     NewSQLMetaStore already opened (Conn, not a second pool): the DSN's
//     pragmas — WAL, busy_timeout, _txlock=immediate — keep applying, and
//     schema management (applySchema/migrateColumn, raw DDL by design) shares
//     the same handle.
//   - Schema stays explicit. AutoMigrate is deliberately not used: WITHOUT
//     ROWID, the MySQL VARCHAR sizes (InnoDB utf8mb4 key-width limits) and
//     the quoted pg "offset" column are load-bearing (docs/operations/testing.md
//     conformance runs the DDL verbatim). The models mirror the DDL; tags
//     only carry primary keys and non-default column names.
//   - Integer flags, not bools. force_push / revoked / upstream columns are
//     INTEGER on PostgreSQL (no implicit bool→int cast there), so the rows
//     model them as int and every backend writes 0/1.
//   - Transactions are deliberate, never incidental: SkipDefaultTransaction
//     drops GORM's implicit per-Create wrapper; the CAS/fence discipline
//     opens transactions where the linearization needs them.

import (
	"database/sql"
	"errors"
	"fmt"

	gosqlite "github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/banyan-vcs/banyan/internal/model"
)

// --- row models (mirror of ddl*(); table names are the historical ones) ---

type repoRow struct {
	Name          string `gorm:"primaryKey"`
	Algo          int
	DefaultBranch string
	StorageID     string
	ForcePush     int
	Parent        string
}

func (repoRow) TableName() string { return "repos" }

func (r repoRow) info() RepoInfo {
	return RepoInfo{
		ID:            r.Name,
		Algo:          model.HashAlgo(r.Algo),
		DefaultBranch: r.DefaultBranch,
		StorageID:     r.StorageID,
		ForcePush:     r.ForcePush != 0,
		Parent:        r.Parent,
	}
}

type timelineRow struct {
	Repo   string `gorm:"primaryKey"`
	Anchor string `gorm:"primaryKey"`
	Name   string `gorm:"primaryKey"`
	Head   string
	Base   string
}

func (timelineRow) TableName() string { return "timelines" }

type packRow struct {
	Repo    string `gorm:"primaryKey"`
	PackOID string `gorm:"primaryKey;column:pack_oid"`
}

func (packRow) TableName() string { return "packs" }

type packNodeRow struct {
	Repo    string `gorm:"primaryKey"`
	PackOID string `gorm:"primaryKey;column:pack_oid"`
	NodeID  string `gorm:"primaryKey"`
}

func (packNodeRow) TableName() string { return "pack_nodes" }

type objectRow struct {
	Repo    string `gorm:"primaryKey"`
	OID     string `gorm:"primaryKey;column:oid"` // OID would snake-case to "o_id"
	PackOID string `gorm:"column:pack_oid"`
	NodeID  string `gorm:"column:node_id"`
	Offset  int
}

func (objectRow) TableName() string { return "objects" }

type placementRow struct {
	Repo     string `gorm:"primaryKey"`
	Replicas int
}

func (placementRow) TableName() string { return "placement" }

type hookRow struct {
	Repo      string `gorm:"primaryKey"`
	ID        string `gorm:"primaryKey"`
	Dir       string
	Events    string
	URL       string `gorm:"column:url"`
	Secret    string
	TimeoutMS int    `gorm:"column:timeout_ms"`
}

func (hookRow) TableName() string { return "hooks" }

type repoBlobRow struct {
	Repo    string `gorm:"primaryKey"`
	BlobKey string `gorm:"primaryKey;column:blob_key"`
	Data    []byte
}

func (repoBlobRow) TableName() string { return "repo_blobs" }

type repoPoolRow struct {
	Member   string `gorm:"primaryKey"`
	Pool     string
	Upstream int
}

func (repoPoolRow) TableName() string { return "repo_pools" }

type renameFenceRow struct {
	Frm  string `gorm:"primaryKey;column:frm"`
	Dst  string `gorm:"column:dst"`
	Done int    `gorm:"column:done"`
}

func (renameFenceRow) TableName() string { return "rename_fence" }

type tokenRow struct {
	TokenHash string `gorm:"primaryKey;column:token_hash"`
	Owner     string
	Name      string
	Scope     string
	ExpiresAt int64 `gorm:"column:expires_at"`
	Revoked   int
	CreatedAt int64 `gorm:"column:created_at"`
}

func (tokenRow) TableName() string { return "tokens" }

func (r tokenRow) info() TokenInfo {
	return TokenInfo{
		Hash: r.TokenHash, User: r.Owner, Name: r.Name, Scope: r.Scope,
		Expires: r.ExpiresAt, Revoked: r.Revoked != 0, Created: r.CreatedAt,
	}
}

type subtreeRuleRow struct {
	Repo  string `gorm:"primaryKey"`
	Path  string `gorm:"primaryKey"`
	Users string
}

func (subtreeRuleRow) TableName() string { return "subtree_rules" }

// --- open path ---

// gormOpen mounts GORM on the existing pool (one pool, two query surfaces).
// TranslateError maps unique violations to gorm.ErrDuplicatedKey uniformly
// across dialects — the isUnique per-driver probes retire as methods move
// onto the ORM.
func gormOpen(backend string, db *sql.DB) (*gorm.DB, error) {
	cfg := &gorm.Config{
		TranslateError:         true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Discard,
	}
	var dialector gorm.Dialector
	switch backend {
	case "sqlite":
		dialector = gosqlite.Dialector{Conn: db}
	case "mysql":
		dialector = mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true})
	case "postgres":
		dialector = postgres.New(postgres.Config{Conn: db})
	default:
		return nil, fmt.Errorf("gorm meta: unknown backend %q", backend)
	}
	return gorm.Open(dialector, cfg)
}

// txq starts a clean statement on a transaction. Reusing a finished gorm.DB
// chain can inherit its clauses; every ORM statement in a transaction starts
// from here so probe/walk/update sequences never bleed into each other.
func txq(tx *gorm.DB) *gorm.DB { return tx.Session(&gorm.Session{NewDB: true}) }

// GormOpen is gormOpen exported for the other SQL-backed stores (the web
// tier, the console) — the same mount discipline: GORM on the pool the
// caller opened, schema management stays the caller's raw DDL.
func GormOpen(backend string, db *sql.DB) (*gorm.DB, error) { return gormOpen(backend, db) }

// ormFenceHit is the GORM-side fence probe for mutating transactions: run as
// the first statement of the transaction it takes a locking read (rowLock on
// the MVCC backends; SQLite serializes writers wholesale — _txlock=immediate
// in the DSN), so a fence installed after this probe cannot slip its flip in
// before COMMIT — and the flip's DELETE of the fence row blocks until this
// transaction settles.
func (s *SQLMetaStore) ormFenceHit(tx *gorm.DB, names ...string) (bool, error) {
	for _, n := range names {
		q := txq(tx).Table("rename_fence").Select("1").
			Where("(frm = ? OR dst = ?) AND done = 0", n, n).Limit(1)
		if s.d.rowLock != "" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var one int
		switch err := q.Row().Scan(&one); {
		case err == nil:
			return true, nil
		case errors.Is(err, sql.ErrNoRows):
			// next name
		default:
			return false, err
		}
	}
	return false, nil
}

// ormFenceReject returns ErrRepoRenaming when a fence covers any name.
func (s *SQLMetaStore) ormFenceReject(tx *gorm.DB, names ...string) error {
	hit, err := s.ormFenceHit(tx, names...)
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
