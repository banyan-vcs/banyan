package meta

// Group store conformance (docs/design/data-model.md §15): the same battery
// on every backend, per the swappability contract. The load-bearing
// behaviors: assignment is one label per repo (re-assign replaces, clearing
// works), and deleting a group clears its assignments — a repo never points
// at a missing group.
import (
	"context"
	"errors"
	"testing"
)

func TestGroupStoreConformance(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			ms, done := o(t)
			defer done()
			gs, ok := ms.(GroupStore)
			if !ok {
				t.Fatalf("backend %s does not implement GroupStore", name)
			}
			ctx := context.Background()

			// --- create + duplicate + validation ---
			if err := gs.CreateGroup(ctx, GroupInfo{Name: "eng", Description: "engineering", CreatedBy: "alice", CreatedAt: 1}); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := gs.CreateGroup(ctx, GroupInfo{Name: "eng", CreatedBy: "bob"}); !errors.Is(err, ErrGroupExists) {
				t.Fatalf("duplicate must bounce: %v", err)
			}
			if err := gs.CreateGroup(ctx, GroupInfo{Name: "a/b"}); !errors.Is(err, ErrGroupInvalid) {
				t.Fatalf("slash name must bounce: %v", err)
			}
			if err := gs.CreateGroup(ctx, GroupInfo{Name: ""}); !errors.Is(err, ErrGroupInvalid) {
				t.Fatalf("empty name must bounce: %v", err)
			}

			// --- list: name-sorted ---
			if err := gs.CreateGroup(ctx, GroupInfo{Name: "docs", CreatedBy: "bob", CreatedAt: 2}); err != nil {
				t.Fatalf("create 2: %v", err)
			}
			list, err := gs.ListGroups(ctx)
			if err != nil || len(list) != 2 || list[0].Name != "docs" || list[1].Name != "eng" {
				t.Fatalf("list: %+v err=%v", list, err)
			}

			// --- assignment: set, replace, clear; unknown group bounces ---
			if err := gs.SetRepoGroup(ctx, "demo", "eng"); err != nil {
				t.Fatalf("assign: %v", err)
			}
			if err := gs.SetRepoGroup(ctx, "demo", "docs"); err != nil {
				t.Fatalf("re-assign must replace: %v", err)
			}
			if err := gs.SetRepoGroup(ctx, "demo", "missing"); !errors.Is(err, ErrGroupUnknown) {
				t.Fatalf("unknown group must bounce: %v", err)
			}
			if err := gs.SetRepoGroup(ctx, "other", "eng"); err != nil {
				t.Fatalf("assign 2: %v", err)
			}
			m, err := gs.RepoGroups(ctx)
			if err != nil || len(m) != 2 || m["demo"] != "docs" || m["other"] != "eng" {
				t.Fatalf("repo groups: %+v err=%v", m, err)
			}
			if err := gs.SetRepoGroup(ctx, "demo", ""); err != nil {
				t.Fatalf("clear: %v", err)
			}
			if m, err = gs.RepoGroups(ctx); err != nil || len(m) != 1 || m["other"] != "eng" {
				t.Fatalf("after clear: %+v err=%v", m, err)
			}

			// --- delete: assignments of the group drop with it ---
			if err := gs.DeleteGroup(ctx, "eng"); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if err := gs.DeleteGroup(ctx, "eng"); !errors.Is(err, ErrGroupUnknown) {
				t.Fatalf("second delete must 404: %v", err)
			}
			if m, err = gs.RepoGroups(ctx); err != nil || len(m) != 0 {
				t.Fatalf("delete must clear assignments: %+v err=%v", m, err)
			}
			if list, err = gs.ListGroups(ctx); err != nil || len(list) != 1 || list[0].Name != "docs" {
				t.Fatalf("after delete: %+v err=%v", list, err)
			}
		})
	}
}
