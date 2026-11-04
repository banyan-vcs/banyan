package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
)

// TestRenameFenceRejectsMutations pins the write barrier's reject side
// (docs/03 §6b): while the fence stands every mutating operation on either
// name fails with ErrRepoRenaming (a racing push fails cleanly instead of
// landing a confirmed-but-invisible write), reads keep answering, the flip
// lifts the fence, AbortRename lifts it, and only a DIFFERENT overlapping
// rename is refused — re-running the same one is idempotent.
func TestRenameFenceRejectsMutations(t *testing.T) {
	open := []struct {
		name string
		mk   func(t *testing.T) MetaStore
	}{
		{"memory", func(t *testing.T) MetaStore { return NewMemoryMetaStore() }},
		{"sqlite", func(t *testing.T) MetaStore {
			ms, err := NewSQLiteMetaStore(filepath.Join(t.TempDir(), "fence.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ms.Close() }) // release the Windows file lock
			return ms
		}},
	}
	for _, o := range open {
		t.Run(o.name, func(t *testing.T) {
			m := o.mk(t)
			ctx := context.Background()
			if err := m.CreateRepo(ctx, "alpha", model.SHA1); err != nil {
				t.Fatal(err)
			}
			oid := model.OID("0123456789012345678901234567890123456789")
			if err := m.PutPack(ctx, "alpha", oid, []model.OID{oid}, []string{"n1"}); err != nil {
				t.Fatal(err)
			}
			tl := model.Timeline{Repo: "alpha", Anchor: model.RootAnchor, Name: "main"}
			if err := m.EnsureTimeline(ctx, tl, oid, model.OID("")); err != nil {
				t.Fatal(err)
			}

			if err := m.BeginRename(ctx, "alpha", "beta"); err != nil {
				t.Fatal(err)
			}
			renaming := func(err error) bool { return errors.Is(err, ErrRepoRenaming) }

			if err := m.PutPack(ctx, "alpha", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, []string{"n1"}); !renaming(err) {
				t.Errorf("PutPack under old name during fence: got %v", err)
			}
			if err := m.PutPack(ctx, "beta", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, []string{"n1"}); !renaming(err) {
				t.Errorf("PutPack under new name during fence: got %v", err)
			}
			if err := m.CASHead(ctx, tl, oid, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); !renaming(err) {
				t.Errorf("CASHead during fence: got %v", err)
			}
			if err := m.EnsureTimeline(ctx, model.Timeline{Repo: "alpha", Anchor: model.RootAnchor, Name: "dev"}, oid, model.OID("")); !renaming(err) {
				t.Errorf("EnsureTimeline during fence: got %v", err)
			}
			if err := m.CreateRepo(ctx, "beta", model.SHA1); !renaming(err) {
				t.Errorf("CreateRepo(target) during fence: got %v", err)
			}
			if err := m.DeleteRepo(ctx, "alpha"); !renaming(err) {
				t.Errorf("DeleteRepo during fence: got %v", err)
			}
			if err := m.SetHook(ctx, "alpha", HookInfo{ID: "h", Events: []string{"pre-receive"}, URL: "http://x"}); !renaming(err) {
				t.Errorf("SetHook during fence: got %v", err)
			}
			if err := m.SetPlacement(ctx, "alpha", 2); !renaming(err) {
				t.Errorf("SetPlacement during fence: got %v", err)
			}
			if err := m.LinkPool(ctx, "alpha", "@pools/p", false); !renaming(err) {
				t.Errorf("LinkPool during fence: got %v", err)
			}
			// Reads stay live: the fence rejects mutations, never visibility.
			if _, err := m.GetRepo(ctx, "alpha"); err != nil {
				t.Errorf("GetRepo during fence: %v", err)
			}
			// A DIFFERENT overlapping rename is refused; the same one re-installs.
			if err := m.BeginRename(ctx, "alpha", "gamma"); !renaming(err) {
				t.Errorf("overlapping BeginRename: got %v", err)
			}
			if err := m.BeginRename(ctx, "alpha", "beta"); err != nil {
				t.Errorf("idempotent BeginRename: %v", err)
			}

			// Abort lifts the fence: writes under the old name land again.
			if err := m.AbortRename(ctx, "alpha", "beta"); err != nil {
				t.Fatal(err)
			}
			if err := m.PutPack(ctx, "alpha", "cccccccccccccccccccccccccccccccccccccccc", []model.OID{oid}, []string{"n1"}); err != nil {
				t.Errorf("PutPack after abort: %v", err)
			}

			// Full two-phase rename: fence → flip (which lifts it) → writes
			// under the new name succeed, under the old name the repo is gone.
			if err := m.BeginRename(ctx, "alpha", "beta"); err != nil {
				t.Fatal(err)
			}
			if err := m.RenameRepo(ctx, "alpha", "beta"); err != nil {
				t.Fatal(err)
			}
			packs, err := m.ListPacks(ctx, "beta")
			if err != nil || len(packs) != 2 {
				t.Errorf("packs after flip = %v, %v (want 2 migrated)", packs, err)
			}
			if err := m.PutPack(ctx, "beta", "dddddddddddddddddddddddddddddddddddddddd", []model.OID{oid}, []string{"n1"}); err != nil {
				t.Errorf("PutPack after flip: %v", err)
			}
			if _, err := m.GetRepo(ctx, "alpha"); !errors.Is(err, ErrRepoNotFound) {
				t.Errorf("old name after flip: got %v", err)
			}
			// Tombstone: the retired name refuses re-creation — a late push
			// must not ack into a fresh, disconnected repository.
			if err := m.CreateRepo(ctx, "alpha", model.SHA1); !errors.Is(err, ErrRepoRenamedAway) {
				t.Errorf("CreateRepo on retired name: got %v", err)
			}
			// Deleting the renamed-to repo recycles the old name.
			if err := m.DeleteRepo(ctx, "beta"); err != nil {
				t.Fatal(err)
			}
			if err := m.CreateRepo(ctx, "alpha", model.SHA1); err != nil {
				t.Errorf("CreateRepo after tombstone lifted: %v", err)
			}
		})
	}
}
