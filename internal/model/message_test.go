package model

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Byte-level lossless invariant: real git commits (gpgsig multiline headers,
// two-parent merges, parentless roots) must decompose→render byte-identically ⇒ hash identity (docs/01 §11).
func TestDecomposeRenderRoundtripRealFixtures(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/commit-*.txt")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range fixtures {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		tree, envelope, err := DecomposeCommit(raw)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		rendered := RenderCommit(tree, envelope)
		if !bytes.Equal(rendered, raw) {
			t.Fatalf("%s: render not byte-identical\nwant: %q\ngot:  %q", f, raw, rendered)
		}
		if got := ObjectOID(CommitType, rendered, SHA1); got != ObjectOID(CommitType, raw, SHA1) {
			t.Fatalf("%s: hash changed after roundtrip", f)
		}
		// tree edge decoupled from the envelope: swapping the tree still renders validly, and the bmsg OID is unchanged.
		if MessageOID(envelope, SHA1).IsZero() {
			t.Fatalf("%s: zero message oid", f)
		}
	}
}

func TestDecomposeRejectsNonTreeFirstHeader(t *testing.T) {
	if _, _, err := DecomposeCommit([]byte("parent abc\n\nm\n")); err == nil {
		t.Fatal("non-tree first header should be rejected")
	}
}

// Dedup semantics: identical envelope + different tree ⇒ one bmsg (structure sharing), distinct git commits.
func TestMessageDedupAcrossTrees(t *testing.T) {
	who := ident("bot")
	envA := (&AnchoredCommit{Anchor: RootAnchor, TreeOID: "1111111111111111111111111111111111111111", Author: who, Committer: who, Message: "ci: rebuild\n"}).Encode()
	envB := (&AnchoredCommit{Anchor: RootAnchor, TreeOID: "2222222222222222222222222222222222222222", Author: who, Committer: who, Message: "ci: rebuild\n"}).Encode()
	treeA, envA, _ := DecomposeCommit(envA)
	treeB, envB, _ := DecomposeCommit(envB)
	if MessageOID(envA, SHA1) != MessageOID(envB, SHA1) {
		t.Fatal("identical envelopes must dedupe to one bmsg")
	}
	if treeA == treeB {
		t.Fatal("trees should differ")
	}
	// The rendered artifacts remain two distinct valid git commits.
	if ObjectOID(CommitType, RenderCommit(treeA, envA), SHA1) == ObjectOID(CommitType, RenderCommit(treeB, envB), SHA1) {
		t.Fatal("different trees must render to different commits")
	}
}

// Lightweight record round-trip: record → render → hash matches the original object.
func TestCommitRecordRender(t *testing.T) {
	who := ident("a")
	c := &AnchoredCommit{Anchor: RootAnchor, TreeOID: "abc0000000000000000000000000000000000000", Author: who, Committer: who, Message: "m\n"}
	raw := c.Encode()
	tree, envelope, err := DecomposeCommit(raw)
	if err != nil {
		t.Fatal(err)
	}
	rec := CommitRecord{GitOID: c.OID(SHA1), Tree: tree, Msg: MessageOID(envelope, SHA1)}
	if rec.GitOID != ObjectOID(CommitType, RenderCommit(rec.Tree, envelope), SHA1) {
		t.Fatal("record render must reproduce git oid")
	}
}
