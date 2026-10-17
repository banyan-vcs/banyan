package storage

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/banyan-vcs/banyan/internal/model"
)

// packfile 编解码封装：go-git 作为编解码库使用（docs/00 基座选型）。
// 边界原则：go-git 类型不泄漏出 storage 包。
//
// v5.13 API 对接：
//   解码  packfile.NewScanner(r) + NewParserWithStorage(sc, storer) + Parse()
//   编码  packfile.NewEncoder(w, storer, false /*无 delta*/) + Encode(wants, 0)

// memStorer 把解码出的对象收进内存表（storer.EncodedObjectStorer 适配）。
// order 记录 Parser 的插入序（= pack 序），供 offset 配对使用。
type memStorer struct {
	mu      sync.Mutex
	objects map[plumbing.Hash]plumbing.EncodedObject
	order   []plumbing.Hash
}

func newMemStorer() *memStorer {
	return &memStorer{objects: map[plumbing.Hash]plumbing.EncodedObject{}}
}

// NewEncodedObject 满足 storer 接口：返回 MemoryObject。
func (m *memStorer) NewEncodedObject() plumbing.EncodedObject {
	return &plumbing.MemoryObject{}
}

func (m *memStorer) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	h := o.Hash()
	clone := &plumbing.MemoryObject{}
	clone.SetType(o.Type())
	clone.SetSize(o.Size())
	r, err := o.Reader()
	if err != nil {
		return h, err
	}
	defer r.Close()
	if _, err := io.Copy(clone, r); err != nil {
		return h, err
	}
	m.mu.Lock()
	if _, dup := m.objects[h]; !dup {
		m.order = append(m.order, h)
	}
	m.objects[h] = clone
	m.mu.Unlock()
	return h, nil
}

func (m *memStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[h]
	if !ok || (t != plumbing.AnyObject && o.Type() != t) {
		return nil, plumbing.ErrObjectNotFound
	}
	return o, nil
}

// HasEncodedObject 满足 storer 接口（返回 error 而非 bool）。
func (m *memStorer) HasEncodedObject(h plumbing.Hash) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[h]; !ok {
		return plumbing.ErrObjectNotFound
	}
	return nil
}

func (m *memStorer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[h]
	if !ok {
		return 0, plumbing.ErrObjectNotFound
	}
	return o.Size(), nil
}

func (m *memStorer) AddAlternate(string) error { return nil }

func (m *memStorer) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var objs []plumbing.EncodedObject
	for _, o := range m.objects {
		if t == plumbing.AnyObject || o.Type() == t {
			objs = append(objs, o)
		}
	}
	return &sliceIter{objs: objs}, nil
}

// sliceIter 是最简 EncodedObjectIter 实现。
type sliceIter struct {
	objs []plumbing.EncodedObject
	i    int
}

func (s *sliceIter) Next() (plumbing.EncodedObject, error) {
	if s.i >= len(s.objs) {
		return nil, io.EOF
	}
	o := s.objs[s.i]
	s.i++
	return o, nil
}

func (s *sliceIter) ForEach(f func(plumbing.EncodedObject) error) error {
	for _, o := range s.objs {
		if err := f(o); err != nil {
			return err
		}
	}
	return nil
}

func (s *sliceIter) Close() {}

// DecodedPack 是一次 push pack 解码的结果。
type DecodedPack struct {
	PackOID model.OID        // 分片主键（pack trailer checksum）
	Blobs   map[model.OID][]byte
	Trees   map[model.OID][]byte // 原始 tree 编码
	Commits map[model.OID][]byte // 原始 commit 编码（含 banyan-anchor 头）
	Order   []model.OID          // pack 内对象序（Scanner 序）
	// Offsets 是 oid → pack 内偏移（idx v2 的数据源）。与 Parser 的
	// 对象插入序按位配对得到；数量不齐时为空（调用方跳过 idx 写入）。
	Offsets map[model.OID]uint32
}

// DecodePack 解析收到的 pack：Parser 展开 OFS/REF delta 并按 pack 序
// 收集对象，第二遍 Scanner 收集对象头 offset，二者按位配对。
func DecodePack(raw []byte) (*DecodedPack, error) {
	st := newMemStorer()
	parser, err := packfile.NewParserWithStorage(packfile.NewScanner(bytes.NewReader(raw)), st)
	if err != nil {
		return nil, fmt.Errorf("decode pack: %w", err)
	}
	checksum, err := parser.Parse()
	if err != nil {
		return nil, fmt.Errorf("decode pack: %w", err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	dp := &DecodedPack{
		Blobs:   map[model.OID][]byte{},
		Trees:   map[model.OID][]byte{},
		Commits: map[model.OID][]byte{},
	}
	for _, h := range st.order {
		o := st.objects[h]
		oid := model.OID(h.String())
		rd, err := o.Reader()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(rd)
		rd.Close()
		if err != nil {
			return nil, err
		}
		switch o.Type() {
		case plumbing.BlobObject:
			dp.Blobs[oid] = content
		case plumbing.TreeObject:
			dp.Trees[oid] = content
		case plumbing.CommitObject:
			dp.Commits[oid] = content
		default: // tag 等：M1 扩展
		}
		dp.Order = append(dp.Order, oid)
	}
	if !checksum.IsZero() {
		dp.PackOID = model.OID(checksum.String())
	} else {
		dp.PackOID = syntheticPackOID(dp)
	}

	// 第二遍：Scanner 对象头（offset），与 Parser 插入序按位配对。
	if offs, serr := scanOffsets(raw); serr == nil && len(offs) == len(st.order) {
		dp.Offsets = make(map[model.OID]uint32, len(offs))
		for i, h := range st.order {
			dp.Offsets[model.OID(h.String())] = uint32(offs[i])
		}
	}
	return dp, nil
}

// scanOffsets 顺序收集 pack 内每个对象头的偏移。
// Scanner 不追踪对象边界，需先读 pack 头取对象数，按数循环——
// 否则末尾会把 trailer 校验和当对象头解析而报错。
func scanOffsets(raw []byte) ([]int64, error) {
	sc := packfile.NewScanner(bytes.NewReader(raw))
	_, count, err := sc.Header()
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, count)
	for i := uint32(0); i < count; i++ {
		h, err := sc.NextObjectHeader()
		if err != nil {
			return nil, err
		}
		out = append(out, h.Offset)
	}
	return out, nil
}

// EncodePack 把对象集合合成为无 delta 的响应 pack（docs/03 §3 步骤⑤：
// 正确性优先，delta 化是 repack 的职责）。
func EncodePack(w io.Writer, blobs map[model.OID][]byte, trees map[model.OID][]byte, commits map[model.OID][]byte) error {
	st := newMemStorer()
	want := make([]plumbing.Hash, 0, len(blobs)+len(trees)+len(commits))
	for oid, content := range blobs {
		h, err := putObject(st, plumbing.BlobObject, oid, content)
		if err != nil {
			return err
		}
		want = append(want, h)
	}
	for oid, content := range trees {
		h, err := putObject(st, plumbing.TreeObject, oid, content)
		if err != nil {
			return err
		}
		want = append(want, h)
	}
	for oid, content := range commits {
		h, err := putObject(st, plumbing.CommitObject, oid, content)
		if err != nil {
			return err
		}
		want = append(want, h)
	}
	enc := packfile.NewEncoder(w, st, false)
	if _, err := enc.Encode(want, 0); err != nil {
		return fmt.Errorf("encode pack: %w", err)
	}
	return nil
}

func putObject(st *memStorer, t plumbing.ObjectType, oid model.OID, content []byte) (plumbing.Hash, error) {
	o := &plumbing.MemoryObject{}
	o.SetType(t)
	o.SetSize(int64(len(content)))
	if _, err := o.Write(content); err != nil {
		return plumbing.ZeroHash, err
	}
	got := o.Hash()
	if model.OID(got.String()) != oid {
		return plumbing.ZeroHash, fmt.Errorf("object %s: hash mismatch (computed %s)", oid, got)
	}
	if _, err := st.SetEncodedObject(o); err != nil {
		return plumbing.ZeroHash, err
	}
	return got, nil
}

func syntheticPackOID(dp *DecodedPack) model.OID {
	// 骨架阶段的分片主键回退：对象清单的 SHA-1。
	var buf bytes.Buffer
	for _, oid := range dp.Order {
		buf.WriteString(string(oid))
		buf.WriteByte('\n')
	}
	return model.ObjectOID("pack", buf.Bytes(), model.SHA1)
}
