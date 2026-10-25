package storage

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
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
	Tags    map[model.OID][]byte // raw annotated-tag encoding
	Order   []model.OID          // object order within the pack (Parser resolution order)
	// Offsets is oid → offset in the pack (the data source for idx v2),
	// CRCs the per-object crc32 — both collected from the Parser's observer
	// callbacks, which carry the offset explicitly (the Parser's resolution
	// order differs from pack order for delta entries, so the old
	// positional pairing mis-mapped delta packs; found by P1-1 extraction).
	// Offsets are 64-bit: packs beyond 4GiB are first-class (idx v2's
	// large-offset table).
	Offsets map[model.OID]uint64
	CRCs    map[model.OID]uint32
}

// idxObserver collects the authoritative oid → offset/crc mapping from the
// Parser's observer callbacks (the same feed go-git's idx encoder uses).
// order (with a seen-set guard against placeholder double callbacks) feeds
// the streaming ingest path, where there is no storer to read it back from.
type idxObserver struct {
	offsets map[model.OID]uint64
	crcs    map[model.OID]uint32
	order   []model.OID
	seen    map[model.OID]bool
}

func (o *idxObserver) OnHeader(uint32) error { return nil }
func (o *idxObserver) OnInflatedObjectHeader(plumbing.ObjectType, int64, int64) error {
	return nil
}
func (o *idxObserver) OnInflatedObjectContent(h plumbing.Hash, pos int64, crc uint32, _ []byte) error {
	if o.offsets == nil {
		o.offsets = map[model.OID]uint64{}
		o.crcs = map[model.OID]uint32{}
		o.seen = map[model.OID]bool{}
	}
	oid := model.OID(h.String())
	o.offsets[oid] = uint64(pos)
	o.crcs[oid] = crc
	if !o.seen[oid] {
		o.seen[oid] = true
		o.order = append(o.order, oid)
	}
	return nil
}
func (o *idxObserver) OnFooter(plumbing.Hash) error { return nil }

// DecodePack parses a received pack: the Parser expands OFS/REF deltas and
// collects objects in resolution order; an observer collects the per-object
// idx offsets and crcs.
func DecodePack(raw []byte) (*DecodedPack, error) {
	st := newMemStorer()
	obs := &idxObserver{}
	parser, err := packfile.NewParserWithStorage(packfile.NewScanner(bytes.NewReader(raw)), st, obs)
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
		Tags:    map[model.OID][]byte{},
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
		case plumbing.TagObject:
			dp.Tags[oid] = content
		default: // future object types
		}
		dp.Order = append(dp.Order, oid)
	}
	if !checksum.IsZero() {
		dp.PackOID = model.OID(checksum.String())
	} else {
		dp.PackOID = syntheticPackOID(dp)
	}
	if len(obs.offsets) == len(st.order) {
		dp.Offsets = obs.offsets
		dp.CRCs = obs.crcs
	}
	return dp, nil
}

// EncodePack synthesizes a delta-free response pack from an object set
// (docs/04 §3 step 5: small fetches stay delta-free — below the serve
// threshold the encoder cost buys nothing).
func EncodePack(w io.Writer, blobs, trees, commits, tags map[model.OID][]byte) error {
	return encodePackOpts(w, blobs, trees, commits, tags, 0, false)
}

// packWindow is git's default pack window: how many previously sorted
// objects each candidate is compared against for delta compression.
const packWindow = 10

// EncodePackDelta synthesizes a delta-encoded pack with OFS_DELTA entries
// (git pack-objects window heuristics are the encoder library's; docs/09
// §3.2: repack merges fragments with deltification, the serve path deltas
// past a size threshold — docs/11 P1-4).
//
// Encoder subtlety (found while implementing P1-4): NewEncoder's third
// parameter is useRefDeltas, NOT a delta switch — delta compression itself
// is governed by the Encode window, and a window of 0 disables it entirely.
// The previous call passed the delta flag as useRefDeltas with window 0, so
// it silently produced non-delta packs while claiming otherwise.
func EncodePackDelta(w io.Writer, blobs, trees, commits, tags map[model.OID][]byte) error {
	return encodePackOpts(w, blobs, trees, commits, tags, packWindow, false)
}

// EncodePackDeltaRef is EncodePackDelta with REF_DELTA entries instead of
// OFS_DELTA, for v2 fetch clients that did not send the ofs-delta arg.
func EncodePackDeltaRef(w io.Writer, blobs, trees, commits, tags map[model.OID][]byte) error {
	return encodePackOpts(w, blobs, trees, commits, tags, packWindow, true)
}

func encodePackOpts(w io.Writer, blobs map[model.OID][]byte, trees map[model.OID][]byte, commits map[model.OID][]byte, tags map[model.OID][]byte, window uint, useRefDeltas bool) error {
	st := newMemStorer()
	want := make([]plumbing.Hash, 0, len(blobs)+len(trees)+len(commits)+len(tags))
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
	for oid, content := range tags {
		h, err := putObject(st, plumbing.TagObject, oid, content)
		if err != nil {
			return err
		}
		want = append(want, h)
	}
	enc := packfile.NewEncoder(w, st, useRefDeltas)
	if _, err := enc.Encode(want, window); err != nil {
		return fmt.Errorf("encode pack: %w", err)
	}
	return nil
}

// ScanObjectTypes lists the on-wire entry types of a pack in pack order
// (serve-path observability and tests: the presence of ofs-delta entries
// proves the online encoder deltified).
func ScanObjectTypes(raw []byte) ([]model.ObjectType, error) {
	sc := packfile.NewScanner(bytes.NewReader(raw))
	_, count, err := sc.Header()
	if err != nil {
		return nil, err
	}
	out := make([]model.ObjectType, 0, count)
	for i := uint32(0); i < count; i++ {
		h, err := sc.NextObjectHeader()
		if err != nil {
			return nil, err
		}
		out = append(out, model.ObjectType(h.Type.String()))
		if _, _, err := sc.NextObject(io.Discard); err != nil {
			return nil, err
		}
	}
	return out, nil
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

// VerifyPackChecksum checks the SHA-1 pack trailer: the last 20 bytes must
// equal SHA-1 over everything before them, and match the pack OID. Identity
// equals integrity (docs/10 §3) — a cheap full-content check with no decode.
func VerifyPackChecksum(packOID model.OID, raw []byte) bool {
	if len(raw) < 20 {
		return false
	}
	body, trailer := raw[:len(raw)-20], raw[len(raw)-20:]
	sum := sha1.Sum(body)
	return string(sum[:]) == string(trailer) && hex.EncodeToString(trailer) == string(packOID)
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
