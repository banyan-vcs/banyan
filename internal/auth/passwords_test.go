package auth

// Server-managed passwords (docs/gateway/authorization.md §2a): the
// account table answers before the config table, a table miss falls
// through to config, the verified-credential cache rides its TTL (and a
// changed password's old value dies with it), and the admin flag grants
// management rank from either source.
import (
	"net/http"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func basicReq(user, pass string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth(user, pass)
	return r
}

func TestStoredPasswords(t *testing.T) {
	// CI runner CPU: hashing at MinCost — see BcryptCost.
	BcryptCost = bcrypt.MinCost
	t.Cleanup(func() { BcryptCost = bcrypt.DefaultCost })
	ms := meta.NewMemoryMetaStore()
	hash, err := HashPassword("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(hash, "s3cret!") || verifyPassword(hash, "wrong") {
		t.Fatal("hash/verify roundtrip broken")
	}
	if err := ms.PutUser(t.Context(), meta.User{Name: "alice", Password: hash, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	adminHash, _ := HashPassword("rootpw")
	if err := ms.PutUser(t.Context(), meta.User{Name: "root", Password: adminHash, Admin: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	p := &StaticPolicy{
		Users: map[string]string{"bob": "cfg-pass", "alice": "stale-config-pass"},
	}
	p.verified.now = func() time.Time { return fakeNow }
	p.UserSource = ms

	// --- table first: alice's stored password wins over the config entry ---
	if id := p.Identify(basicReq("alice", "s3cret!")); id.User != "alice" {
		t.Fatalf("table password must identify: got %q", id.User)
	}
	if id := p.Identify(basicReq("alice", "stale-config-pass")); !id.Anonymous() {
		// a table account with a wrong password must NOT fall through to the
		// config entry under the same name — the table owns the account
		t.Fatalf("wrong table password must not fall through to config: got %q", id.User)
	}
	// --- config fallback: bob has no table row ---
	if id := p.Identify(basicReq("bob", "cfg-pass")); id.User != "bob" {
		t.Fatalf("config fallback must identify: got %q", id.User)
	}
	if id := p.Identify(basicReq("bob", "nope")); !id.Anonymous() {
		t.Fatalf("wrong config password: got %q", id.User)
	}
	// --- wrong password on a table-only name stays anonymous ---
	if id := p.Identify(basicReq("root", "nope")); !id.Anonymous() {
		t.Fatalf("wrong table password: got %q", id.User)
	}
	// --- admin flag from the table ---
	if !p.isAdmin("root") {
		t.Fatal("table admin flag must grant management rank")
	}
	if p.isAdmin("alice") || p.isAdmin("nobody") {
		t.Fatal("non-admin rows must not")
	}

	// --- the cache: within TTL the OLD password still verifies (that is
	// the documented window); after it lapses, the new one is required ---
	fakeNow = time.Unix(1000, 0)
	if id := p.Identify(basicReq("alice", "s3cret!")); id.User != "alice" {
		t.Fatalf("first verify: got %q", id.User)
	}
	newHash, _ := HashPassword("rotated")
	_ = ms.PutUser(t.Context(), meta.User{Name: "alice", Password: newHash, CreatedAt: 2})
	if id := p.Identify(basicReq("alice", "s3cret!")); id.User != "alice" {
		t.Fatal("within TTL the cached verdict stands (the documented window)")
	}
	fakeNow = fakeNow.Add(verifiedTTL + time.Second)
	if id := p.Identify(basicReq("alice", "s3cret!")); !id.Anonymous() {
		t.Fatal("past TTL the old password must die")
	}
	if id := p.Identify(basicReq("alice", "rotated")); id.User != "alice" {
		t.Fatal("the rotated password must verify")
	}

	// --- invalidation drops the window ---
	fakeNow = fakeNow.Add(time.Second)
	if id := p.Identify(basicReq("alice", "rotated")); id.User != "alice" {
		t.Fatal("prime the cache")
	}
	_ = ms.PutUser(t.Context(), meta.User{Name: "alice", Password: hash, CreatedAt: 3})
	p.InvalidateCredentials()
	if id := p.Identify(basicReq("alice", "rotated")); !id.Anonymous() {
		t.Fatal("after invalidation the old verdict must not survive")
	}
}

// fakeNow backs the cache clock in the password tests.
var fakeNow = time.Unix(1000, 0)
