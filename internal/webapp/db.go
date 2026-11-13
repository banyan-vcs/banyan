// db.go — the web tier's own state. sqlite / mysql / postgres, one schema,
// every table prefixed web_ so the database may be SHARED with the
// gateway's metadata plane (the demo deployment points both at the same
// sqlite file; WAL plus busy_timeout plus immediate transactions — the
// pragma set meta.SQLiteDSN pins — are the multi-process contract).
//
// The schema is deliberately portable SQL: ? placeholders everywhere (the
// postgres driver path rebinds to $N), no dialect-specific types. Migration
// posture matches the rest of banyan: CREATE TABLE IF NOT EXISTS at boot.
package webapp

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

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
	db     *sql.DB
	rebind func(string) string
}

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
		rebind = func(q string) string { return q }
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
		driver, dsn, rebind = "pgx", target, rebindPG
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
	w := &DB{db: db, rebind: rebind}
	if err := w.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}

func (w *DB) Close() error { return w.db.Close() }

func (w *DB) q(query string) string { return w.rebind(query) }

// rebindPG rewrites ? placeholders to $N outside single-quoted strings —
// the same walk the metadata plane performs.
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

func (w *DB) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS web_sessions (
  id            VARCHAR(64)  NOT NULL PRIMARY KEY,
  username      VARCHAR(190) NOT NULL,
  credential    VARCHAR(1024) NOT NULL,
  provider_id   VARCHAR(64)  NOT NULL DEFAULT '',
  provider_kind VARCHAR(16)  NOT NULL DEFAULT '',
  created_at    BIGINT       NOT NULL,
  expires_at    BIGINT       NOT NULL
);
CREATE TABLE IF NOT EXISTS web_oauth_states (
  state       VARCHAR(64) NOT NULL PRIMARY KEY,
  provider_id VARCHAR(64) NOT NULL,
  expires_at  BIGINT      NOT NULL
);
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
);`
	_, err := w.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("web db schema: %w", err)
	}
	return nil
}

func unix(t time.Time) int64 { return t.Unix() }

// --- sessions ---

// CreateSession stores a new session row.
func (w *DB) CreateSession(s Session, ttl time.Duration) error {
	now := unix(time.Now())
	_, err := w.db.Exec(w.q(`INSERT INTO web_sessions
		(id, username, credential, provider_id, provider_kind, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?)`),
		s.ID, s.Username, s.Credential, s.ProviderID, s.ProviderKind, now, now+int64(ttl.Seconds()))
	return err
}

// SessionByID answers the session when present and unexpired; expired rows
// are removed on sight (lazy sweep — plus SweepExpired on boot).
func (w *DB) SessionByID(id string) (Session, bool) {
	var s Session
	var created, expires int64
	err := w.db.QueryRow(w.q(`SELECT id, username, credential, provider_id, provider_kind, created_at, expires_at
		FROM web_sessions WHERE id=?`), id).
		Scan(&s.ID, &s.Username, &s.Credential, &s.ProviderID, &s.ProviderKind, &created, &expires)
	if err != nil {
		return Session{}, false
	}
	if expires < unix(time.Now()) {
		_, _ = w.db.Exec(w.q(`DELETE FROM web_sessions WHERE id=?`), id)
		return Session{}, false
	}
	return s, true
}

// DeleteSession removes one session (logout).
func (w *DB) DeleteSession(id string) error {
	_, err := w.db.Exec(w.q(`DELETE FROM web_sessions WHERE id=?`), id)
	return err
}

// SweepExpired drops every expired session row.
func (w *DB) SweepExpired() error {
	_, err := w.db.Exec(w.q(`DELETE FROM web_sessions WHERE expires_at < ?`), unix(time.Now()))
	return err
}

// --- oauth states (anti-CSRF; DB-backed so any instance of the web tier
// can answer the callback) ---

// PutOAuthState records a state for one provider with an expiry.
func (w *DB) PutOAuthState(state, providerID string, ttl time.Duration) error {
	_, err := w.db.Exec(w.q(`INSERT INTO web_oauth_states (state, provider_id, expires_at) VALUES (?,?,?)`),
		state, providerID, unix(time.Now())+int64(ttl.Seconds()))
	return err
}

// TakeOAuthState consumes a state (single use); "" when absent or expired.
func (w *DB) TakeOAuthState(state string) string {
	var providerID string
	var expires int64
	err := w.db.QueryRow(w.q(`SELECT provider_id, expires_at FROM web_oauth_states WHERE state=?`), state).
		Scan(&providerID, &expires)
	if err != nil || expires < unix(time.Now()) {
		return ""
	}
	_, _ = w.db.Exec(w.q(`DELETE FROM web_oauth_states WHERE state=?`), state)
	return providerID
}

// SweepOAuthStates drops expired states.
func (w *DB) SweepOAuthStates() error {
	_, err := w.db.Exec(w.q(`DELETE FROM web_oauth_states WHERE expires_at < ?`), unix(time.Now()))
	return err
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
	_, err := w.db.Exec(w.q(`INSERT INTO web_imports
		(id, repo, source_url, initiator, status, error, refs, default_branch, started_at, finished_at)
		VALUES (?,?,?,?,?,'',0,'',?,0)`),
		j.ID, j.Repo, j.SourceURL, j.Initiator, "running", j.StartedAt)
	return err
}

// FinishImport closes a job either way.
func (w *DB) FinishImport(id, status, errMsg string, refs int64, defaultBranch string) error {
	_, err := w.db.Exec(w.q(`UPDATE web_imports
		SET status=?, error=?, refs=?, default_branch=?, finished_at=? WHERE id=?`),
		status, errMsg, refs, defaultBranch, unix(time.Now()), id)
	return err
}

// ListImports answers the initiator's jobs, newest first.
func (w *DB) ListImports(initiator string) ([]ImportJob, error) {
	rows, err := w.db.Query(w.q(`SELECT id, repo, source_url, initiator, status, error, refs, default_branch, started_at, finished_at
		FROM web_imports WHERE initiator=? ORDER BY started_at DESC`), initiator)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportJob
	for rows.Next() {
		var j ImportJob
		if err := rows.Scan(&j.ID, &j.Repo, &j.SourceURL, &j.Initiator, &j.Status, &j.Error,
			&j.Refs, &j.DefaultBranch, &j.StartedAt, &j.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if out == nil {
		out = []ImportJob{}
	}
	return out, nil
}

// FailInterruptedImports marks every still-running job as failed — called
// at boot; a restart kills the goroutines those rows belonged to.
func (w *DB) FailInterruptedImports() error {
	_, err := w.db.Exec(w.q(`UPDATE web_imports
		SET status='failed', error='interrupted by a web-tier restart', finished_at=? WHERE status='running'`),
		unix(time.Now()))
	return err
}
