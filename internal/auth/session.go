// Session credentials (docs/gateway/authorization.md §6a): short-lived
// tokens minted ONLY programmatically — an SSH public-key authentication
// (the `banyan-auth` exec) or an authenticated web-console approval. No
// operator-minted permanent PATs on this path: the TTL bounds exposure,
// the registry keeps revocation, and the CLI refreshes by re-authenticating
// (SSH silently, the web flow on demand).
package auth

import (
	"context"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// SessionTTL is the default session credential lifetime. Long enough for a
// working day, short enough that a leaked credential is a bounded incident.
const SessionTTL = 12 * time.Hour

// MintSession issues one short-lived session credential for user. The
// plaintext exists exactly once at mint (the registry stores its sha256);
// scope is write — sessions fetch, push and commit, but never administer.
func MintSession(ctx context.Context, tokens meta.TokenRegistry, user string, ttl time.Duration, now time.Time) (string, meta.TokenInfo, error) {
	plain, err := meta.NewTokenPlaintext()
	if err != nil {
		return "", meta.TokenInfo{}, err
	}
	info := meta.TokenInfo{
		Hash:    meta.HashToken(plain),
		User:    user,
		Name:    "cli-session",
		Scope:   meta.ScopeWrite,
		Expires: now.Add(ttl).Unix(),
		Created: now.Unix(),
	}
	if err := tokens.PutToken(ctx, info); err != nil {
		return "", meta.TokenInfo{}, err
	}
	return plain, info, nil
}
