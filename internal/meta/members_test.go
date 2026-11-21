package meta

// Member store conformance (docs/design/data-model.md §16): the same battery
// on every backend. The load-bearing behaviors: the inheritance guard (a
// direct grant must rank strictly above the group's), effective resolution
// (max of direct and inherited, inherited rows marked), managed-vs-unmanaged
// (an empty member plane leaves the repo ungoverned), and the group-delete
// hygiene (members die with the group).
import (
	"context"
	"errors"
	"testing"
)

func TestMemberStoreConformance(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			ms, done := o(t)
			defer done()
			ms2, ok := ms.(MemberStore)
			if !ok {
				t.Fatalf("backend %s does not implement MemberStore", name)
			}
			gs, ok := ms.(GroupStore)
			if !ok {
				t.Fatalf("backend %s does not implement GroupStore", name)
			}
			ctx := context.Background()

			// --- the world: one group, one labeled repo, one unlabeled ---
			if err := gs.CreateGroup(ctx, GroupInfo{Name: "eng", CreatedBy: "alice", CreatedAt: 1}); err != nil {
				t.Fatalf("create group: %v", err)
			}
			if err := gs.SetRepoGroup(ctx, "demo", "eng"); err != nil {
				t.Fatalf("label: %v", err)
			}

			// --- group members: set, replace, list, remove, unknowns ---
			if err := ms2.SetGroupMember(ctx, "eng", Member{User: "alice", Rank: RankOwner, AddedBy: "admin", AddedAt: 1}); err != nil {
				t.Fatalf("set group member: %v", err)
			}
			if err := ms2.SetGroupMember(ctx, "eng", Member{User: "bob", Rank: RankViewer, AddedBy: "admin", AddedAt: 2}); err != nil {
				t.Fatalf("set group member 2: %v", err)
			}
			if err := ms2.SetGroupMember(ctx, "eng", Member{User: "bob", Rank: RankDeveloper, AddedBy: "admin", AddedAt: 3}); err != nil {
				t.Fatalf("replace group member: %v", err)
			}
			if err := ms2.SetGroupMember(ctx, "missing", Member{User: "x", Rank: RankViewer}); !errors.Is(err, ErrGroupUnknown) {
				t.Fatalf("unknown group must bounce: %v", err)
			}
			if err := ms2.SetGroupMember(ctx, "eng", Member{User: "x", Rank: "boss"}); !errors.Is(err, ErrMemberInvalid) {
				t.Fatalf("bad rank must bounce: %v", err)
			}
			if err := ms2.SetGroupMember(ctx, "eng", Member{User: " x", Rank: RankViewer}); !errors.Is(err, ErrMemberInvalid) {
				t.Fatalf("whitespace user must bounce: %v", err)
			}
			gm, err := ms2.GroupMembers(ctx, "eng")
			if err != nil || len(gm) != 2 || gm[0].User != "alice" || gm[0].Rank != RankOwner || gm[1].User != "bob" || gm[1].Rank != RankDeveloper {
				t.Fatalf("group members: %+v err=%v", gm, err)
			}
			if err := ms2.SetGroupMember(ctx, "eng", Member{User: "nobody"}); !errors.Is(err, ErrMemberUnknown) {
				t.Fatalf("removing a non-member must bounce: %v", err)
			}

			// --- the inheritance guard on direct grants ---
			// bob inherits developer from eng: equal bounces, lower bounces,
			// higher passes.
			if err := ms2.SetRepoMember(ctx, "demo", Member{User: "bob", Rank: RankDeveloper}); !errors.Is(err, ErrMemberRank) {
				t.Fatalf("equal to inherited must bounce: %v", err)
			}
			if err := ms2.SetRepoMember(ctx, "demo", Member{User: "bob", Rank: RankViewer}); !errors.Is(err, ErrMemberRank) {
				t.Fatalf("below inherited must bounce: %v", err)
			}
			if err := ms2.SetRepoMember(ctx, "demo", Member{User: "bob", Rank: RankOwner, AddedBy: "alice", AddedAt: 4}); err != nil {
				t.Fatalf("above inherited must pass: %v", err)
			}
			// carol inherits nothing: any rank is fine.
			if err := ms2.SetRepoMember(ctx, "demo", Member{User: "carol", Rank: RankViewer, AddedBy: "alice", AddedAt: 5}); err != nil {
				t.Fatalf("no-inheritance grant must pass: %v", err)
			}

			// --- effective members: direct first, inherited marked, union ---
			eff, err := ms2.RepoEffectiveMembers(ctx, "demo")
			if err != nil {
				t.Fatalf("effective: %v", err)
			}
			// alice (owner, inherited from eng), bob (owner, direct — outranks),
			// carol (viewer, direct).
			if len(eff) != 3 || eff[0].User != "alice" || eff[0].ViaGroup != "eng" || eff[0].Rank != RankOwner {
				t.Fatalf("effective[0] want alice inherited owner: %+v", eff)
			}
			if eff[1].User != "bob" || eff[1].ViaGroup != "" || eff[1].Rank != RankOwner {
				t.Fatalf("effective[1] want bob direct owner: %+v", eff)
			}
			if eff[2].User != "carol" || eff[2].ViaGroup != "" || eff[2].Rank != RankViewer {
				t.Fatalf("effective[2] want carol direct viewer: %+v", eff)
			}

			// --- MemberRank: resolution, inheritance max, unmanaged ---
			for _, tc := range []struct{ user, want string }{
				{"alice", RankOwner},   // inherited only
				{"bob", RankOwner},     // direct (the strictly-above grant)
				{"carol", RankViewer},  // direct only
				{"stranger", ""},       // managed repo, not a member
			} {
				got, managed, err := ms2.MemberRank(ctx, tc.user, "demo")
				if err != nil || !managed || got != tc.want {
					t.Fatalf("MemberRank(%s): got %q managed=%v err=%v", tc.user, got, managed, err)
				}
			}
			if _, managed, err := ms2.MemberRank(ctx, "alice", "unlabeled"); err != nil || managed {
				t.Fatalf("unlabeled memberless repo must be unmanaged: managed=%v err=%v", managed, err)
			}
			// a group with members but no labeled repos governs nothing
			if _, managed, err := ms2.MemberRank(ctx, "alice", "unrelated"); err != nil || managed {
				t.Fatalf("unrelated repo must be unmanaged: managed=%v err=%v", managed, err)
			}

			// --- direct removal falls back to the inherited rank ---
			if err := ms2.SetRepoMember(ctx, "demo", Member{User: "bob"}); err != nil {
				t.Fatalf("remove direct: %v", err)
			}
			if got, _, _ := ms2.MemberRank(ctx, "bob", "demo"); got != RankDeveloper {
				t.Fatalf("bob must fall back to inherited developer, got %q", got)
			}
			if err := ms2.SetRepoMember(ctx, "demo", Member{User: "ghost"}); !errors.Is(err, ErrMemberUnknown) {
				t.Fatalf("removing a non-member must bounce: %v", err)
			}
			// after the fallback the guard applies again at the old height
			if err := ms2.SetRepoMember(ctx, "demo", Member{User: "bob", Rank: RankDeveloper}); !errors.Is(err, ErrMemberRank) {
				t.Fatalf("equal to re-inherited must bounce: %v", err)
			}

			// --- group delete: members and grants die with the group ---
			if err := gs.DeleteGroup(ctx, "eng"); err != nil {
				t.Fatalf("delete group: %v", err)
			}
			if gm, err := ms2.GroupMembers(ctx, "eng"); err != nil || len(gm) != 0 {
				t.Fatalf("group members must be gone: %+v err=%v", gm, err)
			}
			// demo stays managed (carol's direct row survives) but alice's
			// inheritance dies with the group — she is a stranger now.
			if got, managed, _ := ms2.MemberRank(ctx, "alice", "demo"); !managed || got != "" {
				t.Fatalf("after group delete alice must be a stranger on the still-managed demo: got %q managed=%v", got, managed)
			}
			if got, _, _ := ms2.MemberRank(ctx, "carol", "demo"); got != RankViewer {
				t.Fatalf("carol's direct grant must survive: got %q", got)
			}
		})
	}
}
