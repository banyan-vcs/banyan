// Personal access tokens in the metadata plane (docs/gateway/authorization.md §6, docs/project/roadmap.md M5):
// the plaintext exists only at creation (returned once to the operator);
// the store keeps its sha256. Tokens are cluster state — any gateway with
// the metadata (all-in-one, refs-serving node) validates them; the gateway
// behind an upstream console uses the console's own PATs instead (docs/web/console.md).
// TokenRegistry is deliberately NOT part of the MetaStore composite: only
// the stores that own metadata implement it, and consumers type-assert
// (meta.(TokenRegistry)) so remote-meta gateways compile unchanged.
package meta

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"gorm.io/gorm"
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
	// DeleteExpiredTokens drops rows past their Expires (0 = never); it
	// answers the number removed. Sessions mint every re-auth, so the
	// mint paths sweep lazily — the table must not grow forever.
	DeleteExpiredTokens(ctx context.Context, now int64) (int64, error)
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

func (m *MemoryMetaStore) DeleteExpiredTokens(_ context.Context, now int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for h, t := range m.tokens {
		if t.Expires != 0 && t.Expires < now {
			delete(m.tokens, h)
			n++
		}
	}
	return n, nil
}

// --- SQL implementation (sqlite / mysql / postgres share it) ---

func (s *SQLMetaStore) PutToken(ctx context.Context, t TokenInfo) error {
	if err := t.Valid(); err != nil {
		return err
	}
	err := s.orm.WithContext(ctx).Create(&tokenRow{
		TokenHash: t.Hash, Owner: t.User, Name: t.Name, Scope: t.Scope,
		ExpiresAt: t.Expires, Revoked: boolInt(t.Revoked), CreatedAt: t.Created,
	}).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("token hash already exists")
	}
	return err
}

func (s *SQLMetaStore) TokenByHash(ctx context.Context, hash string) (TokenInfo, error) {
	var row tokenRow
	err := s.orm.WithContext(ctx).Where("token_hash = ?", hash).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return TokenInfo{}, fmt.Errorf("%w: %s", ErrTokenUnknown, hash)
	}
	if err != nil {
		return TokenInfo{}, err
	}
	return row.info(), nil
}

func (s *SQLMetaStore) ListTokens(ctx context.Context, user string) ([]TokenInfo, error) {
	q := s.orm.WithContext(ctx).Model(&tokenRow{})
	if user != "" {
		q = q.Where("owner = ?", user)
	}
	var rows []tokenRow
	if err := q.Order("created_at, token_hash").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TokenInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.info())
	}
	return out, nil
}

func (s *SQLMetaStore) RevokeToken(ctx context.Context, hash string) error {
	return s.orm.WithContext(ctx).Model(&tokenRow{}).
		Where("token_hash = ?", hash).Update("revoked", 1).Error
}

func (s *SQLMetaStore) DeleteExpiredTokens(ctx context.Context, now int64) (int64, error) {
	res := s.orm.WithContext(ctx).
		Where("expires_at != 0 AND expires_at < ?", now).Delete(&tokenRow{})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// HashToken is the at-rest form of a token plaintext (sha256 hex).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// NewTokenPlaintext mints one `banyan_`-prefixed random token (crypto/rand,
// 32 bytes base64url — the same shape the console issues, docs/web/console.md §4).
func NewTokenPlaintext() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "banyan_" + base64.RawURLEncoding.EncodeToString(b), nil
}
