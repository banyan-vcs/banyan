package storage

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sync"

	"crypto/sha1"
	"compress/zlib"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Thin pack writer (docs/12 P2-6, B8): a have-subtracted fetch packs its
// objects as REF_DELTA against bases the CLIENT provably holds but that are
// NOT in the pack — git's thin pack; the client's index-pack --fix-thin
// resolves the bases locally. go-git's encoder cannot reference absent
// bases, so this is a self-contained writer (format per pack-format.txt:
// header, entries, trailing SHA-1).
//
// Entry order: commits, trees, blobs, tags (git-ish; REF_DELTA entries may
// appear anywhere since their bases are external). Objects without a base —
// or whose delta is not meaningfully smaller — are written whole.

// packWriter streams a pack with its trailer hash.
type packWriter struct {
	w   io.Writer
	h   hash.Hash
	mw  io.Writer
	zw  *zlib.Writer // reused per entry (Reset)
	err error
}

func newPackWriter(w io.Writer) *packWriter {
	h := sha1.New()
	return &packWriter{w: w, h: h, mw: io.MultiWriter(w, h), zw: zlib.NewWriter(io.Discard)}
}

func (p *packWriter) write(b []byte) {
	if p.err != nil {
		return
	}
	_, p.err = p.mw.Write(b)
}

// object writes one entry: header varint, optional raw base oid, zlib body.
func (p *packWriter) object(typ byte, size int, extra []byte, body []byte) {
	if p.err != nil {
		return
	}
	// Object header: first byte = (type << 4) | size[3:0]; continuation
	// bytes carry 7 more size bits each.
	b := byte(typ)<<4 | byte(size&0x0f)
	s := size >> 4
	for s != 0 {
		p.write([]byte{b | 0x80})
		b = byte(s & 0x7f)
		s >>= 7
	}
	p.write([]byte{b})
	if len(extra) > 0 {
		p.write(extra)
	}
	p.zw.Reset(p.mw)
	if _, err := p.zw.Write(body); err != nil {
		p.err = err
		return
	}
	if err := p.zw.Close(); err != nil {
		p.err = err
	}
}

func (p *packWriter) finish() error {
	if p.err != nil {
		return p.err
	}
	_, err := p.w.Write(p.h.Sum(nil))
	return err
}

// On-wire object type codes (packobj.go owns the parse-side constants).
const (
	writeTypeCommit byte = 1
	writeTypeTree   byte = 2
	writeTypeBlob   byte = 3
	writeTypeTag    byte = 4
	writeTypeRef    byte = 7
)

// thinPackEntry pairs a served object with its contents and wire type.
type thinPackEntry struct {
	oid  model.OID
	typ  byte
	data []byte
}

// EncodePackThin writes a pack that may REF_DELTA against bases held by the
// CLIENT (not in the pack). baseFor maps served oid → base oid; loadBase
// fetches a base's type and content (server-side read). An entry is
// deltified only when its delta is at most 3/4 of the whole object.
func EncodePackThin(w io.Writer, algo model.HashAlgo,
	blobs, trees, commits, tags map[model.OID][]byte,
	baseFor map[model.OID]model.OID, loadBase func(model.OID) (model.ObjectType, []byte, error)) error {

	entries := make([]thinPackEntry, 0, len(commits)+len(trees)+len(blobs)+len(tags))
	for oid, data := range commits {
		entries = append(entries, thinPackEntry{oid, writeTypeCommit, data})
	}
	for oid, data := range trees {
		entries = append(entries, thinPackEntry{oid, writeTypeTree, data})
	}
	for oid, data := range blobs {
		entries = append(entries, thinPackEntry{oid, writeTypeBlob, data})
	}
	for oid, data := range tags {
		entries = append(entries, thinPackEntry{oid, writeTypeTag, data})
	}

	pw := newPackWriter(w)
	pw.write([]byte("PACK"))
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)                    // version
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(entries))) // object count
	pw.write(hdr[:])

	width := RawOIDWidth(algo)
	var baseMu sync.Mutex
	baseCache := map[model.OID][]byte{}
	for _, e := range entries {
		baseOID, hasBase := baseFor[e.oid]
		if hasBase {
			baseMu.Lock()
			baseData, cached := baseCache[baseOID]
			baseMu.Unlock()
			if !cached {
				bt, bdata, err := loadBase(baseOID)
				if err == nil {
					if (e.typ == writeTypeCommit && bt != model.CommitType) ||
						(e.typ == writeTypeTree && bt != model.TreeType) ||
						(e.typ == writeTypeBlob && bt != model.BlobType) ||
						(e.typ == writeTypeTag && bt != model.TagType) {
						bdata = nil // type mismatch: no delta
					}
					baseMu.Lock()
					baseCache[baseOID] = bdata
					baseMu.Unlock()
					baseData = bdata
				}
			}
			if len(baseData) > 0 {
				if delta := DiffDelta(baseData, e.data); delta != nil && len(delta) < len(e.data)*3/4 {
					raw, err := hex.DecodeString(string(baseOID))
					if err == nil && len(raw) == width {
						pw.object(writeTypeRef, len(delta), raw, delta) // header size = stored delta bytes, not target size
						continue
					}
				}
			}
		}
		pw.object(e.typ, len(e.data), nil, e.data)
	}
	if err := pw.finish(); err != nil {
		return fmt.Errorf("encode thin pack: %w", err)
	}
	return nil
}
