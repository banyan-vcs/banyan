// Package model defines banyan's core data model (docs/01-data-model.md):
//
//   - Object layer: blob/tree are immutable content-addressed objects, byte-for-byte git-compatible encoding;
//   - Commit layer: AnchoredCommit can anchor (anchor) a tree at any directory,
//     not just the repo root tree — git commit is the anchor="/" special case.
package model

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
)

// HashAlgo selects the object hash algorithm. SHA-1 is the default for maximal
// git interop (object-format=sha1); SHA-256 repos register object-format=sha256.
type HashAlgo int

const (
	SHA1 HashAlgo = iota
	SHA256
)

func (a HashAlgo) New() hash.Hash {
	if a == SHA256 {
		return sha256.New()
	}
	return sha1.New()
}

// OID is a lowercase hex object identifier (40 chars for SHA-1, 64 for SHA-256).
type OID string

func (o OID) IsZero() bool   { return o == "" }
func (o OID) String() string { return string(o) }
func (o OID) Bytes() []byte  { b, _ := hex.DecodeString(string(o)); return b }

// NormalizeZero canonicalizes all-zero IDs from the git wire protocol (old value
// when creating a ref, new value when deleting one) and the empty zero value to "".
func (o OID) NormalizeZero() OID {
	if o == "" {
		return ""
	}
	for i := 0; i < len(o); i++ {
		if o[i] != '0' {
			return o
		}
	}
	return ""
}

// ObjectType matches the git object types.
type ObjectType string

const (
	BlobType   ObjectType = "blob"
	TreeType   ObjectType = "tree"
	CommitType ObjectType = "commit"
	TagType    ObjectType = "tag"
)

// ObjectOID computes the git-compatible object ID: H("<type> <size>\0" || content).
// Identical input yields exactly git's OID — the foundation of the projection layer's zero-translation compatibility.
func ObjectOID(t ObjectType, content []byte, algo HashAlgo) OID {
	h := algo.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", t, len(content))))
	h.Write(content)
	return OID(hex.EncodeToString(h.Sum(nil)))
}

// Blob is the file-content object.
type Blob struct {
	Content []byte
}

func (b *Blob) OID(algo HashAlgo) OID { return ObjectOID(BlobType, b.Content, algo) }

// TreeEntry is a named tree entry, compatible with git tree encoding.
type TreeEntry struct {
	Name   string
	Mode   string // "100644" "100755" "120000" "40000" (dir) "160000" (gitlink)
	Target OID
	IsDir  bool
}

// Tree is an immutable directory object.
type Tree struct {
	Entries []TreeEntry
}

func (t *Tree) OID(algo HashAlgo) OID {
	b, err := t.Encode()
	if err != nil {
		return ""
	}
	return ObjectOID(TreeType, b, algo)
}

// Encode follows the git tree format: repeated "<mode> <name>\0<raw-oid>";
// entries sorted per git rules (directory names compared with '/' appended).
func (t *Tree) Encode() ([]byte, error) {
	entries := append([]TreeEntry(nil), t.Entries...)
	sortEntries(entries)
	out := make([]byte, 0, len(entries)*32)
	for _, e := range entries {
		if e.Name == "" {
			panic("encode tree: empty entry name")
		}
		mode := e.Mode
		if mode == "" {
			if e.IsDir {
				mode = "40000"
			} else {
				mode = "100644"
			}
		}
		raw, err := hex.DecodeString(string(e.Target))
		if err != nil {
			return nil, fmt.Errorf("tree entry %q: bad oid %q: %w", e.Name, e.Target, err)
		}
		out = append(out, mode...)
		out = append(out, ' ')
		out = append(out, e.Name...)
		out = append(out, 0)
		out = append(out, raw...)
	}
	return out, nil
}

// DecodeTree parses the git tree encoding.
func DecodeTree(b []byte) (*Tree, error) {
	t := &Tree{}
	for len(b) > 0 {
		sp := indexByte(b, ' ')
		if sp <= 0 {
			return nil, fmt.Errorf("decode tree: malformed mode")
		}
		mode := string(b[:sp])
		b = b[sp+1:]
		nul := indexByte(b, 0)
		if nul <= 0 {
			return nil, fmt.Errorf("decode tree: malformed name; head=%q mode=%q", b[:min(32, len(b))], mode)
		}
		name := string(b[:nul])
		b = b[nul+1:]
		oidLen := 20 // SHA-1 raw length
		if len(b) < oidLen {
			return nil, fmt.Errorf("decode tree: truncated oid")
		}
		// SHA-256 repos use 32-byte raw oids; they cannot be reliably distinguished
		// from remaining data, so callers of DecodeTree must pass encodings of the matching algo (see ObjectLookup).
		t.Entries = append(t.Entries, TreeEntry{
			Name:   name,
			Mode:   mode,
			Target: OID(hex.EncodeToString(b[:oidLen])),
			IsDir:  mode == "40000",
		})
		b = b[oidLen:]
	}
	return t, nil
}

// sortEntries sorts in git tree order: directories are treated as names with a trailing '/'.
func sortEntries(entries []TreeEntry) {
	less := func(i, j int) bool {
		ni, nj := entries[i].Name, entries[j].Name
		if entries[i].IsDir {
			ni += "/"
		}
		if entries[j].IsDir {
			nj += "/"
		}
		return ni < nj
	}
	// Insertion sort suffices (skeleton stage, limited tree entries); M1 switches to sort.Slice.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
