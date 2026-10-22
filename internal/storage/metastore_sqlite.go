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
// (docs/02 §2.3). WAL mode + busy_timeout keeps readers running during the
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
	repo     TEXT NOT NULL,
	oid      TEXT NOT NULL,
	pack_oid TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	offset   INTEGER NOT NULL DEFAULT 0, -- reserved: M3 fill, saves one idx roundtrip
	PRIMARY KEY(repo, oid)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_objects_pack ON objects(repo, pack_oid);
`

// sqliteDSN builds the connection string. Pragmas must ride the DSN:
// database/sql pools plain connections, and a PRAGMA issued via Exec applies
// to one pooled connection only. WAL keeps readers running while the
// single-writer CAS holds the write lock (docs/02 §2); busy_timeout bounds
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
	return &SQLiteMetaStore{db: db}, nil
}

func (s *SQLiteMetaStore) Close() error { return s.db.Close() }

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *SQLiteMetaStore) CreateRepo(ctx context.Context, repo string, algo model.HashAlgo) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO repos(name, algo, default_branch) VALUES(?, ?, 'main')", repo, int(algo))
	if isUniqueViolation(err) {
		return fmt.Errorf("repo %s already exists", repo)
	}
	return err
}

func (s *SQLiteMetaStore) GetRepo(ctx context.Context, repo string) (RepoInfo, error) {
	var info RepoInfo
	var algo int
	err := s.db.QueryRowContext(ctx,
		"SELECT name, algo, default_branch FROM repos WHERE name=?", repo).
		Scan(&info.ID, &algo, &info.DefaultBranch)
	if err == sql.ErrNoRows {
		return RepoInfo{}, fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	if err != nil {
		return RepoInfo{}, err
	}
	info.Algo = model.HashAlgo(algo)
	return info, nil
}

func (s *SQLiteMetaStore) ListRepos(ctx context.Context) ([]RepoInfo, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, algo, default_branch FROM repos ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoInfo
	for rows.Next() {
		var info RepoInfo
		var algo int
		if err := rows.Scan(&info.ID, &algo, &info.DefaultBranch); err != nil {
			return nil, err
		}
		info.Algo = model.HashAlgo(algo)
		out = append(out, info)
	}
	return out, rows.Err()
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
