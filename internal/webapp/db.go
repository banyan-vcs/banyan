// db.go — the web tier's own state. sqlite / mysql / postgres, one schema,
// every table prefixed web_ so the database may be SHARED with the
// gateway's metadata plane (the demo deployment points both at the same
// sqlite file; WAL plus busy_timeout plus immediate transactions — the
// pragma set meta.SQLiteDSN pins — are the multi-process contract).
//
// Data access goes through GORM mounted on the same pool the caller opens
// (meta.GormOpen — the metadata plane's mount discipline: one pool, schema
// management raw, every data operation on the ORM). The schema is
// deliberately portable SQL, CREATE TABLE IF NOT EXISTS at boot.
package webapp

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite" // pure-Go SQLite driver (modernc engine), no cgo
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/gorm"

	"github.com/banyan-vcs/banyan/internal/ddl"
	"github.com/banyan-vcs/banyan/internal/meta"
)

// Session is one logged-in browser. Credential is the gateway-facing
// secret (a local password or a provider OAuth token) stored in the same
// demo posture as the static policy's plaintext YAML — an at-rest-encrypted
// column is the registered follow-up (docs/web/web-layer.md §1).
type Session struct {
	ID            string
	Username      string
	Credential    string
	ProviderID    string // "" = local login
	ProviderKind  string // "" | gitlab | github
}

// DB is the web tier's database handle.
type DB struct {
	db  *sql.DB   // the shared pool: schema DDL + Close
	orm *gorm.DB  // every data operation (same mount as the metadata plane)
}

// --- row models (mirror of the DDL below; table names are the historical ones) ---

type webSessionRow struct {
	ID            string `gorm:"column:id;primaryKey"`
	Username      string `gorm:"column:username"`
	Credential    string `gorm:"column:credential"`
	ProviderID    string `gorm:"column:provider_id"`
	ProviderKind  string `gorm:"column:provider_kind"`
	CreatedAt     int64  `gorm:"column:created_at"`
	ExpiresAt     int64  `gorm:"column:expires_at"`
}

func (webSessionRow) TableName() string { return "web_sessions" }

func (r webSessionRow) session() Session {
	return Session{ID: r.ID, Username: r.Username, Credential: r.Credential, ProviderID: r.ProviderID, ProviderKind: r.ProviderKind}
}

type webOAuthStateRow struct {
	State      string `gorm:"column:state;primaryKey"`
	ProviderID string `gorm:"column:provider_id"`
	ExpiresAt  int64  `gorm:"column:expires_at"`
}

func (webOAuthStateRow) TableName() string { return "web_oauth_states" }

type webImportRow struct {
	ID            string `gorm:"column:id;primaryKey"`
	Repo          string `gorm:"column:repo"`
	SourceURL     string `gorm:"column:source_url"`
	Initiator     string `gorm:"column:initiator"`
	Status        string `gorm:"column:status"`
	Error         string `gorm:"column:error"`
	Refs          int64  `gorm:"column:refs"`
	DefaultBranch string `gorm:"column:default_branch"`
	StartedAt     int64  `gorm:"column:started_at"`
	FinishedAt    int64  `gorm:"column:finished_at"`
}

func (webImportRow) TableName() string { return "web_imports" }

func (r webImportRow) job() ImportJob {
	return ImportJob{ID: r.ID, Repo: r.Repo, SourceURL: r.SourceURL, Initiator: r.Initiator,
		Status: r.Status, Error: r.Error, Refs: r.Refs, DefaultBranch: r.DefaultBranch,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
}

type webCLIGrantRow struct {
	HandshakeID  string `gorm:"column:handshake_id;primaryKey"`
	Token        string `gorm:"column:token"`
	UserName     string `gorm:"column:user_name"`
	TokenExpires int64  `gorm:"column:token_expires"`
	ExpiresAt    int64  `gorm:"column:expires_at"`
}

func (webCLIGrantRow) TableName() string { return "web_cli_grants" }

// OpenDB opens the web database. spec forms: "sqlite:<path>", "mysql:<dsn>",
// "postgres:<dsn>"; a bare path is sqlite (the <root>/web.db default).
func OpenDB(spec string) (*DB, error) {
	backend, target := "sqlite", spec
	if i := strings.IndexByte(spec, ':'); i > 0 && !strings.ContainsRune(spec[:i], '/') && !strings.ContainsRune(spec[:i], '\\') {
		switch spec[:i] {
		case "sqlite", "mysql", "postgres":
			backend, target = spec[:i], spec[i+1:]
		}
	}
	var (
		driver string
		dsn    string
	)
	switch backend {
	case "sqlite":
		driver = "sqlite"
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("web db: %w", err)
		}
		dsn = meta.SQLiteDSN(target)
	case "mysql":
		driver, dsn = "mysql", target
	case "postgres":
		driver, dsn = "pgx", target
	default:
		return nil, fmt.Errorf("web db: unknown backend %q", backend)
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("web db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("web db: %w", err)
	}
	orm, err := meta.GormOpen(backend, db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("web db: gorm: %w", err)
	}
	w := &DB{db: db, orm: orm}
	if err := w.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}

func (w *DB) Close() error { return w.db.Close() }

// webSteps is the web tier's migration history (ledger: web_migrations —
// its own table because this database may be the SAME file as the
// metadata plane's). Same posture as the metadata plane: replayable steps,
// fresh databases get the latest shape in 0001.
// webTablesUp is step 0001 (and the table half of the baseline snapshot).
func webTablesUp(q ddl.Q) error {
	for _, stmt := range webTableDDL() {
		if err := q.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// webImportsIdxUp is step 0002 (the index half of the baseline snapshot) —
// ListImports filters initiator and sorts started_at DESC.
func webImportsIdxUp(q ddl.Q) error {
	return q.EnsureIndex("idx_web_imports_initiator",
		"CREATE INDEX idx_web_imports_initiator ON web_imports(initiator, started_at)")
}

// webBaseline is the authoritative full shape for FRESH databases — the
// step closures shared, so the two tracks cannot drift.
func webBaseline(q ddl.Q) error {
	if err := webTablesUp(q); err != nil {
		return err
	}
	return webImportsIdxUp(q)
}

// webSteps is the web tier's migration history (ledger: web_migrations —
// its own table because this database may be the SAME file as the
// metadata plane's). Same posture as the metadata plane: replayable steps,
// fresh databases take the baseline snapshot instead.
var webSteps = []ddl.Step{
	{Name: "0001-web-tables", Up: webTablesUp},
	{Name: "0002-imports-initiator-idx", Up: webImportsIdxUp},
}

func webTableDDL() []string {
	return []string{`
CREATE TABLE IF NOT EXISTS web_sessions (
  id            VARCHAR(64)  NOT NULL PRIMARY KEY,
  username      VARCHAR(190) NOT NULL,
  credential    VARCHAR(1024) NOT NULL,
  provider_id   VARCHAR(64)  NOT NULL DEFAULT '',
  provider_kind VARCHAR(16)  NOT NULL DEFAULT '',
  created_at    BIGINT       NOT NULL,
  expires_at    BIGINT       NOT NULL
)`, `
CREATE TABLE IF NOT EXISTS web_oauth_states (
  state       VARCHAR(64) NOT NULL PRIMARY KEY,
  provider_id VARCHAR(64) NOT NULL,
  expires_at  BIGINT      NOT NULL
)`, `
CREATE TABLE IF NOT EXISTS web_imports (
  id             VARCHAR(32)   NOT NULL PRIMARY KEY,
  repo           VARCHAR(190)  NOT NULL,
  source_url     VARCHAR(1024) NOT NULL,
  initiator      VARCHAR(190)  NOT NULL,
  status         VARCHAR(16)   NOT NULL,
  error          VARCHAR(2048) NOT NULL DEFAULT '',
  refs           BIGINT        NOT NULL DEFAULT 0,
  default_branch VARCHAR(190)  NOT NULL DEFAULT '',
  started_at     BIGINT        NOT NULL,
  finished_at    BIGINT        NOT NULL DEFAULT 0
)`, `
CREATE TABLE IF NOT EXISTS web_cli_grants (
  handshake_id VARCHAR(64)  NOT NULL PRIMARY KEY,
  token        VARCHAR(128) NOT NULL,
  user_name    VARCHAR(190) NOT NULL,
  token_expires BIGINT      NOT NULL DEFAULT 0,
  expires_at   BIGINT       NOT NULL
)`}
}

func (w *DB) migrate() error {
	if err := ddl.New(w.orm, "web_migrations").Baseline(webBaseline, "web_sessions").Run(webSteps); err != nil {
		return fmt.Errorf("web db schema: %w", err)
	}
	return nil
}

func unix(t time.Time) int64 { return t.Unix() }

// --- sessions ---

// CreateSession stores a new session row.
func (w *DB) CreateSession(s Session, ttl time.Duration) error {
	now := unix(time.Now())
	return w.orm.Create(&webSessionRow{
		ID: s.ID, Username: s.Username, Credential: s.Credential,
		ProviderID: s.ProviderID, ProviderKind: s.ProviderKind,
		CreatedAt: now, ExpiresAt: now + int64(ttl.Seconds()),
	}).Error
}

// SessionByID answers the session when present and unexpired; expired rows
// are removed on sight (lazy sweep — plus SweepExpired on boot).
func (w *DB) SessionByID(id string) (Session, bool) {
	var row webSessionRow
	if err := w.orm.Where("id = ?", id).Take(&row).Error; err != nil {
		return Session{}, false
	}
	if row.ExpiresAt < unix(time.Now()) {
		_ = w.orm.Where("id = ?", id).Delete(&webSessionRow{}).Error
		return Session{}, false
	}
	return row.session(), true
}

// DeleteSession removes one session (logout).
func (w *DB) DeleteSession(id string) error {
	return w.orm.Where("id = ?", id).Delete(&webSessionRow{}).Error
}

// SweepExpired drops every expired session row.
func (w *DB) SweepExpired() error {
	return w.orm.Where("expires_at < ?", unix(time.Now())).Delete(&webSessionRow{}).Error
}

// --- oauth states (anti-CSRF; DB-backed so any instance of the web tier
// can answer the callback) ---

// PutOAuthState records a state for one provider with an expiry.
func (w *DB) PutOAuthState(state, providerID string, ttl time.Duration) error {
	return w.orm.Create(&webOAuthStateRow{
		State: state, ProviderID: providerID, ExpiresAt: unix(time.Now()) + int64(ttl.Seconds()),
	}).Error
}

// TakeOAuthState consumes a state (single use); "" when absent or expired.
func (w *DB) TakeOAuthState(state string) string {
	var row webOAuthStateRow
	if err := w.orm.Where("state = ?", state).Take(&row).Error; err != nil || row.ExpiresAt < unix(time.Now()) {
		_ = w.orm.Where("state = ?", state).Delete(&webOAuthStateRow{}).Error
		return ""
	}
	_ = w.orm.Where("state = ?", state).Delete(&webOAuthStateRow{}).Error
	return row.ProviderID
}

// SweepOAuthStates drops expired states.
func (w *DB) SweepOAuthStates() error {
	return w.orm.Where("expires_at < ?", unix(time.Now())).Delete(&webOAuthStateRow{}).Error
}

// Basic returns the gateway-facing Authorization value for the session:
// the local login's user:password, or git's token conventions for provider
// sessions (oauth2:<token> for GitLab, x-access-token:<token> for GitHub).
func (s Session) Basic() string {
	switch s.ProviderKind {
	case "github":
		return "Basic " + base64Of("x-access-token:" + s.Credential)
	case "gitlab":
		return "Basic " + base64Of("oauth2:" + s.Credential)
	default:
		return "Basic " + base64Of(s.Username+":"+s.Credential)
	}
}

// --- imports (job history; the gateway face itself stays stateless) ---

// ImportJob is one import's record. The source credential is deliberately
// NOT here — it exists only inside the running call.
type ImportJob struct {
	ID            string `json:"id"`
	Repo          string `json:"repo"`
	SourceURL     string `json:"sourceUrl"`
	Initiator     string `json:"initiator"`
	Status        string `json:"status"` // running | succeeded | failed
	Error         string `json:"error,omitempty"`
	Refs          int64  `json:"refs"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	StartedAt     int64  `json:"startedAt"`
	FinishedAt    int64  `json:"finishedAt"`
}

// CreateImport records a job as running.
func (w *DB) CreateImport(j ImportJob) error {
	return w.orm.Create(&webImportRow{
		ID: j.ID, Repo: j.Repo, SourceURL: j.SourceURL, Initiator: j.Initiator,
		Status: "running", StartedAt: j.StartedAt,
	}).Error
}

// FinishImport closes a job either way.
func (w *DB) FinishImport(id, status, errMsg string, refs int64, defaultBranch string) error {
	return w.orm.Model(&webImportRow{}).Where("id = ?", id).Updates(map[string]any{
		"status": status, "error": errMsg, "refs": refs,
		"default_branch": defaultBranch, "finished_at": unix(time.Now()),
	}).Error
}

// ListImports answers the initiator's jobs, newest first.
func (w *DB) ListImports(initiator string) ([]ImportJob, error) {
	var rows []webImportRow
	if err := w.orm.Where("initiator = ?", initiator).Order("started_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ImportJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.job())
	}
	return out, nil
}

// FailInterruptedImports marks every still-running job as failed — called
// at boot; a restart kills the goroutines those rows belonged to.
func (w *DB) FailInterruptedImports() error {
	return w.orm.Model(&webImportRow{}).
		Where("status = ?", "running").
		Updates(map[string]any{
			"status": "failed", "error": "interrupted by a web-tier restart", "finished_at": unix(time.Now()),
		}).Error
}
