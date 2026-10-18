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

// packfile encode/decode wrapper: go-git is used purely as a codec library
// (docs/00 base selection). Boundary rule: go-git types never leak out of the storage package.
//
// v5.13 API mapping:
//   decode  packfile.NewScanner(r) + NewParserWithStorage(sc, storer) + Parse()
//   encode  packfile.NewEncoder(w, storer, false /*no delta*/) + Encode(wants, 0)

// memStorer collects decoded objects into an in-memory table (storer.EncodedObjectStorer adapter).
// order records the Parser's insertion order (= pack order), used for offset pairing.
type memStorer struct {
	mu      sync.Mutex
	objects map[plumbing.Hash]plumbing.EncodedObject
	order   []plumbing.Hash
}

func newMemStorer() *memStorer {
	return &memStorer{objects: map[plumbing.Hash]plumbing.EncodedObject{}}
}

// NewEncodedObject satisfies the storer interface: returns a MemoryObject.
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

// HasEncodedObject satisfies the storer interface (returns error, not bool).
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

// sliceIter is the simplest EncodedObjectIter implementation.
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

// DecodedPack is the result of decoding one push pack.
type DecodedPack struct {
	PackOID model.OID // shard primary key (pack trailer checksum)
	Blobs   map[model.OID][]byte
	Trees   map[model.OID][]byte // raw tree encoding
	Commits map[model.OID][]byte // raw commit encoding (incl. banyan-anchor headers)
	Order   []model.OID          // object order within the pack (Scanner order)
	// Offsets is oid → offset in the pack (the data source for idx v2).
	// Paired positionally with the Parser's object insertion order; empty when
	// counts mismatch (callers skip the idx write).
	Offsets map[model.OID]uint32
}

// DecodePack parses a received pack: the Parser expands OFS/REF deltas and
// collects objects in pack order; a second Scanner pass collects object-header offsets; paired positionally.
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
		default: // tags etc.: M1 extension
		}
		dp.Order = append(dp.Order, oid)
	}
	if !checksum.IsZero() {
		dp.PackOID = model.OID(checksum.String())
	} else {
		dp.PackOID = syntheticPackOID(dp)
	}

	// Second pass: Scanner object headers (offsets), paired positionally with Parser insertion order.
	if offs, serr := scanOffsets(raw); serr == nil && len(offs) == len(st.order) {
		dp.Offsets = make(map[model.OID]uint32, len(offs))
		for i, h := range st.order {
			dp.Offsets[model.OID(h.String())] = uint32(offs[i])
		}
	}
	return dp, nil
}

// scanOffsets collects the offset of every object header in the pack, in order.
// The Scanner does not track object boundaries: read the pack header first for
// the object count and loop by count — otherwise the end-of-pack trailer checksum is parsed as an object header and errors.
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

// EncodePack synthesizes a delta-free response pack from an object set
// (docs/03 §3 step 5: correctness first; deltification is repack's job).
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
	// Skeleton-stage shard key fallback: SHA-1 of the object list.
	var buf bytes.Buffer
	for _, oid := range dp.Order {
		buf.WriteString(string(oid))
		buf.WriteByte('\n')
	}
	return model.ObjectOID("pack", buf.Bytes(), model.SHA1)
}
