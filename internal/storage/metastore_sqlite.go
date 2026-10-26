package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo (OS-neutral, AGENTS.md)
)

// SQLiteMetaStore persists all MetaStore state in one SQLite database file
// (docs/03 §2.3). WAL mode + busy_timeout keeps readers running during the
// single-writer head CAS (BEGIN IMMEDIATE on a dedicated connection). The
// immutable tables (packs, objects) are INSERT OR IGNORE — first writer wins.
type SQLiteMetaStore struct {
	db *sql.DB
}

const sqliteChunk = 500 // rows per batched statement (safe under param limits)

const schemaV1 = `
CREATE TABLE IF NOT EXISTS repos(
	name           TEXT PRIMARY KEY,
	algo           INTEGER NOT NULL,
	default_branch TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS timelines(
	repo   TEXT NOT NULL,
	anchor TEXT NOT NULL,
	name   TEXT NOT NULL,
	head   TEXT NOT NULL,
	base   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(repo, anchor, name)
);
CREATE TABLE IF NOT EXISTS packs(
	repo     TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	PRIMARY KEY(repo, pack_oid)
);
CREATE TABLE IF NOT EXISTS pack_nodes(
	repo     TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	PRIMARY KEY(repo, pack_oid, node_id)
);
CREATE TABLE IF NOT EXISTS objects(
	repo   TEXT NOT NULL,
	oid    TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	offset   INTEGER NOT NULL DEFAULT 0, -- reserved: M3 fill, saves one idx roundtrip
	PRIMARY KEY(repo, oid)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_objects_pack ON objects(repo, pack_oid);
CREATE TABLE IF NOT EXISTS placement(
	repo     TEXT PRIMARY KEY,
	replicas INTEGER NOT NULL DEFAULT 0    -- 0 = cluster default (docs/10 §3)
);
CREATE TABLE IF NOT EXISTS hooks(
	repo       TEXT NOT NULL,
	id         TEXT NOT NULL,
	dir        TEXT NOT NULL DEFAULT '',   -- scope: '' = repo level, else directory path
	events     TEXT NOT NULL,             -- comma-separated event names
	url        TEXT NOT NULL,
	secret     TEXT NOT NULL DEFAULT '',
	timeout_ms INTEGER NOT NULL DEFAULT 0, -- 0 = gateway default
	PRIMARY KEY(repo, id)
) WITHOUT ROWID;
`

// sqliteDSN builds the connection string. Pragmas must ride the DSN:
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

// NewSQLiteMetaStore opens (creating if needed) the database at path and
// applies pragmas + schema. The file's parent directory is created.
func NewSQLiteMetaStore(path string) (*SQLiteMetaStore, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlite meta: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite meta: open: %w", err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite meta: schema: %w", err)
	}
	if err := migrateRepoStorageID(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite meta: storage-id migration: %w", err)
	}
	if err := migrateRepoForcePush(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite meta: force-push migration: %w", err)
	}
	return &SQLiteMetaStore{db: db}, nil
}

// migrateRepoStorageID adds repos.storage_id for databases predating the
// column (docs/03 §6) and backfills every existing row with a fresh identity.
func migrateRepoStorageID(db *sql.DB) error {
	var hasCol int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('repos') WHERE name='storage_id'`).Scan(&hasCol); err != nil {
		return err
	}
	if hasCol == 1 {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE repos ADD COLUMN storage_id TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	rows, err := db.Query("SELECT name FROM repos WHERE storage_id=''")
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
		if _, err := db.Exec("UPDATE repos SET storage_id=? WHERE name=?", NewStorageID(), n); err != nil {
			return err
		}
	}
	return nil
}

// migrateRepoForcePush adds repos.force_push for databases predating the
// column (docs/04 §6a repo-level switch; default off = guard active).
func migrateRepoForcePush(db *sql.DB) error {
	var hasCol int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('repos') WHERE name='force_push'`).Scan(&hasCol); err != nil {
		return err
	}
	if hasCol == 1 {
		return nil
	}
	_, err := db.Exec("ALTER TABLE repos ADD COLUMN force_push INTEGER NOT NULL DEFAULT 0")
	return err
}

func (s *SQLiteMetaStore) Close() error { return s.db.Close() }

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *SQLiteMetaStore) CreateRepo(ctx context.Context, repo string, algo model.HashAlgo) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO repos(name, algo, default_branch, storage_id) VALUES(?, ?, 'main', ?)",
		repo, int(algo), NewStorageID())
	if isUniqueViolation(err) {
		return fmt.Errorf("repo %s already exists", repo)
	}
	return err
}

func (s *SQLiteMetaStore) GetRepo(ctx context.Context, repo string) (RepoInfo, error) {
	var info RepoInfo
	var algo int
	var force int
	err := s.db.QueryRowContext(ctx,
		"SELECT name, algo, default_branch, storage_id, force_push FROM repos WHERE name=?", repo).
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

func (s *SQLiteMetaStore) SetForcePush(ctx context.Context, repo string, enabled bool) error {
	res, err := s.db.ExecContext(ctx, "UPDATE repos SET force_push=? WHERE name=?", boolInt(enabled), repo)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *SQLiteMetaStore) ListHooks(ctx context.Context, repo string) ([]HookInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, dir, events, url, secret, timeout_ms FROM hooks WHERE repo=? ORDER BY id", repo)
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

func (s *SQLiteMetaStore) SetHook(ctx context.Context, repo string, h HookInfo) error {
	if err := h.validate(); err != nil {
		return err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos WHERE name=?", repo).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO hooks(repo, id, dir, events, url, secret, timeout_ms)
		VALUES(?,?,?,?,?,?,?)`, repo, h.ID, h.Dir, strings.Join(h.Events, ","), h.URL, h.Secret, h.TimeoutMS)
	return err
}

func (s *SQLiteMetaStore) DeleteHook(ctx context.Context, repo, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM hooks WHERE repo=? AND id=?", repo, id)
	return err
}

func (s *SQLiteMetaStore) ListRepos(ctx context.Context) ([]RepoInfo, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, algo, default_branch, storage_id, force_push FROM repos ORDER BY name")
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
func (s *SQLiteMetaStore) RenameRepo(ctx context.Context, from, to string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM repos WHERE name=?", from).Scan(&one); err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, from)
	} else if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM repos WHERE name=?", to).Scan(&one); err == nil {
		return fmt.Errorf("repo %s already exists", to)
	} else if err != sql.ErrNoRows {
		return err
	}
	for _, table := range []string{"timelines", "packs", "pack_nodes", "objects", "placement"} {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET repo=? WHERE repo=?", to, from); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE repos SET name=? WHERE name=?", to, from); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteMetaStore) PutPack(ctx context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error {
	if len(nodes) == 0 {
		return fmt.Errorf("putpack %s/%s: no nodes", repo, packOID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var one string
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM repos WHERE name=?", repo).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO packs(repo, pack_oid) VALUES(?, ?)", repo, string(packOID)); err != nil {
		return err
	}
	for _, node := range nodes {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO pack_nodes(repo, pack_oid, node_id) VALUES(?, ?, ?)",
			repo, string(packOID), node); err != nil {
			return err
		}
	}
	primary := nodes[0]
	for start := 0; start < len(objects); start += sqliteChunk {
		end := start + sqliteChunk
		if end > len(objects) {
			end = len(objects)
		}
		var b strings.Builder
		// UPSERT (not INSERT OR IGNORE): repack registers a merged pack while
		// object rows still point at the old shards — they must re-point to the
		// newly registered pack, or the subsequent DropPack deletes them.
		b.WriteString("INSERT INTO objects(repo, oid, pack_oid, node_id) VALUES")
		args := make([]any, 0, (end-start)*4)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteString("(?, ?, ?, ?)")
			args = append(args, repo, string(objects[i]), string(packOID), primary)
		}
		if _, err := tx.ExecContext(ctx, b.String()+" ON CONFLICT(repo, oid) DO UPDATE SET pack_oid=excluded.pack_oid, node_id=excluded.node_id", args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteMetaStore) GetPack(ctx context.Context, repo string, packOID model.OID) (PackInfo, error) {
	var one string
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM packs WHERE repo=? AND pack_oid=?", repo, string(packOID)).Scan(&one)
	if err == sql.ErrNoRows {
		return PackInfo{}, fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	if err != nil {
		return PackInfo{}, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT node_id FROM pack_nodes WHERE repo=? AND pack_oid=? ORDER BY node_id", repo, string(packOID))
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

func (s *SQLiteMetaStore) ListPacks(ctx context.Context, repo string) ([]PackInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT p.pack_oid, n.node_id FROM packs p JOIN pack_nodes n ON p.repo=n.repo AND p.pack_oid=n.pack_oid WHERE p.repo=?",
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

func (s *SQLiteMetaStore) PackObjects(ctx context.Context, repo string, packOID model.OID) ([]model.OID, error) {
	var one string
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM packs WHERE repo=? AND pack_oid=?", repo, string(packOID)).Scan(&one)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT oid FROM objects WHERE repo=? AND pack_oid=?", repo, string(packOID))
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

// DropPack unregisters the pack and its object locations in one transaction.
// Missing-object rows are tolerated (they may have been re-registered to the
// replacement pack already); a missing pack row is ErrPackNotFound.
func (s *SQLiteMetaStore) DropPack(ctx context.Context, repo string, packOID model.OID) error {
	var one string
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM packs WHERE repo=? AND pack_oid=?", repo, string(packOID)).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	if err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	fail := func(err error) error {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	// Object rows pointing at this pack are deleted only when they still do:
	// the replacement pack's registration may have already re-pointed them.
	if _, err := conn.ExecContext(ctx,
		"DELETE FROM objects WHERE repo=? AND pack_oid=?", repo, string(packOID)); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx,
		"DELETE FROM pack_nodes WHERE repo=? AND pack_oid=?", repo, string(packOID)); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx,
		"DELETE FROM packs WHERE repo=? AND pack_oid=?", repo, string(packOID)); err != nil {
		return fail(err)
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// SetPackNodes atomically replaces the holder set (reconciler swap, docs/10 §8.3).
func (s *SQLiteMetaStore) SetPackNodes(ctx context.Context, repo string, packOID model.OID, nodes []string) error {
	var one string
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM packs WHERE repo=? AND pack_oid=?", repo, string(packOID)).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s/%s", ErrPackNotFound, repo, packOID)
	}
	if err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	fail := func(err error) error {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx,
		"DELETE FROM pack_nodes WHERE repo=? AND pack_oid=?", repo, string(packOID)); err != nil {
		return fail(err)
	}
	for _, n := range nodes {
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO pack_nodes(repo, pack_oid, node_id) VALUES(?,?,?)", repo, string(packOID), n); err != nil {
			return fail(err)
		}
	}
	if len(nodes) > 0 {
		if _, err := conn.ExecContext(ctx,
			"UPDATE objects SET node_id=? WHERE repo=? AND pack_oid=?", nodes[0], repo, string(packOID)); err != nil {
			return fail(err)
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func (s *SQLiteMetaStore) SetPlacement(ctx context.Context, repo string, replicas int) error {
	if _, err := s.GetRepo(ctx, repo); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO placement(repo, replicas) VALUES(?,?) ON CONFLICT(repo) DO UPDATE SET replicas=excluded.replicas",
		repo, replicas)
	return err
}

func (s *SQLiteMetaStore) GetPlacement(ctx context.Context, repo string) (int, error) {
	if _, err := s.GetRepo(ctx, repo); err != nil {
		return 0, err
	}
	var replicas int
	err := s.db.QueryRowContext(ctx,
		"SELECT replicas FROM placement WHERE repo=?", repo).Scan(&replicas)
	if err == sql.ErrNoRows {
		return 0, nil // no row = cluster default
	}
	if err != nil {
		return 0, err
	}
	return replicas, nil
}

func (s *SQLiteMetaStore) LocateObjects(ctx context.Context, repo string, oids []model.OID) (map[model.OID]ObjectLocation, error) {
	out := map[model.OID]ObjectLocation{}
	keys := make([]model.OID, 0, len(oids))
	seen := map[model.OID]bool{}
	for _, oid := range oids {
		if !seen[oid] {
			seen[oid] = true
			keys = append(keys, oid)
		}
	}
	for start := 0; start < len(keys); start += sqliteChunk {
		end := start + sqliteChunk
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
		rows, err := s.db.QueryContext(ctx, b.String(), args...)
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

func (s *SQLiteMetaStore) Timeline(ctx context.Context, tl model.Timeline) (model.TimelineState, error) {
	var st model.TimelineState
	err := s.db.QueryRowContext(ctx,
		"SELECT head, base FROM timelines WHERE repo=? AND anchor=? AND name=?",
		tl.Repo, string(tl.Anchor), tl.Name).Scan(&st.Head, &st.Base)
	if err == sql.ErrNoRows {
		return model.TimelineState{}, ErrTimelineUnknown
	}
	if err != nil {
		return model.TimelineState{}, err
	}
	return st, nil
}

// CASHead runs the compare-and-set inside one BEGIN IMMEDIATE transaction on
// a dedicated connection — the linearization point for ref updates.
func (s *SQLiteMetaStore) CASHead(ctx context.Context, tl model.Timeline, expected, next model.OID) error {
	expected = expected.NormalizeZero()
	next = next.NormalizeZero()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	fail := func(err error) error {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	var head, base string
	err = conn.QueryRowContext(ctx,
		"SELECT head, base FROM timelines WHERE repo=? AND anchor=? AND name=?",
		tl.Repo, string(tl.Anchor), tl.Name).Scan(&head, &base)
	if err == sql.ErrNoRows {
		if expected != "" {
			return fail(ErrNonFastForward) // creating requires expected=zero
		}
		if next == "" {
			return fail(nil) // delete of a missing timeline is a no-op
		}
		_, err = conn.ExecContext(ctx,
			"INSERT INTO timelines(repo, anchor, name, head, base) VALUES(?, ?, ?, ?, '')",
			tl.Repo, string(tl.Anchor), tl.Name, string(next))
		if err != nil {
			return fail(err)
		}
		_, err = conn.ExecContext(ctx, "COMMIT")
		return err
	}
	if err != nil {
		return fail(err)
	}
	if model.OID(head).NormalizeZero() != expected {
		return fail(ErrNonFastForward)
	}
	if next == "" {
		// ref deletion: zeroing the head removes the timeline entry.
		if _, err := conn.ExecContext(ctx,
			"DELETE FROM timelines WHERE repo=? AND anchor=? AND name=?",
			tl.Repo, string(tl.Anchor), tl.Name); err != nil {
			return fail(err)
		}
	} else if _, err := conn.ExecContext(ctx,
		"UPDATE timelines SET head=? WHERE repo=? AND anchor=? AND name=?",
		string(next), tl.Repo, string(tl.Anchor), tl.Name); err != nil {
		return fail(err)
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func (s *SQLiteMetaStore) EnsureTimeline(ctx context.Context, tl model.Timeline, head, base model.OID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one string
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM timelines WHERE repo=? AND anchor=? AND name=?",
		tl.Repo, string(tl.Anchor), tl.Name).Scan(&one)
	if err == nil {
		return nil // already exists (incl. locally committed): don't overwrite
	}
	if err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO timelines(repo, anchor, name, head, base) VALUES(?, ?, ?, ?, ?)",
		tl.Repo, string(tl.Anchor), tl.Name, string(head), string(base)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteMetaStore) ListTimelines(ctx context.Context, repo string, anchor model.AnchorPath) ([]model.Timeline, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT anchor, name FROM timelines WHERE repo=? AND anchor=? ORDER BY name",
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

func (s *SQLiteMetaStore) ListAllTimelines(ctx context.Context, repo string) ([]model.Timeline, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT anchor, name FROM timelines WHERE repo=? ORDER BY anchor, name", repo)
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
