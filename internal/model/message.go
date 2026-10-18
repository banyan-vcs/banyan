package model

import (
	"bytes"
	"fmt"
	"strings"
)

// Message objects and lightweight commits (design v3, docs/01 §11).
//
// A commit's source information is split in two:
//
//	envelope = everything past the tree line: parents, author, committer,
//	extra headers (gpgsig/mergetag and other multiline headers), message body —
//	stored as an independent content-addressed Message object (type tag bmsg,
//	banyan-internal only, never on the git wire); lightweight record = two fixed pointers (tree edge, bmsg ref).
//
// The git commit object thereby becomes **a runtime render artifact**:
//
//	render(tree, envelope) = "tree <oid>\n" + envelope   — byte-exact round-trip.
//
//	For push-adopted commits the envelope is kept verbatim ⇒ render identity
//	⇒ hash identity; for native banyan commits (view tip / adoption merge,
//	plus future snapshots and stacked changes) rendering is deterministic (§10 pattern generalized).
//
// Gains: the commit record degenerates to a fixed-width graph edge (the
// CommitGraph's native shape); envelopes dedupe by content addressing (bot or
// templated merge messages repeat heavily in monorepos); a metadata-only
// amend adds one bmsg and touches no structure.
//
// Invariant: Decompose∘Render = identity (byte-level); bmsg and git object spaces stay disjoint by type tag.

// DecomposeCommit splits raw git commit bytes into (tree, envelope).
// The envelope = all bytes after the first tree line, verbatim (incl. header
// continuation lines and blank lines). A non-tree first line errors — guarding
// against non-canonical objects; callers fall back to storing it whole.
func DecomposeCommit(raw []byte) (OID, []byte, error) {
	i := bytes.IndexByte(raw, '\n')
	if i < 0 {
		return "", nil, fmt.Errorf("decompose commit: no header terminator")
	}
	first := raw[:i]
	if !bytes.HasPrefix(first, []byte("tree ")) {
		return "", nil, fmt.Errorf("decompose commit: first header %q is not tree", first)
	}
	tree := OID(strings.TrimSpace(string(first[5:])))
	if tree.IsZero() {
		return "", nil, fmt.Errorf("decompose commit: empty tree oid")
	}
	return tree, raw[i+1:], nil
}

// RenderCommit renders a git commit at runtime: tree line + envelope, byte-exact round-trip.
func RenderCommit(tree OID, envelope []byte) []byte {
	out := make([]byte, 0, len(envelope)+len(tree)+6)
	out = append(out, "tree "...)
	out = append(out, tree...)
	out = append(out, '\n')
	return append(out, envelope...)
}

// MessageOID computes the envelope's bmsg object ID: same git hashing scheme
// but type tag bmsg — naturally disjoint from the blob/tree/commit OID space.
func MessageOID(envelope []byte, algo HashAlgo) OID {
	return ObjectOID("bmsg", envelope, algo)
}

// CommitRecord is a lightweight commit record: git oid ↔ (tree edge, bmsg ref).
type CommitRecord struct {
	GitOID OID // git object ID of the rendered artifact
	Tree   OID // tree edge (the commit's core semantics)
	Msg    OID // bmsg reference (all metadata)
}
