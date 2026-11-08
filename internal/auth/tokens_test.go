package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func tokenRequest(t *testing.T, user, pass string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth(user, pass)
	return r
}

// memTokens is the smallest meta.TokenRegistry: the memory meta store already
// implements it; this exercises TokenPolicy against it unchanged.
func newTokenPolicy(t *testing.T) (*TokenPolicy, *meta.MemoryMetaStore) {
	t.Helper()
	meta := meta.NewMemoryMetaStore()
	inner := &StaticPolicy{
		Users:             map[string]string{"alice": "a", "admin": "x"},
		Admins:            []string{"admin"},
		ProtectedBranches: []string{"main"},
	}
	p := &TokenPolicy{Inner: inner, Tokens: meta}
	return p, meta
}

func putToken(t *testing.T, tr meta.TokenRegistry, user, scope string, expires int64) string {
	t.Helper()
	plain, err := meta.NewTokenPlaintext()
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.PutToken(t.Context(), meta.TokenInfo{
		Hash: meta.HashToken(plain), User: user, Name: "t", Scope: scope, Expires: expires, Created: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestTokenPolicyIdentify(t *testing.T) {
	p, m := newTokenPolicy(t)
	read := putToken(t, m, "alice", meta.ScopeRead, 0)

	if id := p.Identify(tokenRequest(t, "alice", read)); id.User != "alice" || id.Scope != meta.ScopeRead {
		t.Fatalf("token identity = %+v", id)
	}
	// The CI-job shape (any username, token as password) is rejected like
	// the console rejects it: a non-empty username must match the owner
	// (docs/17 §4) — CI jobs mint their own per-user token.
	if id := p.Identify(tokenRequest(t, "oauth2", read)); !id.Anonymous() {
		t.Fatalf("ci shape = %+v, want anonymous (owner mismatch)", id)
	}
	// A username that contradicts the owner invalidates the credential.
	if id := p.Identify(tokenRequest(t, "bob", read)); !id.Anonymous() {
		t.Fatalf("mismatched owner = %+v, want anonymous", id)
	}
	// Unknown / malformed tokens never fall back to the password table.
	if id := p.Identify(tokenRequest(t, "alice", "banyan_unknown")); !id.Anonymous() {
		t.Fatalf("unknown token = %+v, want anonymous", id)
	}
	// Passwords still delegate.
	if id := p.Identify(tokenRequest(t, "alice", "a")); id.User != "alice" || id.Scope != "" {
		t.Fatalf("password identity = %+v", id)
	}
	// Expiry is enforced at read time (unix 1000 is long past any clock).
	exp := putToken(t, m, "alice", meta.ScopeWrite, 1000)
	if id := p.Identify(tokenRequest(t, "alice", exp)); !id.Anonymous() {
		t.Fatalf("expired = %+v, want anonymous", id)
	}
	// Revocation kills the token, not the user.
	rev := putToken(t, m, "alice", meta.ScopeWrite, 0)
	if err := m.RevokeToken(t.Context(), meta.HashToken(rev)); err != nil {
		t.Fatal(err)
	}
	if id := p.Identify(tokenRequest(t, "alice", rev)); !id.Anonymous() {
		t.Fatalf("revoked = %+v, want anonymous", id)
	}
	if id := p.Identify(tokenRequest(t, "alice", "a")); id.User != "alice" {
		t.Fatal("revocation must not affect the password login")
	}
}

func TestTokenPolicyScopeCeiling(t *testing.T) {
	p, m := newTokenPolicy(t)
	read := putToken(t, m, "alice", meta.ScopeRead, 0)
	write := putToken(t, m, "alice", meta.ScopeWrite, 0)
	adminTok := putToken(t, m, "admin", meta.ScopeAdmin, 0)

	aliceRead := p.Identify(tokenRequest(t, "alice", read))
	aliceWrite := p.Identify(tokenRequest(t, "alice", write))
	adminTokID := p.Identify(tokenRequest(t, "admin", adminTok))

	// Fetch is open to every scope (inner rules still apply).
	if !p.CanFetch(aliceRead, "r1", "") {
		t.Fatal("read token must fetch")
	}
	// Push needs write — a read token is barred even though alice may push.
	if !p.CanPush(aliceWrite, "r1", "", "dev") {
		t.Fatal("write token must push an ordinary branch")
	}
	if p.CanPush(aliceRead, "r1", "", "dev") {
		t.Fatal("read token must not push")
	}
	// The inner branch rules still apply on top of the scope: main is
	// protected, alice is not an admin.
	if p.CanPush(aliceWrite, "r1", "", "main") {
		t.Fatal("protected branch must stay admin-only")
	}
	// Admin checker: token scope reaches it; read/write do not.
	if !p.CanAdmin(adminTokID, "") {
		t.Fatal("admin token must manage")
	}
	if p.CanAdmin(aliceWrite, "") || p.CanAdmin(aliceRead, "r1") {
		t.Fatal("non-admin tokens must not manage")
	}
	// The inner policy's password admins keep their rights through the decorator.
	pwAdmin := p.Identify(tokenRequest(t, "admin", "x"))
	if !p.CanAdmin(pwAdmin, "") {
		t.Fatal("password admin must manage through the decorator")
	}
	if ac, ok := interface{}(p).(AdminChecker); !ok || !ac.CanAdmin(pwAdmin, "r1") {
		t.Fatal("TokenPolicy must implement AdminChecker")
	}
}
