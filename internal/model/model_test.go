package model

import (
	"strings"
	"testing"
	"time"
)

func ident(name string) Identity {
	return Identity{Name: name, Email: name + "@banyan.dev", When: time.Unix(1760000000, 0).UTC()}
}

func mkTree(t *testing.T, lookup *MemoryLookup, entries ...TreeEntry) OID {
	t.Helper()
	return lookup.PutTree(&Tree{Entries: entries})
}

func file(t *testing.T, lookup *MemoryLookup, name, content string) TreeEntry {
	t.Helper()
	return TreeEntry{Name: name, Mode: "100644", Target: lookup.PutBlob([]byte(content))}
}

func TestRootAnchorCommitIsByteIdenticalToGit(t *testing.T) {
	// A commit with anchor="/" must carry no banyan headers — the foundation of zero-translation git compatibility.
	c := &AnchoredCommit{
		Anchor:    RootAnchor,
		TreeOID:   "0123456789012345678901234567890123456789",
		Parents:   []OID{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Author:    ident("han"),
		Committer: ident("han"),
		Message:   "initial\n",
	}
	raw := string(c.Encode())
	if strings.Contains(raw, "banyan-") {
		t.Fatalf("root-anchor commit leaked banyan headers:\n%s", raw)
	}
	for _, want := range []string{"tree 012345", "parent aaaaaaaa", "author han <han@banyan.dev>"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in:\n%s", want, raw)
		}
	}
}

func TestAnchoredCommitRoundTrip(t *testing.T) {
	c := &AnchoredCommit{
		Anchor:        "/src/lib",
		TreeOID:       "0123456789012345678901234567890123456789",
		Parents:       []OID{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Author:        ident("han"),
		Committer:     ident("han"),
		Message:       "subdir commit\n",
		ProjectedFrom: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Synthetic:     true,
	}
	got, err := DecodeCommit(c.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Anchor != c.Anchor || got.TreeOID != c.TreeOID || got.Synthetic != true ||
		got.ProjectedFrom != c.ProjectedFrom || len(got.Parents) != 1 || got.Parents[0] != c.Parents[0] {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestAnchorPath(t *testing.T) {
	if _, err := ParseAnchorPath("/a/b/"); err == nil {
		t.Fatal("trailing slash should fail")
	}
	if _, err := ParseAnchorPath("/a/../b"); err == nil {
		t.Fatal(".. should fail")
	}
	p, err := ParseAnchorPath("/src/lib")
	if err != nil {
		t.Fatal(err)
	}
	if p.Parent() != AnchorPath("/src") || AnchorPath("/src").Parent() != RootAnchor {
		t.Fatalf("parent chain wrong: %s", p.Parent())
	}
}

func TestReplaceSubtreeAssemblesRoot(t *testing.T) {
	lookup := NewMemoryLookup(SHA1)
	// Root tree: old lib content + docs.
	oldLib := mkTree(t, lookup, file(t, lookup, "a.go", "old"))
	docs := mkTree(t, lookup, file(t, lookup, "readme.md", "hi"))
	root := mkTree(t, lookup,
		TreeEntry{Name: "lib", Mode: "40000", Target: oldLib, IsDir: true},
		TreeEntry{Name: "docs", Mode: "40000", Target: docs, IsDir: true},
	)
	// The subdirectory's new tree: one commit on the src/lib axis.
	newLib := mkTree(t, lookup, file(t, lookup, "a.go", "new"), file(t, lookup, "b.go", "added"))

	got, err := ReplaceSubtree(lookup, SHA1, root, "/lib", newLib)
	if err != nil {
		t.Fatal(err)
	}
	// docs must be preserved untouched.
	sub := SubtreeAt(lookup, got, "/docs")
	if err != nil || sub != docs {
		t.Fatalf("docs disturbed: %v %v", sub, err)
	}
	sub = SubtreeAt(lookup, got, "/lib")
	if err != nil || sub != newLib {
		t.Fatalf("lib not replaced: %v %v", sub, err)
	}
	// Idempotence: replacing the same subtree again leaves the root OID unchanged.
	again, err := ReplaceSubtree(lookup, SHA1, got, "/lib", newLib)
	if err != nil || again != got {
		t.Fatalf("not idempotent: %v vs %v (%v)", again, got, err)
	}
}

func TestSplitHistoryProjectsSubtree(t *testing.T) {
	lookup := NewMemoryLookup(SHA1)
	v1 := mkTree(t, lookup, file(t, lookup, "a.go", "v1"))
	v2 := mkTree(t, lookup, file(t, lookup, "a.go", "v2"))
	v3 := mkTree(t, lookup, file(t, lookup, "a.go", "v3"))
	rootV1 := mkTree(t, lookup, TreeEntry{Name: "lib", Mode: "40000", Target: v1, IsDir: true})
	rootV2 := mkTree(t, lookup, TreeEntry{Name: "lib", Mode: "40000", Target: v2, IsDir: true})
	rootV3 := mkTree(t, lookup, TreeEntry{Name: "lib", Mode: "40000", Target: v3, IsDir: true})

	c1 := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootV1, Author: ident("a"), Committer: ident("a"), Message: "one\n"})
	c2 := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootV2, Parents: []OID{c1}, Author: ident("a"), Committer: ident("a"), Message: "two\n"})
	// A third commit also changes lib — only a length-3 chain locks down the
	// parent backfill order (ascending backfill once mutated a middle element's OID after it was referenced).
	c3 := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootV3, Parents: []OID{c2}, Author: ident("a"), Committer: ident("a"), Message: "three\n"})

	res, err := SplitHistory(lookup, SHA1, c3, "/lib")
	if err != nil {
		t.Fatal(err)
	}
	chain := res.Commits
	if len(chain) != 3 {
		t.Fatalf("want 3 projected commits, got %d", len(chain))
	}
	if chain[0].TreeOID != v3 || chain[1].TreeOID != v2 || chain[2].TreeOID != v1 {
		t.Fatalf("projection order wrong: %+v", chain)
	}
	if res.Head != chain[0].OID(SHA1) {
		t.Fatalf("head %s != newest %s", res.Head, chain[0].OID(SHA1))
	}
	// Purity: recomputation is byte-identical (stable SHA ⇒ clients can cache/negotiate).
	again, _ := SplitHistory(lookup, SHA1, c3, "/lib")
	if chain[0].OID(SHA1) != again.Commits[0].OID(SHA1) ||
		chain[1].OID(SHA1) != again.Commits[1].OID(SHA1) ||
		res.Head != again.Head {
		t.Fatal("split history not deterministic")
	}
	// Parent-link invariant: every parent must equal exactly some chain element's
	// final OID (regression guard: incremental building once produced stale-hash ghost pointers).
	oids := map[OID]bool{}
	for _, pc := range chain {
		oids[pc.OID(SHA1)] = true
	}
	for _, pc := range chain {
		for _, p := range pc.Parents {
			if !oids[p] {
				t.Fatalf("parent %s not materialized in chain", p)
			}
		}
	}
	if parents := chain[len(chain)-1].Parents; len(parents) != 0 {
		t.Fatalf("oldest projected commit must have no parents, got %v", parents)
	}
	// Projected commits carry a provenance trailer.
	if chain[2].ProjectedFrom != c1 {
		t.Fatalf("projected-from wrong: %v", chain[2].ProjectedFrom)
	}
}

func TestSplitHistoryPreservesMergeTopology(t *testing.T) {
	lookup := NewMemoryLookup(SHA1)
	libV1 := mkTree(t, lookup, file(t, lookup, "a.go", "v1"))
	libV2 := mkTree(t, lookup, file(t, lookup, "a.go", "v2"))

	rootV1 := mkTree(t, lookup, TreeEntry{Name: "lib", Mode: "40000", Target: libV1, IsDir: true})
	c1 := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootV1, Author: ident("a"), Committer: ident("a"), Message: "base\n"})

	// Branch: change lib.
	rootV2 := mkTree(t, lookup, TreeEntry{Name: "lib", Mode: "40000", Target: libV2, IsDir: true})
	cBranch := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootV2, Parents: []OID{c1}, Author: ident("b"), Committer: ident("b"), Message: "branch: lib v2\n"})

	// Main: lib untouched (README change).
	rootMain := mkTree(t, lookup,
		file(t, lookup, "README", "x"),
		TreeEntry{Name: "lib", Mode: "40000", Target: libV1, IsDir: true})
	cMain := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootMain, Parents: []OID{c1}, Author: ident("m"), Committer: ident("m"), Message: "main: readme\n"})

	// Merge: lib=v2 + README.
	mergedRoot := mkTree(t, lookup,
		file(t, lookup, "README", "x"),
		TreeEntry{Name: "lib", Mode: "40000", Target: libV2, IsDir: true})
	cMerge := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: mergedRoot, Parents: []OID{cMain, cBranch}, Author: ident("m"), Committer: ident("m"), Message: "merge branch\n"})

	res, err := SplitHistory(lookup, SHA1, cMerge, "/lib")
	if err != nil {
		t.Fatal(err)
	}
	// Change set: c1 (root), cBranch (lib v2), cMerge (changed vs cMain).
	// cMain is untouched (its lib equals its parent c1's).
	if len(res.Commits) != 3 {
		t.Fatalf("want 3 projected commits (base/branch/merge), got %d: %+v", len(res.Commits), res.Commits)
	}
	byFrom := map[OID]*AnchoredCommit{}
	for _, pc := range res.Commits {
		byFrom[pc.ProjectedFrom] = pc
	}
	pMerge, pBranch, pBase := byFrom[cMerge], byFrom[cBranch], byFrom[c1]
	if pMerge == nil || pBranch == nil || pBase == nil {
		t.Fatalf("missing projections: merge=%v branch=%v base=%v", pMerge != nil, pBranch != nil, pBase != nil)
	}
	// The merge point's two parent edges resolve to proj(base) (via cMain) and proj(branch).
	if len(pMerge.Parents) != 2 {
		t.Fatalf("merge projection must keep 2 parents, got %v", pMerge.Parents)
	}
	wantBase, wantBranch := pBase.OID(SHA1), pBranch.OID(SHA1)
	gotA, gotB := pMerge.Parents[0], pMerge.Parents[1]
	if (gotA != wantBase || gotB != wantBranch) && (gotA != wantBranch || gotB != wantBase) {
		t.Fatalf("merge parents = [%s %s], want [%s %s]", gotA, gotB, wantBase, wantBranch)
	}
	if res.Head != pMerge.OID(SHA1) {
		t.Fatalf("head = %s, want merge projection %s", res.Head, pMerge.OID(SHA1))
	}
}

func TestAssembleRootProducesSyntheticCommit(t *testing.T) {
	lookup := NewMemoryLookup(SHA1)
	libV1 := mkTree(t, lookup, file(t, lookup, "a.go", "v1"))
	rootV1 := mkTree(t, lookup, TreeEntry{Name: "lib", Mode: "40000", Target: libV1, IsDir: true})
	r1 := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootV1, Author: ident("a"), Committer: ident("a"), Message: "root\n"})

	heads := &stubHeads{r1}
	libV2 := mkTree(t, lookup, file(t, lookup, "a.go", "v2"))
	l2 := lookup.PutCommit(&AnchoredCommit{Anchor: "/lib", TreeOID: libV2, Author: ident("b"), Committer: ident("b"), Message: "subdir\n"})

	syn, err := AssembleRoot(lookup, heads, SHA1, "demo", "main", "/lib", l2, ident("banyan"))
	if err != nil {
		t.Fatal(err)
	}
	if !syn.Synthetic || syn.Parents[0] != r1 || syn.Anchor != RootAnchor {
		t.Fatalf("synthetic commit wrong: %+v", syn)
	}
	sub := SubtreeAt(lookup, syn.TreeOID, "/lib")
	if err != nil || sub != libV2 {
		t.Fatalf("assembled root missing new lib: %v %v", sub, err)
	}
}

type stubHeads struct{ head OID }

func (s *stubHeads) TimelineHead(_ Timeline) (TimelineState, error) {
	return TimelineState{Head: s.head}, nil
}
