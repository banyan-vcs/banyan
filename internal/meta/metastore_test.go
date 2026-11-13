package meta

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
)

// The conformance suite runs identically against every MetaStore backend:
// behavioral parity is the contract that makes the backend swappable
// (docs/storage/storage.md §2.1). The concurrent-CAS case doubles as the SQLite
// busy/WAL serialization exercise.
//
// metaOpeners is the shared backend matrix — every store-level battery
// (conformance, the rename fence) runs it, so the cluster backends cannot
// drift behind the file backends. mysql/postgres join when their DSN is
// provided — locally via Docker (docs/storage/ha.md), in CI via service containers:
//
//	BANYAN_TEST_MYSQL_DSN="root:banyan@tcp(127.0.0.1:3306)/banyan?charset=utf8mb4"
//	BANYAN_TEST_POSTGRES_DSN="postgres://postgres:banyan@127.0.0.1:5432/banyan?sslmode=disable"
func metaOpeners() map[string]func(*testing.T) (MetaStore, func()) {
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
	for backend, env := range map[string]string{
		"mysql":    "BANYAN_TEST_MYSQL_DSN",
		"postgres": "BANYAN_TEST_POSTGRES_DSN",
	} {
		dsn := os.Getenv(env)
		if dsn == "" {
			continue
		}
		open[backend] = func(t *testing.T) (MetaStore, func()) {
			s, err := NewSQLMetaStore(backend, dsn)
			if err != nil {
				t.Fatalf("open %s: %v", backend, err)
			}
			dropAllTables(t, s)
			if err := s.applySchema(t.Context()); err != nil {
				t.Fatalf("reschema %s: %v", backend, err)
			}
			return s, func() { s.Close() }
		}
	}
	return open
}

func TestMetaStoreConformance(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			m, done := o(t)
			defer done()
			runMetaConformance(t, m)
		})
	}
}

// dropAllTables wipes the schema on a shared cluster database so each
// battery starts empty (the file backends get a fresh file). Every table
// applySchema creates must be listed here — a missed table survives runs
// and turns the shared-DB suites order-dependent.
func dropAllTables(t *testing.T, s *SQLMetaStore) {
	t.Helper()
	for _, table := range []string{"rename_fence", "repo_pools", "repo_blobs", "hooks", "placement", "objects", "pack_nodes", "packs", "timelines", "repos", "tokens", "subtree_rules"} {
		if _, err := s.db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
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
	// Fork-point advance (SetTimelineBase, docs/design/data-model.md §12 explicit rebase): the
	// base moves while the head stays, a later CAS preserves the new base,
	// and unknown timelines reject.
	if err := m.SetTimelineBase(ctx, sub, "h9"); err != nil {
		t.Fatalf("set base: %v", err)
	}
	if st, err := m.Timeline(ctx, sub); err != nil || st.Head != "s2" || st.Base != "h9" {
		t.Fatalf("after set base = %+v, %v", st, err)
	}
	if err := m.CASHead(ctx, sub, "s2", "s3"); err != nil {
		t.Fatalf("cas after set base: %v", err)
	}
	if st, err := m.Timeline(ctx, sub); err != nil || st.Head != "s3" || st.Base != "h9" {
		t.Fatalf("cas dropped base = %+v, %v", st, err)
	}
	if err := m.SetTimelineBase(ctx, model.Timeline{Repo: "r1", Anchor: "/lib", Name: "ghost"}, "h"); !errors.Is(err, ErrTimelineUnknown) {
		t.Fatalf("set base unknown = %v, want ErrTimelineUnknown", err)
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

	// --- repo blobs round-trip and ride rename/delete ---
	if err := m.SetRepoBlob(ctx, "r1", "custom-hooks.tar", []byte("tarball")); err != nil {
		t.Fatalf("set blob: %v", err)
	}
	if err := m.SetRepoBlob(ctx, "r1", "custom-hooks.tar", []byte("tarball-v2")); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if b, ok, err := m.GetRepoBlob(ctx, "r1", "custom-hooks.tar"); err != nil || !ok || string(b) != "tarball-v2" {
		t.Fatalf("blob = %q %v %v, want tarball-v2 true", b, ok, err)
	}
	if _, ok, _ := m.GetRepoBlob(ctx, "r1", "missing"); ok {
		t.Fatal("missing blob must answer not-found")
	}

	// --- pool registry: link, upsert, list, unlink, delete-repo side effects ---
	if err := m.LinkPool(ctx, "r1", "@pools/1/pool", true); err != nil {
		t.Fatalf("link pool: %v", err)
	}
	if err := m.LinkPool(ctx, "r-fork", "@pools/1/pool", false); err != nil {
		t.Fatalf("link fork: %v", err)
	}
	if err := m.LinkPool(ctx, "r1", "@pools/2/pool", true); err != nil { // upsert moves member
		t.Fatalf("relink pool: %v", err)
	}
	if p, ok, err := m.PoolOf(ctx, "r1"); err != nil || !ok || p != "@pools/2/pool" {
		t.Fatalf("pool of r1 = %q %v %v, want @pools/2/pool", p, ok, err)
	}
	if _, ok, _ := m.PoolOf(ctx, "unlinked"); ok {
		t.Fatal("unlinked repo must answer no pool")
	}
	if err := m.UnlinkPool(ctx, "r-fork"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, ok, _ := m.PoolOf(ctx, "r-fork"); ok {
		t.Fatal("unlinked repo must stay unlinked")
	}
	if err := m.LinkPool(ctx, "r-fork", "@pools/2/pool", false); err != nil {
		t.Fatalf("relink fork: %v", err)
	}
	if err := m.DeletePoolRecords(ctx, "@pools/2/pool"); err != nil {
		t.Fatalf("delete pool records: %v", err)
	}
	if _, ok, _ := m.PoolOf(ctx, "r-fork"); ok {
		t.Fatal("member of deleted pool must be unlinked")
	}

	// --- rename carries hooks; delete drops every repo-keyed row ---
	if err := m.SetHook(ctx, "r1", HookInfo{ID: "notify", Events: []string{"pre-receive"}, URL: "http://h"}); err != nil {
		t.Fatalf("set hook: %v", err)
	}
	if rs, ok := m.(SubtreeRuleStore); ok {
		if err := rs.SetSubtreeRule(ctx, "r1", SubtreeRule{Path: "/secret", Users: []string{"admin"}}); err != nil {
			t.Fatalf("set subtree rule: %v", err)
		}
	}
	if err := m.LinkPool(ctx, "r1", "@pools/carry/pool", false); err != nil {
		t.Fatalf("link before rename: %v", err)
	}
	if err := m.RenameRepo(ctx, "r1", "r2"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if p, ok, err := m.PoolOf(ctx, "r2"); err != nil || !ok || p != "@pools/carry/pool" {
		t.Fatalf("pool lost across rename: %q %v %v", p, ok, err)
	}
	if hs, err := m.ListHooks(ctx, "r2"); err != nil || len(hs) != 1 || hs[0].ID != "notify" {
		t.Fatalf("hooks lost across rename: %v, %v", hs, err)
	}
	if rs, ok := m.(SubtreeRuleStore); ok {
		if rules, err := rs.SubtreeRules(ctx, "r2"); err != nil || len(rules) != 1 || rules[0].Path != "/secret" {
			t.Fatalf("subtree rules lost across rename: %v, %v", rules, err)
		}
	}
	if err := m.DeleteRepo(ctx, "r2"); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if _, err := m.GetRepo(ctx, "r2"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("deleted repo get = %v, want ErrRepoNotFound", err)
	}
	if _, err := m.GetPack(ctx, "r2", "p1"); err == nil {
		t.Fatal("pack must be gone after repo deletion")
	}
	if tls, err := m.ListAllTimelines(ctx, "r2"); err != nil || len(tls) != 0 {
		t.Fatalf("deleted timelines = %v, %v", tls, err)
	}
	if hs, err := m.ListHooks(ctx, "r2"); err != nil || len(hs) != 0 {
		t.Fatalf("deleted hooks = %v, %v", hs, err)
	}
	if rs, ok := m.(SubtreeRuleStore); ok {
		if rules, err := rs.SubtreeRules(ctx, "r2"); err != nil || len(rules) != 0 {
			t.Fatalf("deleted subtree rules = %v, %v", rules, err)
		}
	}
	// Deletion is idempotent.
	if err := m.DeleteRepo(ctx, "r2"); err != nil {
		t.Fatalf("delete again: %v", err)
	}

	// --- token registry (docs/gateway/authorization.md §6): every metadata-owning store ---
	tr, ok := m.(TokenRegistry)
	if !ok {
		t.Fatal("metadata-owning store must implement TokenRegistry")
	}
	plain, err := NewTokenPlaintext()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(plain) <= len("banyan_") || plain[:7] != "banyan_" {
		t.Fatalf("plaintext shape = %q", plain)
	}
	tok := TokenInfo{Hash: HashToken(plain), User: "alice", Name: "ci", Scope: ScopeWrite, Created: 12345}
	if err := tr.PutToken(ctx, tok); err != nil {
		t.Fatalf("put token: %v", err)
	}
	if err := tr.PutToken(ctx, tok); err == nil {
		t.Fatal("duplicate token hash must fail")
	}
	bad := tok
	bad.Scope = "root"
	if tr.PutToken(ctx, bad) == nil {
		t.Fatal("invalid scope must fail")
	}
	got, err := tr.TokenByHash(ctx, HashToken(plain))
	if err != nil || got.User != "alice" || got.Scope != ScopeWrite || got.Revoked {
		t.Fatalf("by-hash = %+v, %v", got, err)
	}
	if _, err := tr.TokenByHash(ctx, HashToken("banyan_nope")); !errors.Is(err, ErrTokenUnknown) {
		t.Fatalf("unknown token error = %v, want ErrTokenUnknown", err)
	}
	if err := tr.PutToken(ctx, TokenInfo{Hash: HashToken("banyan_second"), User: "bob", Scope: ScopeRead, Created: 12346}); err != nil {
		t.Fatalf("put second: %v", err)
	}
	all, err := tr.ListTokens(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all = %v, %v", all, err)
	}
	mine, err := tr.ListTokens(ctx, "alice")
	if err != nil || len(mine) != 1 || mine[0].User != "alice" {
		t.Fatalf("list alice = %v, %v", mine, err)
	}
	if err := tr.RevokeToken(ctx, HashToken(plain)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := tr.RevokeToken(ctx, HashToken(plain)); err != nil {
		t.Fatalf("revoke idempotent: %v", err)
	}
	if got, _ := tr.TokenByHash(ctx, HashToken(plain)); !got.Revoked {
		t.Fatal("revoked flag must persist")
	}

	// --- subtree rule store (docs/gateway/authorization.md §8): every metadata-owning store ---
	rs, ok := m.(SubtreeRuleStore)
	if !ok {
		t.Fatal("metadata-owning store must implement SubtreeRuleStore")
	}
	if err := m.CreateRepo(ctx, "r3", model.SHA1); err != nil {
		t.Fatalf("create r3: %v", err)
	}
	if err := rs.SetSubtreeRule(ctx, "nope", SubtreeRule{Path: "/a", Users: []string{"u"}}); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("rule on unknown repo = %v, want ErrRepoNotFound", err)
	}
	if err := rs.SetSubtreeRule(ctx, "r3", SubtreeRule{Path: "a", Users: []string{"u"}}); err == nil {
		t.Fatal("relative path must fail")
	}
	if err := rs.SetSubtreeRule(ctx, "r3", SubtreeRule{Path: "/a/", Users: []string{"u"}}); err == nil {
		t.Fatal("trailing slash must fail")
	}
	if err := rs.SetSubtreeRule(ctx, "r3", SubtreeRule{Path: "/a/..", Users: []string{"u"}}); err == nil {
		t.Fatal("dot-dot segment must fail")
	}
	if err := rs.SetSubtreeRule(ctx, "r3", SubtreeRule{Path: "/a", Users: nil}); err == nil {
		t.Fatal("empty allowlist must fail")
	}
	if err := rs.SetSubtreeRule(ctx, "r3", SubtreeRule{Path: "/lib/deep", Users: []string{"bob", "alice"}}); err != nil {
		t.Fatalf("set rule: %v", err)
	}
	if err := rs.SetSubtreeRule(ctx, "r3", SubtreeRule{Path: "/lib", Users: []string{"alice"}}); err != nil {
		t.Fatalf("set rule 2: %v", err)
	}
	rules, err := rs.SubtreeRules(ctx, "r3")
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules = %v, %v", rules, err)
	}
	if rules[0].Path != "/lib" || rules[1].Path != "/lib/deep" {
		t.Fatalf("rules must be path-sorted: %+v", rules)
	}
	// Users are stored sorted (stable answers across backends).
	if got := rules[1].Users; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("users must round-trip sorted: %v", got)
	}
	// Upsert replaces one path without touching others.
	if err := rs.SetSubtreeRule(ctx, "r3", SubtreeRule{Path: "/lib", Users: []string{"carol"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if rules, _ = rs.SubtreeRules(ctx, "r3"); len(rules) != 2 {
		t.Fatalf("upsert must not duplicate: %+v", rules)
	} else if rules[0].Users[0] != "carol" || rules[1].Users[0] != "alice" {
		t.Fatalf("upsert result = %+v", rules)
	}
	// Delete is idempotent; the sibling rule survives.
	if err := rs.DeleteSubtreeRule(ctx, "r3", "/lib"); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	if err := rs.DeleteSubtreeRule(ctx, "r3", "/lib"); err != nil {
		t.Fatalf("delete idempotent: %v", err)
	}
	if rules, _ = rs.SubtreeRules(ctx, "r3"); len(rules) != 1 || rules[0].Path != "/lib/deep" {
		t.Fatalf("post-delete rules = %+v", rules)
	}
}

// TestSQLiteBackupRestore pins the online backup contract: VACUUM INTO
// while the store keeps working, then reopen the snapshot and every piece
// of state survives (the ops path for single-binary deployments, docs/storage/ha.md).
func TestSQLiteBackupRestore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteMetaStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := t.Context()
	root := model.Timeline{Repo: "bk", Anchor: "/", Name: "main"}
	if err := s.CreateRepo(ctx, "bk", model.SHA1); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPack(ctx, "bk", model.OID(strings.Repeat("a", 40)), []model.OID{model.OID(strings.Repeat("b", 40))}, []string{"n1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CASHead(ctx, root, "", model.OID(strings.Repeat("c", 40))); err != nil {
		t.Fatal(err)
	}

	bakPath := filepath.Join(dir, "backup.db")
	if err := s.Backup(bakPath); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := s.Backup(bakPath); err == nil {
		t.Fatal("backup onto an existing file must refuse")
	}

	r, err := NewSQLiteMetaStore(bakPath)
	if err != nil {
		t.Fatalf("reopen backup: %v", err)
	}
	defer r.Close()
	if _, err := r.GetRepo(ctx, "bk"); err != nil {
		t.Fatalf("repo lost: %v", err)
	}
	if _, err := r.GetPack(ctx, "bk", model.OID(strings.Repeat("a", 40))); err != nil {
		t.Fatalf("pack lost: %v", err)
	}
	st, err := r.Timeline(ctx, root)
	if err != nil || st.Head != model.OID(strings.Repeat("c", 40)) {
		t.Fatalf("timeline lost: %v %+v", err, st)
	}
	// The source keeps serving after the snapshot.
	if err := s.CASHead(ctx, root, model.OID(strings.Repeat("c", 40)), model.OID(strings.Repeat("d", 40))); err != nil {
		t.Fatal(err)
	}
}
