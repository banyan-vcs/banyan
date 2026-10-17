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
	// anchor="/" 的 commit 不得携带任何 banyan 头——git 零翻译兼容的根基。
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
	// 根树：lib 旧内容 + docs。
	oldLib := mkTree(t, lookup, file(t, lookup, "a.go", "old"))
	docs := mkTree(t, lookup, file(t, lookup, "readme.md", "hi"))
	root := mkTree(t, lookup,
		TreeEntry{Name: "lib", Mode: "40000", Target: oldLib, IsDir: true},
		TreeEntry{Name: "docs", Mode: "40000", Target: docs, IsDir: true},
	)
	// 子目录新树：src/lib 维度的一次提交。
	newLib := mkTree(t, lookup, file(t, lookup, "a.go", "new"), file(t, lookup, "b.go", "added"))

	got, err := ReplaceSubtree(lookup, SHA1, root, "/lib", newLib)
	if err != nil {
		t.Fatal(err)
	}
	// docs 必须原样保留。
	sub := SubtreeAt(lookup, got, "/docs")
	if err != nil || sub != docs {
		t.Fatalf("docs disturbed: %v %v", sub, err)
	}
	sub = SubtreeAt(lookup, got, "/lib")
	if err != nil || sub != newLib {
		t.Fatalf("lib not replaced: %v %v", sub, err)
	}
	// 幂等性：再替换一次同一子树，根 OID 不变。
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
	// 第三个提交同样变更 lib——链长 3 才能锁住 parent 回填顺序
	// （升序回填曾让中间元素的 OID 在被引用后发生变化）。
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
	// 纯函数性：重算结果逐字节一致（SHA 稳定 ⇒ 客户端可缓存/协商）。
	again, _ := SplitHistory(lookup, SHA1, c3, "/lib")
	if chain[0].OID(SHA1) != again.Commits[0].OID(SHA1) ||
		chain[1].OID(SHA1) != again.Commits[1].OID(SHA1) ||
		res.Head != again.Head {
		t.Fatal("split history not deterministic")
	}
	// parent 链接不变式：每个 parent 必须精确等于链内某元素的最终 OID
	// （回归防护：增量建链曾产生过期哈希的幽灵指针）。
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
	// 投影 commit 带溯源 trailer。
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

	// 分支：改 lib。
	rootV2 := mkTree(t, lookup, TreeEntry{Name: "lib", Mode: "40000", Target: libV2, IsDir: true})
	cBranch := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootV2, Parents: []OID{c1}, Author: ident("b"), Committer: ident("b"), Message: "branch: lib v2\n"})

	// 主干：不动 lib（README 变更）。
	rootMain := mkTree(t, lookup,
		file(t, lookup, "README", "x"),
		TreeEntry{Name: "lib", Mode: "40000", Target: libV1, IsDir: true})
	cMain := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: rootMain, Parents: []OID{c1}, Author: ident("m"), Committer: ident("m"), Message: "main: readme\n"})

	// 合并：lib=v2 + README。
	mergedRoot := mkTree(t, lookup,
		file(t, lookup, "README", "x"),
		TreeEntry{Name: "lib", Mode: "40000", Target: libV2, IsDir: true})
	cMerge := lookup.PutCommit(&AnchoredCommit{Anchor: RootAnchor, TreeOID: mergedRoot, Parents: []OID{cMain, cBranch}, Author: ident("m"), Committer: ident("m"), Message: "merge branch\n"})

	res, err := SplitHistory(lookup, SHA1, cMerge, "/lib")
	if err != nil {
		t.Fatal(err)
	}
	// 变更集：c1（根）、cBranch（lib v2）、cMerge（相对 cMain 变化）。
	// cMain 不变（lib 与其父 c1 相同）。
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
	// 合并点的两条 parent 边分别解析到 proj(base)（经 cMain）与 proj(branch)。
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
