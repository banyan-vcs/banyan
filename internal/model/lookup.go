package model

import (
	"fmt"
)

// ObjectLookup 是模型层访问对象的抽象：由 storage 层实现
// （全局对象索引 → 分片节点 → pack/idx 解码，docs/02 §3）。
type ObjectLookup interface {
	Blob(oid OID) ([]byte, error)
	Tree(oid OID) (*Tree, error)
	Commit(oid OID) (*AnchoredCommit, error)
}

// ErrObjectNotFound 表示对象在全局索引与分片中均不存在。
var ErrObjectNotFound = fmt.Errorf("object not found")

// MemoryLookup 是进程内对象表，测试与引导用。
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

// PutBlob/ PutTree / PutCommit 存入并返回 OID。
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
