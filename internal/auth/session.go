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

// MintSession issues one short-lived session credential for user. label
// ("" = none) names the device — "cli-session:laptop42" instead of an
// anonymous "cli-session" — so an admin can revoke ONE lost machine, not
// the user's every session. The plaintext exists exactly once at mint (the
// registry stores its sha256); scope is write — sessions fetch, push and
// commit, but never administer. Every mint also sweeps the registry's
// expired rows (sessions mint on every re-auth; without a sweep the table
// grows forever).
func MintSession(ctx context.Context, tokens meta.TokenRegistry, user string, ttl time.Duration, now time.Time, label string) (string, meta.TokenInfo, error) {
	plain, err := meta.NewTokenPlaintext()
	if err != nil {
		return "", meta.TokenInfo{}, err
	}
	name := "cli-session"
	if l := sanitizeLabel(label); l != "" {
		name = "cli-session:" + l
	}
	info := meta.TokenInfo{
		Hash:    meta.HashToken(plain),
		User:    user,
		Name:    name,
		Scope:   meta.ScopeWrite,
		Expires: now.Add(ttl).Unix(),
		Created: now.Unix(),
	}
	if err := tokens.PutToken(ctx, info); err != nil {
		return "", meta.TokenInfo{}, err
	}
	_, _ = tokens.DeleteExpiredTokens(ctx, now.Unix()) // best-effort hygiene
	return plain, info, nil
}

// sanitizeLabel keeps device labels to the file-name-safe alphabet (they
// surface in token listings and the web console) and caps their length.
func sanitizeLabel(label string) string {
	out := make([]rune, 0, 32)
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		}
		if len(out) == 32 {
			break
		}
	}
	return string(out)
}
