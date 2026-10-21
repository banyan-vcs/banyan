package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
)

// The conformance suite runs identically against every MetaStore backend:
// behavioral parity is the contract that makes the backend swappable
// (docs/02 §2.1). The concurrent-CAS case doubles as the SQLite
// busy/WAL serialization exercise.
func TestMetaStoreConformance(t *testing.T) {
	open := map[string]func(*testing.T) (MetaStore, func()){
		"mem": func(*testing.T) (MetaStore, func()) {
			return NewMemoryMetaStore(), func() {}
		},
		"sqlite": func(t *testing.T) (MetaStore, func()) {
			s, err := NewSQLiteMetaStore(filepath.Join(t.TempDir(), "meta.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			return s, func() { s.Close() }
		},
	}
	for name, o := range open {
		t.Run(name, func(t *testing.T) {
			m, done := o(t)
			defer done()
			runMetaConformance(t, m)
		})
	}
}

func runMetaConformance(t *testing.T, m MetaStore) {
	t.Helper()
	ctx := context.Background()

	// --- repo registry ---
	if err := m.CreateRepo(ctx, "r1", model.SHA1); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := m.CreateRepo(ctx, "r1", model.SHA1); err == nil {
		t.Fatal("duplicate CreateRepo must fail")
	}
	info, err := m.GetRepo(ctx, "r1")
	if err != nil || info.DefaultBranch != "main" {
		t.Fatalf("get repo = %+v, %v", info, err)
	}
	if _, err := m.GetRepo(ctx, "nope"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("missing repo error = %v, want ErrRepoNotFound", err)
	}

	// --- central pack + object index ---
	oids := []model.OID{"a1", "a2", "a3"}
	if err := m.PutPack(ctx, "r1", "p1", oids, []string{"n1", "n2"}); err != nil {
		t.Fatalf("putpack: %v", err)
	}
	// Idempotent re-registration (immutable packs, first writer wins).
	if err := m.PutPack(ctx, "r1", "p1", oids, []string{"n1", "n2"}); err != nil {
		t.Fatalf("putpack idempotent: %v", err)
	}
	if err := m.PutPack(ctx, "nope", "p1", oids, []string{"n1"}); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("putpack unknown repo = %v", err)
	}
	loc, err := m.LocateObjects(ctx, "r1", []model.OID{"a2", "missing"})
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if len(loc) != 1 || loc["a2"].PackOID != "p1" || loc["a2"].NodeID != "n1" {
		t.Fatalf("locate result = %+v", loc)
	}
	pi, err := m.GetPack(ctx, "r1", "p1")
	if err != nil || len(pi.Nodes) != 2 || pi.Nodes[0] != "n1" {
		t.Fatalf("getpack = %+v, %v", pi, err)
	}
	if _, err := m.GetPack(ctx, "r1", "pX"); !errors.Is(err, ErrPackNotFound) {
		t.Fatalf("missing pack error = %v, want ErrPackNotFound", err)
	}
	if err := m.PutPack(ctx, "r1", "p0", []model.OID{"a0"}, []string{"n1"}); err != nil {
		t.Fatalf("putpack p0: %v", err)
	}
	packs, err := m.ListPacks(ctx, "r1")
	if err != nil || len(packs) != 2 {
		t.Fatalf("listpacks = %v, %v", packs, err)
	}

	// --- repack support: PackObjects / DropPack ---
	objs1, err := m.PackObjects(ctx, "r1", "p1")
	if err != nil || len(objs1) != 3 {
		t.Fatalf("packobjects = %v, %v", objs1, err)
	}
	if _, err := m.PackObjects(ctx, "r1", "pX"); !errors.Is(err, ErrPackNotFound) {
		t.Fatalf("packobjects missing = %v, want ErrPackNotFound", err)
	}
	if err := m.DropPack(ctx, "r1", "p0"); err != nil {
		t.Fatalf("droppack: %v", err)
	}
	if err := m.DropPack(ctx, "r1", "p0"); !errors.Is(err, ErrPackNotFound) {
		t.Fatalf("droppack twice = %v, want ErrPackNotFound", err)
	}
	if packs, _ := m.ListPacks(ctx, "r1"); len(packs) != 1 || packs[0].PackOID != "p1" {
		t.Fatalf("post-drop packs = %v", packs)
	}
	if loc, _ := m.LocateObjects(ctx, "r1", []model.OID{"a0"}); len(loc) != 0 {
		t.Fatalf("dropped objects still located: %v", loc)
	}
	if loc, _ := m.LocateObjects(ctx, "r1", []model.OID{"a2"}); len(loc) != 1 {
		t.Fatalf("surviving objects lost: %v", loc)
	}

	// --- branch ref-map (timelines) ---
	root := model.Timeline{Repo: "r1", Anchor: model.RootAnchor, Name: "main"}
	if _, err := m.Timeline(ctx, root); !errors.Is(err, ErrTimelineUnknown) {
		t.Fatalf("unknown timeline = %v, want ErrTimelineUnknown", err)
	}
	if err := m.EnsureTimeline(ctx, root, "h1", ""); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Idempotent: second ensure must not overwrite the head.
	if err := m.EnsureTimeline(ctx, root, "hX", ""); err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if st, err := m.Timeline(ctx, root); err != nil || st.Head != "h1" {
		t.Fatalf("after re-ensure state = %+v, %v", st, err)
	}
	if err := m.CASHead(ctx, root, "stale", "h2"); !errors.Is(err, ErrNonFastForward) {
		t.Fatalf("cas stale = %v, want ErrNonFastForward", err)
	}
	if err := m.CASHead(ctx, root, "h1", "h2"); err != nil {
		t.Fatalf("cas: %v", err)
	}
	sub := model.Timeline{Repo: "r1", Anchor: "/lib", Name: "main"}
	if err := m.EnsureTimeline(ctx, sub, "s1", "h2"); err != nil {
		t.Fatalf("ensure subtree: %v", err)
	}
	roots, err := m.ListTimelines(ctx, "r1", model.RootAnchor)
	if err != nil || len(roots) != 1 || roots[0].Name != "main" {
		t.Fatalf("list root timelines = %+v, %v", roots, err)
	}
	subs, err := m.ListTimelines(ctx, "r1", "/lib")
	if err != nil || len(subs) != 1 || subs[0].Anchor != "/lib" {
		t.Fatalf("list subtree timelines = %+v, %v", subs, err)
	}
	// Deletion via zero next.
	if err := m.CASHead(ctx, sub, "s1", ""); err != nil {
		t.Fatalf("delete timeline: %v", err)
	}
	if _, err := m.Timeline(ctx, sub); !errors.Is(err, ErrTimelineUnknown) {
		t.Fatalf("deleted timeline = %v, want ErrTimelineUnknown", err)
	}
	// Delete of a missing timeline is a no-op (create with expected zero).
	if err := m.CASHead(ctx, sub, "", "s2"); err != nil {
		t.Fatalf("create via zero expected: %v", err)
	}

	// --- CAS race: N concurrent writers, exactly one wins ---
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			next := model.OID(fmt.Sprintf("%040d", i))
			if err := m.CASHead(ctx, root, "h2", next); err == nil {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("concurrent CAS winners = %d, want 1", wins.Load())
	}
}
