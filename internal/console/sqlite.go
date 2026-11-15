package console

// The console's own sqlite store (docs/web/console.md §3). Data access goes
// through GORM mounted on the pool Open created (meta.GormOpen — the same
// mount discipline as the metadata plane and the web tier: one pool, raw
// schema DDL, every data operation on the ORM). Deliberately portable SQL —
// the HA-2 graduation path is Postgres, so no sqlite-specific behavior
// beyond the driver pragmas.
import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite" // pure-Go SQLite driver (modernc engine), no cgo
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/banyan-vcs/banyan/internal/ddl"
	"github.com/banyan-vcs/banyan/internal/meta"
)

// DB is the console's database handle: db holds the pool (schema DDL,
// Close), orm carries the data operations.
type DB struct {
	db  *sql.DB
	orm *gorm.DB
}

// --- row models (mirror of schemaV1; table names are the historical ones) ---

type userRow struct {
	ID        int64 `gorm:"column:id;primaryKey"`
	Username  string
	Admin     int
	CreatedAt int64 `gorm:"column:created_at"`
}

func (userRow) TableName() string { return "users" }

func (r userRow) user() User {
	return User{ID: r.ID, Username: r.Username, Admin: r.Admin != 0, CreatedAt: r.CreatedAt}
}

type tokenRow struct {
	ID        int64 `gorm:"column:id;primaryKey"`
	UserID    int64 `gorm:"column:user_id"`
	Name      string
	TokenHash string `gorm:"column:token_hash"`
	ExpiresAt int64  `gorm:"column:expires_at"`
	Revoked   int
	CreatedAt int64 `gorm:"column:created_at"`
}

func (tokenRow) TableName() string { return "tokens" }

type nodeRow struct {
	ID         int64 `gorm:"column:id;primaryKey"`
	Kind       Kind
	ParentID   int64  `gorm:"column:parent_id"`
	Segment    string
	RepoUID    string `gorm:"column:repo_uid"`
	Visibility string
	Protected  string
	Archived   int
	CreatedAt  int64 `gorm:"column:created_at"`
}

func (nodeRow) TableName() string { return "nodes" }

func (r nodeRow) node() Node {
	return Node{ID: r.ID, Kind: r.Kind, ParentID: r.ParentID, Segment: r.Segment,
		RepoUID: r.RepoUID, Visibility: r.Visibility, Protected: r.Protected,
		Archived: r.Archived != 0, CreatedAt: r.CreatedAt}
}

type memberRow struct {
	NodeID    int64 `gorm:"column:node_id;primaryKey"`
	UserID    int64 `gorm:"column:user_id;primaryKey"`
	Role      Role
	CreatedAt int64 `gorm:"column:created_at"`
}

func (memberRow) TableName() string { return "members" }

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

// consoleSteps is the console's migration history (ledger:
// console_migrations) — schemaV1 is the shipped shape, kept verbatim; the
// same replayable-steps posture as the metadata plane and the web tier.
var consoleSteps = []ddl.Step{
	{Name: "0001-console-tables", Up: func(q ddl.Q) error {
		return q.Exec(schemaV1)
	}},
}

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
	orm, err := meta.GormOpen("sqlite", db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("console db: gorm: %w", err)
	}
	if err := ddl.New(orm, "console_migrations").Baseline(func(q ddl.Q) error {
		return q.Exec(schemaV1) // schemaV1 carries tables AND its indexes
	}, "users").Run(consoleSteps); err != nil {
		db.Close()
		return nil, fmt.Errorf("console db: schema: %w", err)
	}
	return &DB{db: db, orm: orm}, nil
}

func (d *DB) Close() error { return d.db.Close() }

// --- users ---

func (d *DB) CreateUser(ctx context.Context, username string, admin bool) (User, error) {
	if !ValidUsername(username) {
		return User{}, fmt.Errorf("%w: username charset is [A-Za-z0-9_-], 1..32 chars", ErrInvalid)
	}
	row := userRow{Username: username, Admin: boolInt(admin), CreatedAt: time.Now().Unix()}
	if err := d.orm.WithContext(ctx).Create(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return User{}, fmt.Errorf("%w: users.username %q", ErrExists, username)
		}
		return User{}, err
	}
	return row.user(), nil
}

func (d *DB) UserByName(ctx context.Context, username string) (User, error) {
	var row userRow
	err := d.orm.WithContext(ctx).Where("username = ?", username).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return row.user(), nil
}

func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	var rows []userRow
	if err := d.orm.WithContext(ctx).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]User, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.user())
	}
	return out, nil
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
	row := tokenRow{UserID: userID, Name: name, TokenHash: tokenHash, ExpiresAt: expiresAt, CreatedAt: time.Now().Unix()}
	if err := d.orm.WithContext(ctx).Create(&row).Error; err != nil {
		return Token{}, err
	}
	return Token{ID: row.ID, UserID: userID, Name: name, TokenHash: tokenHash,
		ExpiresAt: expiresAt, CreatedAt: row.CreatedAt}, nil
}

// LookupToken resolves a token hash to its (live) user. Revoked or expired
// tokens are not found — the caller treats that as anonymous.
func (d *DB) LookupToken(ctx context.Context, tokenHash string) (User, error) {
	// The join is a projection of users through a live token — one row, read
	// through the ORM's rebinding with classic row semantics.
	row := d.orm.WithContext(ctx).Raw(`
		SELECT u.id, u.username, u.admin, u.created_at
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND t.revoked = 0
		  AND (t.expires_at = 0 OR t.expires_at > ?)`, tokenHash, time.Now().Unix()).Row()
	var u userRow
	switch err := row.Scan(&u.ID, &u.Username, &u.Admin, &u.CreatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return User{}, ErrNotFound
	case err != nil:
		return User{}, err
	}
	return u.user(), nil
}

func (d *DB) RevokeToken(ctx context.Context, id, userID int64) error {
	return d.orm.WithContext(ctx).Model(&tokenRow{}).
		Where("id = ? AND user_id = ?", id, userID).Update("revoked", 1).Error
}

func (d *DB) ListTokens(ctx context.Context, userID int64) ([]Token, error) {
	var rows []tokenRow
	if err := d.orm.WithContext(ctx).Where("user_id = ?", userID).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(rows))
	for _, r := range rows {
		out = append(out, Token{ID: r.ID, UserID: userID, Name: r.Name,
			ExpiresAt: r.ExpiresAt, Revoked: r.Revoked != 0, CreatedAt: r.CreatedAt})
	}
	return out, nil
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
	row := nodeRow{Kind: kind, ParentID: parent, Segment: segment, RepoUID: repoUID,
		Visibility: vis, Protected: protected, CreatedAt: time.Now().Unix()}
	if err := d.orm.WithContext(ctx).Create(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return Node{}, fmt.Errorf("%w: %s %q", ErrExists, string(kind), segment)
		}
		return Node{}, err
	}
	return row.node(), nil
}

func (d *DB) nodeBy(ctx context.Context, where string, args ...any) (Node, error) {
	var row nodeRow
	err := d.orm.WithContext(ctx).Where(where, args...).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}
	return row.node(), nil
}

func (d *DB) GroupByName(ctx context.Context, name string) (Node, error) {
	return d.nodeBy(ctx, "kind = ? AND parent_id = 0 AND segment = ?", "group", name)
}

func (d *DB) ProjectByNames(ctx context.Context, group, project string) (Node, Node, error) {
	g, err := d.GroupByName(ctx, group)
	if err != nil {
		return Node{}, Node{}, err
	}
	n, err := d.nodeBy(ctx, "kind = ? AND parent_id = ? AND segment = ?", "project", g.ID, project)
	return g, n, err
}

// ProjectDirs lists the project's registered subprojects (all depths; dirs
// carry their full repo-relative path in Segment).
func (d *DB) ProjectDirs(ctx context.Context, projectID int64) ([]Node, error) {
	return d.findNodes(ctx, "segment", "kind = ? AND parent_id = ? AND archived = 0", "dir", projectID)
}

func (d *DB) ListGroups(ctx context.Context) ([]Node, error) {
	return d.findNodes(ctx, "id", "kind = ? AND archived = 0", "group")
}

func (d *DB) ListProjects(ctx context.Context) ([]Node, error) {
	return d.findNodes(ctx, "id", "kind = ? AND archived = 0", "project")
}

// findNodes is the list form: filtered rows, ordered, mapped to Node.
func (d *DB) findNodes(ctx context.Context, order, where string, args ...any) ([]Node, error) {
	var rows []nodeRow
	if err := d.orm.WithContext(ctx).Where(where, args...).Order(order).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.node())
	}
	return out, nil
}

// updateNode is the single-column update with the unique-violation mapping
// a rename onto a live sibling would trip.
func (d *DB) updateNode(ctx context.Context, id int64, column string, value any) error {
	err := d.orm.WithContext(ctx).Model(&nodeRow{}).Where("id = ?", id).Update(column, value).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrExists
	}
	return err
}

func (d *DB) RenameNode(ctx context.Context, id int64, segment string) error {
	if !ValidSegment(segment) {
		return fmt.Errorf("%w: bad name %q", ErrInvalid, segment)
	}
	return d.updateNode(ctx, id, "segment", segment)
}

// SetVisibility flips a project between private and public.
func (d *DB) SetVisibility(ctx context.Context, id int64, vis string) error {
	if vis != Private && vis != Public {
		return fmt.Errorf("%w: visibility %q", ErrInvalid, vis)
	}
	return d.updateNode(ctx, id, "visibility", vis)
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
	return d.updateNode(ctx, id, "protected", string(raw))
}

// ProtectedBranches decodes the project's protected list.
func (n *Node) ProtectedBranches() []string {
	var out []string
	_ = json.Unmarshal([]byte(n.Protected), &out)
	return out
}

// Archive soft-deletes a project (and its subprojects): routing stops
// resolving it, the physical repo is left untouched (docs/web/console.md §6).
func (d *DB) ArchiveProject(ctx context.Context, id int64) error {
	return d.orm.WithContext(ctx).Model(&nodeRow{}).
		Where("id = ? OR (kind = ? AND parent_id = ?)", id, "dir", id).
		Update("archived", 1).Error
}

// --- members ---

func (d *DB) SetMember(ctx context.Context, nodeID, userID int64, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("%w: unknown role %q", ErrInvalid, role)
	}
	// Upsert by (node_id, user_id): a role change re-roles the membership.
	return d.orm.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "node_id"}, {Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"role"}),
	}).Create(&memberRow{NodeID: nodeID, UserID: userID, Role: role, CreatedAt: time.Now().Unix()}).Error
}

func (d *DB) RemoveMember(ctx context.Context, nodeID, userID int64) error {
	return d.orm.WithContext(ctx).
		Where("node_id = ? AND user_id = ?", nodeID, userID).Delete(&memberRow{}).Error
}

// MembersOf lists the direct members of one node (management view).
func (d *DB) MembersOf(ctx context.Context, nodeID int64) ([]Member, error) {
	var rows []struct {
		NodeID    int64
		UserID    int64
		Username  string
		Role      Role
		CreatedAt int64 `gorm:"column:created_at"`
	}
	err := d.orm.WithContext(ctx).Raw(`
		SELECT m.node_id, m.user_id, u.username, m.role, m.created_at
		FROM members m JOIN users u ON u.id = m.user_id
		WHERE m.node_id = ? ORDER BY u.username`, nodeID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(rows))
	for _, r := range rows {
		out = append(out, Member{NodeID: r.NodeID, UserID: r.UserID, Username: r.Username, Role: r.Role, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// RolesAt returns role-by-node for one user over the given node ids (one
// round trip; ids come from the chain resolver).
func (d *DB) RolesAt(ctx context.Context, userID int64, nodeIDs []int64) (map[int64]Role, error) {
	if len(nodeIDs) == 0 {
		return map[int64]Role{}, nil
	}
	var rows []memberRow
	if err := d.orm.WithContext(ctx).
		Where("user_id = ? AND node_id IN ?", userID, nodeIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int64]Role, len(rows))
	for _, r := range rows {
		out[r.NodeID] = r.Role
	}
	return out, nil
}

// --- small helpers ---

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
