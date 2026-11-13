package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func newRulePolicy(t *testing.T) (*SubtreeRulePolicy, *meta.MemoryMetaStore) {
	t.Helper()
	ms := meta.NewMemoryMetaStore()
	if err := ms.CreateRepo(t.Context(), "r", 0); err != nil {
		t.Fatal(err)
	}
	inner := &StaticPolicy{
		Users:             map[string]string{"alice": "a", "bob": "b", "admin": "x"},
		Admins:            []string{"admin"},
		ProtectedBranches: []string{"main"},
	}
	return &SubtreeRulePolicy{Inner: inner, Rules: ms}, ms
}

func TestSubtreeRuleChain(t *testing.T) {
	p, ms := newRulePolicy(t)
	alice, bob := Identity{User: "alice"}, Identity{User: "bob"}

	// Unmanaged paths delegate to the inner policy: everything open.
	if !p.CanFetch(bob, "r", "/lib") || !p.CanFetch(bob, "r", "") {
		t.Fatal("no rules yet — inner policy must allow")
	}

	if err := ms.SetSubtreeRule(t.Context(), "r", meta.SubtreeRule{Path: "/lib", Users: []string{"alice"}}); err != nil {
		t.Fatal(err)
	}
	// A rule covers its anchor and everything deeper (closure semantics).
	if !p.CanFetch(alice, "r", "/lib") {
		t.Fatal("listed user must fetch")
	}
	if p.CanFetch(bob, "r", "/lib") || p.CanFetch(bob, "r", "/lib/core") {
		t.Fatal("unlisted user must be denied at the anchor and below")
	}
	// Siblings and the root stay open — the registered boundary: a deep
	// rule can never hide content from a shallower anchor.
	if !p.CanFetch(bob, "r", "/doc") || !p.CanFetch(bob, "r", "") {
		t.Fatal("unmanaged paths must stay open")
	}

	// Push is gated by the same rule (下载与提交控制, docs/gateway/authorization.md §1).
	if p.CanPush(bob, "r", "/lib", "dev") {
		t.Fatal("unlisted user must not push into a restricted subtree")
	}
	// ... but the inner policy still applies on top (protected branch).
	if p.CanPush(alice, "r", "/lib", "main") {
		t.Fatal("protected branch must still deny via the inner policy")
	}

	// Shallowest rule wins: /lib/deep's own rule is shadowed by /lib.
	if err := ms.SetSubtreeRule(t.Context(), "r", meta.SubtreeRule{Path: "/lib/deep", Users: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	if p.CanFetch(bob, "r", "/lib/deep") {
		t.Fatal("the shallowest covering rule (/lib) must govern /lib/deep")
	}
	if err := ms.DeleteSubtreeRule(t.Context(), "r", "/lib"); err != nil {
		t.Fatal(err)
	}
	if !p.CanFetch(bob, "r", "/lib/deep") {
		t.Fatal("with /lib gone, /lib/deep's own rule governs")
	}

	// The root anchor can be managed too.
	if err := ms.SetSubtreeRule(t.Context(), "r", meta.SubtreeRule{Path: "/", Users: []string{"alice"}}); err != nil {
		t.Fatal(err)
	}
	if p.CanFetch(bob, "r", "") || p.CanFetch(bob, "r", "/doc") {
		t.Fatal("a root rule restricts the whole repo")
	}
	if !p.CanFetch(alice, "r", "") {
		t.Fatal("listed user must fetch the root")
	}
}

// The inner policy's admins pass every rule — they manage the rules, a lock
// that could keep them out would brick the management plane.
func TestSubtreeRuleAdminPasses(t *testing.T) {
	p, ms := newRulePolicy(t)
	if err := ms.SetSubtreeRule(t.Context(), "r", meta.SubtreeRule{Path: "/secret", Users: []string{"alice"}}); err != nil {
		t.Fatal(err)
	}
	admin := Identity{User: "admin"}
	if !p.CanFetch(admin, "r", "/secret") || !p.CanPush(admin, "r", "/secret", "main") {
		t.Fatal("inner-policy admins must pass covering rules")
	}
	if !p.CanAdmin(admin, "r") {
		t.Fatal("CanAdmin delegates to the inner policy")
	}
}

type brokenRules struct{}

func (brokenRules) SubtreeRules(context.Context, string) ([]meta.SubtreeRule, error) {
	return nil, errors.New("meta down")
}
func (brokenRules) SetSubtreeRule(context.Context, string, meta.SubtreeRule) error { return nil }
func (brokenRules) DeleteSubtreeRule(context.Context, string, string) error           { return nil }

// A rule plane that cannot answer denies: never fall open (fail-closed).
func TestSubtreeRuleStoreDownFailsClosed(t *testing.T) {
	p := &SubtreeRulePolicy{
		Inner: &StaticPolicy{Users: map[string]string{"alice": "a"}},
		Rules: brokenRules{},
	}
	if p.CanFetch(Identity{User: "alice"}, "r", "/lib") {
		t.Fatal("unavailable rule store must deny")
	}
}

// Identify and VisibleRefs delegate untouched.
func TestSubtreeRuleDelegation(t *testing.T) {
	p, _ := newRulePolicy(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.SetBasicAuth("alice", "a")
	if id := p.Identify(req); id.User != "alice" {
		t.Fatalf("identify = %+v", id)
	}
	refs := []Ref{{Name: "refs/heads/main"}}
	if out := p.VisibleRefs(Identity{User: "alice"}, "r", refs); len(out) != 1 {
		t.Fatalf("visible refs = %+v", out)
	}
}
