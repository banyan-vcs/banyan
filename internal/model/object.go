// Package model 定义 banyan 的核心数据模型（docs/01-data-model.md）：
//
//   - 对象层：blob/tree 为不可变内容寻址对象，编码与 git 逐字节兼容；
//   - 提交层：AnchoredCommit 可锚定（anchor）任意目录的 tree，
//     而非仅限仓库根树——git commit 是 anchor="/" 的特例。
package model

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
)

// HashAlgo 选择对象哈希算法。默认 SHA-1 以获得最大 git 互操作性
// （object-format=sha1）；SHA-256 仓库以 object-format=sha256 注册。
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

// OID 是小写十六进制对象标识（SHA-1 为 40 字符，SHA-256 为 64 字符）。
type OID string

func (o OID) IsZero() bool    { return o == "" }
func (o OID) String() string  { return string(o) }
func (o OID) Bytes() []byte   { b, _ := hex.DecodeString(string(o)); return b }

// NormalizeZero 把 git 线上协议中的全零 ID（新建 ref 的 old 值、删除 ref
// 的 new 值）与空零值统一归一为 ""。
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

// ObjectType 与 git 对象类型一致。
type ObjectType string

const (
	BlobType   ObjectType = "blob"
	TreeType   ObjectType = "tree"
	CommitType ObjectType = "commit"
	TagType    ObjectType = "tag"
)

// ObjectOID 计算 git 兼容的对象 ID：H("<type> <size>\0" || content)。
// 相同输入得到与 git 完全相同的 OID——这是投影层零翻译兼容的地基。
func ObjectOID(t ObjectType, content []byte, algo HashAlgo) OID {
	h := algo.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", t, len(content))))
	h.Write(content)
	return OID(hex.EncodeToString(h.Sum(nil)))
}

// Blob 是文件内容对象。
type Blob struct {
	Content []byte
}

func (b *Blob) OID(algo HashAlgo) OID { return ObjectOID(BlobType, b.Content, algo) }

// TreeEntry 是树的一条命名条目，与 git tree 编码兼容。
type TreeEntry struct {
	Name   string
	Mode   string // "100644" "100755" "120000" "40000"(目录) "160000"(gitlink)
	Target OID
	IsDir  bool
}

// Tree 是不可变目录对象。
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

// Encode 按 git tree 格式编码："<mode> <name>\0<raw-oid>" 重复；
// 条目按 git 规则排序（目录名按追加 '/' 后比较）。
func (t *Tree) Encode() ([]byte, error) {
	entries := append([]TreeEntry(nil), t.Entries...)
	sortEntries(entries)
	out := make([]byte, 0, len(entries)*32)
	for _, e := range entries {
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

// DecodeTree 解析 git tree 编码。
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
			return nil, fmt.Errorf("decode tree: malformed name")
		}
		name := string(b[:nul])
		b = b[nul+1:]
		oidLen := 20 // SHA-1 raw 长度
		if len(b) < oidLen {
			return nil, fmt.Errorf("decode tree: truncated oid")
		}
		// SHA-256 仓库 raw oid 为 32 字节；按剩余数据推断无法可靠区分，
		// 因此 DecodeTree 由调用方保证传入对应算法的编码（见 ObjectLookup）。
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

// sortEntries 按 git 树序排序：目录视为名字后带 '/'。
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
	// 插入排序足够（骨架阶段，树条目数有限）；M1 换 sort.Slice。
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
