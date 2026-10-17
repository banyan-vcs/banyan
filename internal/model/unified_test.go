package model

import "testing"

// 统一 DAG 原语测试（docs/01 §10）：视图 tip 确定性、收编式组装保留子提交。
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
	// 纯 git 对象：不携带任何 banyan 私有头。
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

	// 子树侧提交 C（视图视角的根树 = lib 内容）。
	libV2 := mkTree(t, lookup, file(t, lookup, "a.go", "v2"))
	c := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: libV2, Author: ident("b"), Committer: ident("b"), Message: "subtree change\n"})

	heads := &stubHeads{r1}
	merge, err := AdoptSubtreePush(lookup, SHA1, "demo", "main", "/lib", c, heads)
	if err != nil {
		t.Fatal(err)
	}
	// 确定性（签名/消息全部源自不可变对象）。
	merge2, _ := AdoptSubtreePush(lookup, SHA1, "demo", "main", "/lib", c, heads)
	if merge.OID(SHA1) != merge2.OID(SHA1) {
		t.Fatal("merge commit not deterministic")
	}
	// 双父：[根 head, C 原样保留]——C 的 hash 跨视图一致的关键。
	if len(merge.Parents) != 2 || merge.Parents[0] != r1 || merge.Parents[1] != c {
		t.Fatalf("merge parents = %v, want [%s %s]", merge.Parents, r1, c)
	}
	// merge 的树：根树中 lib 替换为 C 的树，其余条目保留。
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
	// 签名沿用 C；纯 git 对象无私有头。
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
