package auth

// MemberPolicy (docs/design/data-model.md §16): the ledger gates governed
// repositories at two heights (viewer for fetch, developer for push), admins
// outrank the ledger, ungoverned repositories pass through unchanged, and a
// broken ledger fails closed. The inner policy's own rules (protected
// branches here) keep applying underneath an admitted member.
import (
	"context"
	"testing"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func TestMemberPolicy(t *testing.T) {
	ms := meta.NewMemoryMetaStore()
	ctx := context.Background()
	if err := ms.CreateGroup(ctx, meta.GroupInfo{Name: "eng", CreatedBy: "admin", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := ms.SetRepoGroup(ctx, "demo", "eng"); err != nil {
		t.Fatal(err)
	}
	// The governed world: alice owns eng (inherited by demo), bob views eng,
	// carol gets a direct developer grant on demo. spark stays ungoverned.
	for _, m := range []meta.Member{
		{User: "alice", Rank: meta.RankOwner, AddedBy: "admin", AddedAt: 1},
		{User: "bob", Rank: meta.RankViewer, AddedBy: "admin", AddedAt: 1},
	} {
		if err := ms.SetGroupMember(ctx, "eng", m); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.SetRepoMember(ctx, "demo", meta.Member{User: "carol", Rank: meta.RankDeveloper, AddedBy: "admin", AddedAt: 1}); err != nil {
		t.Fatal(err)
	}

	inner := &StaticPolicy{
		Users:             map[string]string{"alice": "a", "bob": "b", "carol": "c", "dave": "d", "root": "r"},
		Admins:            []string{"root"},
		ProtectedBranches: []string{"main"},
	}
	p := &MemberPolicy{Inner: inner, Members: ms}

	id := func(u string) Identity { return Identity{User: u} }

	// --- governed: the two heights ---
	cases := []struct {
		user              string
		fetch, push, main bool // main = push to the protected branch
	}{
		{"alice", true, true, false},  // inherited owner — the inner protected-branch rule still applies
		{"bob", true, false, false},   // inherited viewer: fetch yes, push no
		{"carol", true, true, false},  // direct developer: push yes, protected still admin-only (inner rule)
		{"dave", false, false, false}, // authenticated stranger: locked out
		{"root", true, true, true},    // admin outranks both the ledger and the protected rule
	}
	for _, tc := range cases {
		if got := p.CanFetch(id(tc.user), "demo", ""); got != tc.fetch {
			t.Errorf("%s fetch demo: got %v want %v", tc.user, got, tc.fetch)
		}
		if got := p.CanPush(id(tc.user), "demo", "", "feature"); got != tc.push {
			t.Errorf("%s push demo: got %v want %v", tc.user, got, tc.push)
		}
		if got := p.CanPush(id(tc.user), "demo", "", "main"); got != tc.main {
			t.Errorf("%s push demo main (protected): got %v want %v", tc.user, got, tc.main)
		}
	}

	// --- ungoverned: pass-through to the inner policy ---
	if !p.CanFetch(id("dave"), "spark", "") || !p.CanPush(id("dave"), "spark", "", "feature") {
		t.Error("ungoverned repo must behave exactly as the inner policy: dave is a plain authenticated user")
	}
	if p.CanPush(id("dave"), "spark", "", "main") {
		t.Error("inner protected branch must keep applying on ungoverned repos")
	}

	// --- subtree granularity rides the repo gate, then the inner layer ---
	if !p.CanFetch(id("bob"), "demo", "/lib") {
		t.Error("viewer fetches the whole governed repo, subtrees included")
	}
	if p.CanPush(id("bob"), "demo", "/lib", "x") {
		t.Error("viewer cannot push a subtree of a governed repo")
	}

	// --- the personal-branch inner rule keeps applying to members ---
	if p.CanPush(id("carol"), "demo", "", "alice/wip") {
		t.Error("carol must not push alice's personal branch (inner rule under the member gate)")
	}

	// --- a broken ledger fails closed ---
	broke := &MemberPolicy{Inner: inner, Members: brokenMemberStore{meta.NewMemoryMetaStore()}}
	if broke.CanFetch(id("alice"), "demo", "") || broke.CanPush(id("alice"), "demo", "", "x") {
		t.Error("a ledger error must deny (fail-closed), not fall open")
	}
}

// brokenMemberStore always errors.
type brokenMemberStore struct{ *meta.MemoryMetaStore }

func (brokenMemberStore) MemberRank(context.Context, string, string) (string, bool, error) {
	return "", false, context.DeadlineExceeded
}
