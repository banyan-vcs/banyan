// Personal access tokens in the metadata plane (docs/07 §6, docs/14 M5):
// the plaintext exists only at creation (returned once to the operator);
// the store keeps its sha256. Tokens are cluster state — any gateway with
// the metadata (all-in-one, refs-serving node) validates them; the gateway
// behind an upstream console uses the console's own PATs instead (docs/17).
// TokenRegistry is deliberately NOT part of the MetaStore composite: only
// the stores that own metadata implement it, and consumers type-assert
// (meta.(TokenRegistry)) so remote-meta gateways compile unchanged.
package meta

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
)

// Token scopes are cumulative ranks: read < write < admin. A read token may
// fetch; write adds push and server-side commits; admin adds the management
// RPCs (auth.AdminChecker).
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeAdmin = "admin"
)

// ScopeRank orders the cumulative scopes; unknown scopes rank below read.
func ScopeRank(scope string) int {
	switch scope {
	case ScopeAdmin:
		return 2
	case ScopeWrite:
		return 1
	default:
		return 0
	}
}

// ErrTokenUnknown answers lookups of tokens that were never issued.
var ErrTokenUnknown = errors.New("token unknown")

// TokenInfo is one personal access token. Hash is the hex sha256 of the
// `banyan_`-prefixed plaintext; User is the identity the token acts as;
// Expires is unix seconds (0 = no expiry) and is enforced at read time by
// the auth layer, not by the store.
type TokenInfo struct {
	Hash    string `json:"hash"`
	User    string `json:"user"`
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Expires int64  `json:"expires"`
	Revoked bool   `json:"revoked"`
	Created int64  `json:"created"`
}

func (t TokenInfo) Valid() error {
	if len(t.Hash) != 64 {
		return fmt.Errorf("token hash must be 64 hex chars")
	}
	if t.User == "" {
		return fmt.Errorf("token owner required")
	}
	switch t.Scope {
	case ScopeRead, ScopeWrite, ScopeAdmin:
	default:
		return fmt.Errorf("token scope must be read|write|admin")
	}
	return nil
}

// TokenRegistry manages personal access tokens. All methods are safe for
// concurrent use; RevokeToken is idempotent (missing rows are not an error).
type TokenRegistry interface {
	// PutToken inserts one token; a duplicate hash is an error.
	PutToken(ctx context.Context, t TokenInfo) error
	// TokenByHash answers one token by its sha256 (ErrTokenUnknown otherwise).
	TokenByHash(ctx context.Context, hash string) (TokenInfo, error)
	// ListTokens answers the user's tokens ("" = every user), never the
	// plaintexts — only the metadata shown here is listable.
	ListTokens(ctx context.Context, user string) ([]TokenInfo, error)
	// RevokeToken marks one token revoked (idempotent).
	RevokeToken(ctx context.Context, hash string) error
}

// --- memory implementation ---

func (m *MemoryMetaStore) PutToken(_ context.Context, t TokenInfo) error {
	if err := t.Valid(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.tokens[t.Hash]; dup {
		return fmt.Errorf("token hash already exists")
	}
	m.tokens[t.Hash] = t
	return nil
}

func (m *MemoryMetaStore) TokenByHash(_ context.Context, hash string) (TokenInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[hash]
	if !ok {
		return TokenInfo{}, fmt.Errorf("%w: %s", ErrTokenUnknown, hash)
	}
	return t, nil
}

func (m *MemoryMetaStore) ListTokens(_ context.Context, user string) ([]TokenInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TokenInfo
	for _, t := range m.tokens {
		if user == "" || t.User == user {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].Hash < out[j].Hash
	})
	return out, nil
}

func (m *MemoryMetaStore) RevokeToken(_ context.Context, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tokens[hash]; ok {
		t.Revoked = true
		m.tokens[hash] = t
	}
	return nil
}

// --- SQL implementation (sqlite / mysql / postgres share it) ---

func (s *SQLMetaStore) PutToken(ctx context.Context, t TokenInfo) error {
	if err := t.Valid(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, s.q(
		"INSERT INTO tokens(token_hash, owner, name, scope, expires_at, revoked, created_at) VALUES(?,?,?,?,?,?,?)"),
		t.Hash, t.User, t.Name, t.Scope, t.Expires, boolInt(t.Revoked), t.Created)
	if s.d.isUnique(err) {
		return fmt.Errorf("token hash already exists")
	}
	return err
}

func (s *SQLMetaStore) TokenByHash(ctx context.Context, hash string) (TokenInfo, error) {
	var t TokenInfo
	var revoked int
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT token_hash, owner, name, scope, expires_at, revoked, created_at FROM tokens WHERE token_hash=?"), hash).
		Scan(&t.Hash, &t.User, &t.Name, &t.Scope, &t.Expires, &revoked, &t.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenInfo{}, fmt.Errorf("%w: %s", ErrTokenUnknown, hash)
	}
	if err != nil {
		return TokenInfo{}, err
	}
	t.Revoked = revoked != 0
	return t, nil
}

func (s *SQLMetaStore) ListTokens(ctx context.Context, user string) ([]TokenInfo, error) {
	q := "SELECT token_hash, owner, name, scope, expires_at, revoked, created_at FROM tokens"
	args := []any{}
	if user != "" {
		q += " WHERE owner=?"
		args = append(args, user)
	}
	q += " ORDER BY created_at, token_hash"
	rows, err := s.db.QueryContext(ctx, s.q(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		var revoked int
		if err := rows.Scan(&t.Hash, &t.User, &t.Name, &t.Scope, &t.Expires, &revoked, &t.Created); err != nil {
			return nil, err
		}
		t.Revoked = revoked != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLMetaStore) RevokeToken(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, s.q("UPDATE tokens SET revoked=1 WHERE token_hash=?"), hash)
	return err
}

// HashToken is the at-rest form of a token plaintext (sha256 hex).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// NewTokenPlaintext mints one `banyan_`-prefixed random token (crypto/rand,
// 32 bytes base64url — the same shape the console issues, docs/17 §4).
func NewTokenPlaintext() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "banyan_" + base64.RawURLEncoding.EncodeToString(b), nil
}
