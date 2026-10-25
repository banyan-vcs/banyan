package model

import "testing"

// Unified DAG primitive tests (docs/02 §10): view tip determinism; adoption-based assembly preserves the child commit.
func TestSubtreeTipDeterministic(t *testing.T) {
	lookup := NewMemoryLookup(SHA1)
	lib := mkTree(t, lookup, file(t, lookup, "a.go", "v1"))
	root := mkTree(t, lookup,
		file(t, lookup, "README", "r"),
		TreeEntry{Name: "lib", Mode: "40000", Target: lib, IsDir: true})
	rc := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: root, Author: ident("a"), Committer: ident("a"), Message: "root\n"})

	tip1, err := SubtreeTip(lookup, SHA1, rc, "/lib")
	if err != nil {
		t.Fatal(err)
	}
	tip2, _ := SubtreeTip(lookup, SHA1, rc, "/lib")
	if tip1.OID(SHA1) != tip2.OID(SHA1) {
		t.Fatal("view tip not deterministic")
	}
	if tip1.TreeOID != lib {
		t.Fatalf("tip tree = %s, want subtree %s", tip1.TreeOID, lib)
	}
	if len(tip1.Parents) != 1 || tip1.Parents[0] != rc {
		t.Fatalf("tip parent = %v, want [%s]", tip1.Parents, rc)
	}
	// Pure git object: carries no banyan-private headers.
	raw := string(tip1.Encode())
	if containsAny(raw, "banyan-anchor", "banyan-projected-from", "banyan-synthetic") {
		t.Fatalf("tip carries banyan headers:\n%s", raw)
	}
}

func TestSubtreeTipAbsentPath(t *testing.T) {
	lookup := NewMemoryLookup(SHA1)
	root := mkTree(t, lookup, file(t, lookup, "README", "r"))
	rc := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: root, Author: ident("a"), Committer: ident("a"), Message: "m\n"})
	if _, err := SubtreeTip(lookup, SHA1, rc, "/nope"); err == nil {
		t.Fatal("absent path should error")
	}
}

func TestAdoptSubtreePushPreservesChild(t *testing.T) {
	lookup := NewMemoryLookup(SHA1)
	libV1 := mkTree(t, lookup, file(t, lookup, "a.go", "v1"))
	rootV1 := mkTree(t, lookup,
		file(t, lookup, "README", "r"),
		TreeEntry{Name: "lib", Mode: "40000", Target: libV1, IsDir: true})
	r1 := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootV1, Author: ident("a"), Committer: ident("a"), Message: "root\n"})

	// Subtree-side commit C (root tree from the view's perspective = lib contents).
	libV2 := mkTree(t, lookup, file(t, lookup, "a.go", "v2"))
	c := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: libV2, Author: ident("b"), Committer: ident("b"), Message: "subtree change\n"})

	heads := &stubHeads{r1}
	merge, err := AdoptSubtreePush(lookup, SHA1, "demo", "main", "/lib", c, heads)
	if err != nil {
		t.Fatal(err)
	}
	// Determinism (signature/message all derive from immutable objects).
	merge2, _ := AdoptSubtreePush(lookup, SHA1, "demo", "main", "/lib", c, heads)
	if merge.OID(SHA1) != merge2.OID(SHA1) {
		t.Fatal("merge commit not deterministic")
	}
	// Two parents: [root head, C kept verbatim] — the key to C's hash being identical across views.
	if len(merge.Parents) != 2 || merge.Parents[0] != r1 || merge.Parents[1] != c {
		t.Fatalf("merge parents = %v, want [%s %s]", merge.Parents, r1, c)
	}
	// Merge tree: lib in the root tree replaced by C's tree, other entries preserved.
	sub := SubtreeAt(lookup, merge.TreeOID, "/lib")
	if sub != libV2 {
		t.Fatalf("merge tree lib = %v, want %s", sub, libV2)
	}
	mergeTree, terr := lookup.Tree(merge.TreeOID)
	if terr != nil {
		t.Fatal(terr)
	}
	foundREADME := false
	for _, e := range mergeTree.Entries {
		if e.Name == "README" {
			foundREADME = true
		}
	}
	if !foundREADME {
		t.Fatal("merge tree lost README")
	}
	// Signature reused from C; pure git object with no private headers.
	if merge.Author.Name != "b" {
		t.Fatalf("merge author = %+v, want C's", merge.Author)
	}
	if containsAny(string(merge.Encode()), "banyan-anchor", "banyan-projected-from", "banyan-synthetic") {
		t.Fatal("merge carries banyan headers")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// v4 identity mapping (docs/02 §12): the split projection of an adoption merge
// M=[rootPrev, C] (C scoped to X, tree-equivalent) = C itself — no new commit.
func TestSplitHistoryIdentityMappingForAdoption(t *testing.T) {
	lookup := NewMemoryLookup(SHA1)
	nestedV1 := mkTree(t, lookup, file(t, lookup, "SHARED.txt", "base\n"))
	rootTree := mkTree(t, lookup,
		file(t, lookup, "README", "r"),
		TreeEntry{Name: "lib", Mode: "40000", Target: mkTree(t, lookup,
			file(t, lookup, "a.txt", "a"),
			TreeEntry{Name: "nested", Mode: "40000", Target: nestedV1, IsDir: true},
		), IsDir: true},
	)
	seed := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootTree, Author: ident("a"), Committer: ident("a"), Message: "seed\n"})

	// C: X-scoped tree, no parents (first push anchors on the old projection; simplified here as parentless).
	nestedV2 := mkTree(t, lookup, file(t, lookup, "SHARED.txt", "base\nline\n"))
	cX := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: nestedV2, Author: ident("u"), Committer: ident("u"), Message: "sub: edit\n"})

	// Adoption merge M=[seed, C]: ReplaceSubtree semantics.
	newRoot := mkTree(t, lookup,
		file(t, lookup, "README", "r"),
		TreeEntry{Name: "lib", Mode: "40000", Target: mkTree(t, lookup,
			file(t, lookup, "a.txt", "a"),
			TreeEntry{Name: "nested", Mode: "40000", Target: nestedV2, IsDir: true},
		), IsDir: true},
	)
	m := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: newRoot, Parents: []OID{seed, cX}, Author: ident("u"), Committer: ident("u"), Message: "Merge subtree '/lib/nested': sub: edit\n"})

	res, err := SplitHistory(lookup, SHA1, m, "/lib/nested")
	if err != nil {
		t.Fatal(err)
	}
	// Identity mapping: no new projection of M should appear in the chain; head's tree = C's tree.
	for _, pc := range res.Commits {
		if pc.ProjectedFrom == m {
			t.Fatalf("收编 merge 被投影为新提交（身份映射未生效）: %+v", pc)
		}
	}
	if res.Head == "" || res.Head == m {
		t.Fatalf("head 异常: %s", res.Head)
	}
	// head should be C itself (C's X tree == SubtreeAt(M.tree, X)).
	headCommit, err := lookup.Commit(res.Head)
	if err != nil {
		t.Fatal(err)
	}
	if headCommit.TreeOID != nestedV2 {
		t.Fatalf("head 树 = %s, want C 的树 %s", headCommit.TreeOID, nestedV2)
	}
}
