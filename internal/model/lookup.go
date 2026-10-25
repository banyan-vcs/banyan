package model

import (
	"fmt"
)

// ObjectLookup abstracts object access for the model layer, implemented by the
// storage layer (global object index → shard node → pack/idx decode, docs/03 §3).
type ObjectLookup interface {
	Blob(oid OID) ([]byte, error)
	Tree(oid OID) (*Tree, error)
	Commit(oid OID) (*AnchoredCommit, error)
}

// ErrObjectNotFound means the object exists in neither the global index nor any shard.
var ErrObjectNotFound = fmt.Errorf("object not found")

// MemoryLookup is an in-process object table, for tests and bootstrapping.
type MemoryLookup struct {
	Blobs   map[OID][]byte
	Trees   map[OID]*Tree
	Commits map[OID]*AnchoredCommit
	Algo    HashAlgo
}

func NewMemoryLookup(algo HashAlgo) *MemoryLookup {
	return &MemoryLookup{
		Blobs:   map[OID][]byte{},
		Trees:   map[OID]*Tree{},
		Commits: map[OID]*AnchoredCommit{},
		Algo:    algo,
	}
}

func (m *MemoryLookup) Blob(oid OID) ([]byte, error) {
	if b, ok := m.Blobs[oid]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("%w: blob %s", ErrObjectNotFound, oid)
}

func (m *MemoryLookup) Tree(oid OID) (*Tree, error) {
	if t, ok := m.Trees[oid]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("%w: tree %s", ErrObjectNotFound, oid)
}

func (m *MemoryLookup) Commit(oid OID) (*AnchoredCommit, error) {
	if c, ok := m.Commits[oid]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("%w: commit %s", ErrObjectNotFound, oid)
}

// PutBlob / PutTree / PutCommit store the object and return its OID.
func (m *MemoryLookup) PutBlob(content []byte) OID {
	oid := ObjectOID(BlobType, content, m.Algo)
	m.Blobs[oid] = content
	return oid
}

func (m *MemoryLookup) PutTree(t *Tree) OID {
	oid := t.OID(m.Algo)
	m.Trees[oid] = t
	return oid
}

func (m *MemoryLookup) PutCommit(c *AnchoredCommit) OID {
	oid := c.OID(m.Algo)
	m.Commits[oid] = c
	return oid
}
