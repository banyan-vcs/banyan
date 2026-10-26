package console

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo (AGENTS.md)
)

// DB is the console's own sqlite store (docs/17 §3). It is deliberately
// portable SQL — the HA-2 graduation path is Postgres, so no
// sqlite-specific behavior beyond the driver pragmas.
type DB struct {
	sql *sql.DB
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS users (
  id         INTEGER PRIMARY KEY,
  username   TEXT NOT NULL UNIQUE,
  admin      INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL,
  name       TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  expires_at INTEGER NOT NULL DEFAULT 0,
  revoked    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tokens_user ON tokens(user_id);
CREATE TABLE IF NOT EXISTS nodes (
  id          INTEGER PRIMARY KEY,
  kind        TEXT NOT NULL,                -- 'group' | 'project' | 'dir'
  parent_id   INTEGER NOT NULL,             -- 0=groups; group id; project id
  segment     TEXT NOT NULL,                -- name; dirs carry the repo-relative path
  repo_uid    TEXT NOT NULL DEFAULT '',     -- projects only
  visibility  TEXT NOT NULL DEFAULT 'private',
  protected   TEXT NOT NULL DEFAULT '["main"]',
  archived    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_identity ON nodes(kind, parent_id, segment);
CREATE INDEX IF NOT EXISTS idx_nodes_parent ON nodes(parent_id);
CREATE TABLE IF NOT EXISTS members (
  node_id    INTEGER NOT NULL,
  user_id    INTEGER NOT NULL,
  role       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (node_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_members_user ON members(user_id);
`

// Open creates or opens the console database at path.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("console db: mkdir %s: %w", dir, err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("console db: open: %w", err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		db.Close()
		return nil, fmt.Errorf("console db: schema: %w", err)
	}
	if _, err := db.Exec(reviewSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("console db: review schema: %w", err)
	}
	return &DB{sql: db}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// --- users ---

func (d *DB) CreateUser(ctx context.Context, username string, admin bool) (User, error) {
	if !ValidUsername(username) {
		return User{}, fmt.Errorf("%w: username charset is [A-Za-z0-9_-], 1..32 chars", ErrInvalid)
	}
	u := User{Username: username, Admin: admin, CreatedAt: time.Now().Unix()}
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO users(username, admin, created_at) VALUES (?,?,?)`,
		username, boolInt(admin), u.CreatedAt)
	if err != nil {
		return User{}, mapDup(err, "users.username", username)
	}
	u.ID, _ = res.LastInsertId()
	return u, nil
}

func (d *DB) UserByName(ctx context.Context, username string) (User, error) {
	var u User
	var admin int
	err := d.sql.QueryRowContext(ctx,
		`SELECT id, username, admin, created_at FROM users WHERE username=?`, username).
		Scan(&u.ID, &u.Username, &admin, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Admin = admin != 0
	return u, nil
}

func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, username, admin, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var admin int
		if err := rows.Scan(&u.ID, &u.Username, &admin, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Admin = admin != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// --- tokens ---

// NewTokenSecret generates the plaintext half of a PAT ("banyan_" + 24
// random bytes, base64url). The caller sees it exactly once.
func NewTokenSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "banyan_" + hex.EncodeToString(b)
}

func (d *DB) CreateToken(ctx context.Context, userID int64, name, tokenHash string, expiresAt int64) (Token, error) {
	t := Token{UserID: userID, Name: name, TokenHash: tokenHash, ExpiresAt: expiresAt, CreatedAt: time.Now().Unix()}
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO tokens(user_id, name, token_hash, expires_at, created_at) VALUES (?,?,?,?,?)`,
		userID, name, tokenHash, expiresAt, t.CreatedAt)
	if err != nil {
		return Token{}, err
	}
	t.ID, _ = res.LastInsertId()
	return t, nil
}

// LookupToken resolves a token hash to its (live) user. Revoked or expired
// tokens are not found — the caller treats that as anonymous.
func (d *DB) LookupToken(ctx context.Context, tokenHash string) (User, error) {
	var u User
	var admin int
	err := d.sql.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.admin, u.created_at
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash=? AND t.revoked=0
		  AND (t.expires_at=0 OR t.expires_at>?)`,
		tokenHash, time.Now().Unix()).
		Scan(&u.ID, &u.Username, &admin, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Admin = admin != 0
	return u, nil
}

func (d *DB) RevokeToken(ctx context.Context, id, userID int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE tokens SET revoked=1 WHERE id=? AND user_id=?`, id, userID)
	return err
}

func (d *DB) ListTokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, name, expires_at, revoked, created_at FROM tokens WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var revoked int
		if err := rows.Scan(&t.ID, &t.Name, &t.ExpiresAt, &revoked, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Revoked = revoked != 0
		t.UserID = userID
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- nodes ---

// NewRepoUID mints the immutable physical repo name for a project.
func NewRepoUID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "p_" + hex.EncodeToString(b)
}

func (d *DB) CreateGroup(ctx context.Context, name string) (Node, error) {
	if !ValidSegment(name) {
		return Node{}, fmt.Errorf("%w: bad group name %q", ErrInvalid, name)
	}
	return d.insertNode(ctx, KindGroup, 0, name, "", Private, `["main"]`)
}

func (d *DB) CreateProject(ctx context.Context, groupID int64, name, repoUID string) (Node, error) {
	if !ValidSegment(name) {
		return Node{}, fmt.Errorf("%w: bad project name %q", ErrInvalid, name)
	}
	if repoUID == "" {
		return Node{}, fmt.Errorf("%w: repo_uid required", ErrInvalid)
	}
	return d.insertNode(ctx, KindProject, groupID, name, repoUID, Private, `["main"]`)
}

func (d *DB) CreateSubproject(ctx context.Context, projectID int64, path string) (Node, error) {
	sub, err := CleanSub(path)
	if err != nil {
		return Node{}, err
	}
	if sub == "" {
		return Node{}, fmt.Errorf("%w: empty subproject path", ErrInvalid)
	}
	return d.insertNode(ctx, KindDir, projectID, sub, "", "", "")
}

func (d *DB) insertNode(ctx context.Context, kind Kind, parent int64, segment, repoUID, vis, protected string) (Node, error) {
	n := Node{Kind: kind, ParentID: parent, Segment: segment, RepoUID: repoUID,
		Visibility: vis, Protected: protected, CreatedAt: time.Now().Unix()}
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO nodes(kind, parent_id, segment, repo_uid, visibility, protected, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		string(kind), parent, segment, repoUID, vis, protected, n.CreatedAt)
	if err != nil {
		return Node{}, mapDup(err, string(kind), segment)
	}
	n.ID, _ = res.LastInsertId()
	return n, nil
}

func scanNode(scan func(...any) error) (Node, error) {
	var n Node
	var kind string
	var archived int
	if err := scan(&n.ID, &kind, &n.ParentID, &n.Segment, &n.RepoUID, &n.Visibility, &n.Protected, &archived, &n.CreatedAt); err != nil {
		return Node{}, err
	}
	n.Kind = Kind(kind)
	n.Archived = archived != 0
	return n, nil
}

const nodeCols = `id, kind, parent_id, segment, repo_uid, visibility, protected, archived, created_at`

func (d *DB) GroupByName(ctx context.Context, name string) (Node, error) {
	n, err := scanNode(func(dst ...any) error {
		return d.sql.QueryRowContext(ctx,
			`SELECT `+nodeCols+` FROM nodes WHERE kind='group' AND parent_id=0 AND segment=?`, name).
			Scan(dst...)
	})
	return n, normNodeErr(err)
}

func (d *DB) ProjectByNames(ctx context.Context, group, project string) (Node, Node, error) {
	g, err := d.GroupByName(ctx, group)
	if err != nil {
		return Node{}, Node{}, err
	}
	n, err := scanNode(func(dst ...any) error {
		return d.sql.QueryRowContext(ctx,
			`SELECT `+nodeCols+` FROM nodes WHERE kind='project' AND parent_id=? AND segment=?`, g.ID, project).
			Scan(dst...)
	})
	return g, n, normNodeErr(err)
}

// ProjectDirs lists the project's registered subprojects (all depths; dirs
// carry their full repo-relative path in Segment).
func (d *DB) ProjectDirs(ctx context.Context, projectID int64) ([]Node, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE kind='dir' AND parent_id=? AND archived=0 ORDER BY segment`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (d *DB) ListGroups(ctx context.Context) ([]Node, error) {
	return d.listNodes(ctx, `kind='group' AND archived=0`)
}

func (d *DB) ListProjects(ctx context.Context) ([]Node, error) {
	return d.listNodes(ctx, `kind='project' AND archived=0`)
}

func (d *DB) listNodes(ctx context.Context, where string) ([]Node, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE `+where+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (d *DB) RenameNode(ctx context.Context, id int64, segment string) error {
	if !ValidSegment(segment) {
		return fmt.Errorf("%w: bad name %q", ErrInvalid, segment)
	}
	return d.execNode(ctx, `UPDATE nodes SET segment=? WHERE id=?`, segment, id)
}

// SetVisibility flips a project between private and public.
func (d *DB) SetVisibility(ctx context.Context, id int64, vis string) error {
	if vis != Private && vis != Public {
		return fmt.Errorf("%w: visibility %q", ErrInvalid, vis)
	}
	return d.execNode(ctx, `UPDATE nodes SET visibility=? WHERE id=?`, vis, id)
}

// SetProtected stores the protected-branch list (JSON array).
func (d *DB) SetProtected(ctx context.Context, id int64, branches []string) error {
	for _, b := range branches {
		if b == "" || strings.ContainsAny(b, " \t\n") {
			return fmt.Errorf("%w: bad branch %q", ErrInvalid, b)
		}
	}
	raw, err := json.Marshal(branches)
	if err != nil {
		return err
	}
	return d.execNode(ctx, `UPDATE nodes SET protected=? WHERE id=?`, string(raw), id)
}

// ProtectedBranches decodes the project's protected list.
func (n *Node) ProtectedBranches() []string {
	var out []string
	_ = json.Unmarshal([]byte(n.Protected), &out)
	return out
}

// Archive soft-deletes a project (and its subprojects): routing stops
// resolving it, the physical repo is left untouched (docs/17 §6).
func (d *DB) ArchiveProject(ctx context.Context, id int64) error {
	if _, err := d.sql.ExecContext(ctx,
		`UPDATE nodes SET archived=1 WHERE id=? OR (kind='dir' AND parent_id=?)`, id, id); err != nil {
		return err
	}
	return nil
}

func (d *DB) execNode(ctx context.Context, q string, args ...any) error {
	_, err := d.sql.ExecContext(ctx, q, args...)
	if err != nil && err.Error() != "" && isDup(err) {
		return ErrExists
	}
	return err
}

// --- members ---

func (d *DB) SetMember(ctx context.Context, nodeID, userID int64, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("%w: unknown role %q", ErrInvalid, role)
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO members(node_id, user_id, role, created_at) VALUES (?,?,?,?)
		ON CONFLICT(node_id, user_id) DO UPDATE SET role=excluded.role`,
		nodeID, userID, string(role), time.Now().Unix())
	return err
}

func (d *DB) RemoveMember(ctx context.Context, nodeID, userID int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM members WHERE node_id=? AND user_id=?`, nodeID, userID)
	return err
}

// MembersOf lists the direct members of one node (management view).
func (d *DB) MembersOf(ctx context.Context, nodeID int64) ([]Member, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT m.node_id, m.user_id, u.username, m.role, m.created_at
		FROM members m JOIN users u ON u.id=m.user_id
		WHERE m.node_id=? ORDER BY u.username`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var role string
		if err := rows.Scan(&m.NodeID, &m.UserID, &m.Username, &role, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// RolesAt returns role-by-node for one user over the given node ids (one
// round trip; ids come from the chain resolver).
func (d *DB) RolesAt(ctx context.Context, userID int64, nodeIDs []int64) (map[int64]Role, error) {
	if len(nodeIDs) == 0 {
		return map[int64]Role{}, nil
	}
	q := `SELECT node_id, role FROM members WHERE user_id=? AND node_id IN (` +
		placeholders(len(nodeIDs)) + `)`
	args := make([]any, 0, len(nodeIDs)+1)
	args = append(args, userID)
	for _, id := range nodeIDs {
		args = append(args, id)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]Role, len(nodeIDs))
	for rows.Next() {
		var id int64
		var role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, err
		}
		out[id] = Role(role)
	}
	return out, rows.Err()
}

// --- small helpers ---

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func placeholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += "?"
	}
	return s
}

func normNodeErr(err error) error {
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	return err
}

func mapDup(err error, what, name string) error {
	if isDup(err) {
		return fmt.Errorf("%w: %s %q", ErrExists, what, name)
	}
	return err
}

func isDup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
