// Single-object extraction from a pack via idx offsets (docs/11 P1-1,
// B2): parse the object header at the located offset, inflate just that
// entry, and resolve OFS/REF delta chains recursively — instead of
// transferring and decoding the whole pack for one object. The source is
// random-access (in-memory bytes or a pack FILE read in windows), so a
// multi-hundred-MB pack costs O(object) memory per extraction. Format
// reference: pack-format.txt ("Pack file format" and "The delta data
// format").
package storage

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/banyan-vcs/banyan/internal/model"
)

// On-wire object type codes (pack-format.txt).
const (
	packObjCommit = 1
	packObjTree   = 2
	packObjBlob   = 3
	packObjTag    = 4
	packObjOfs    = 6
	packObjRef    = 7
)

var packObjType = map[byte]model.ObjectType{
	packObjCommit: model.CommitType,
	packObjTree:   model.TreeType,
	packObjBlob:   model.BlobType,
	packObjTag:    model.TagType,
}

// deltaChainLimit bounds delta-base recursion (corrupt packs, cycles).
const deltaChainLimit = 64

// packSource is random access into pack bytes: an in-memory buffer or a
// file (section reads on demand — the whole pack never has to be resident).
type packSource interface {
	io.ReaderAt
	Size() int64
}

type sizedSource struct {
	io.ReaderAt
	size int64
}

func (s *sizedSource) Size() int64 { return s.size }

// ExtractObject reads one object out of in-memory pack bytes using its idx.
func ExtractObject(raw []byte, idx *PackIndex, algo model.HashAlgo, oid model.OID) (model.ObjectType, []byte, error) {
	return extractObject(bytes.NewReader(raw), idx, algo, oid)
}

// ExtractObjectAt reads one object out of a random-access pack source (the
// node-side FILE path): header bytes and the object's zlib stream are read
// in windows at the located offsets, never the whole pack.
func ExtractObjectAt(src io.ReaderAt, size int64, idx *PackIndex, algo model.HashAlgo, oid model.OID) (model.ObjectType, []byte, error) {
	return extractObject(&sizedSource{ReaderAt: src, size: size}, idx, algo, oid)
}

// ExtractObject reads one object out of a pack using its idx. The object
// header at the idx offset carries the type and inflated size; ofs-delta
// entries encode a negative offset to their base, ref-delta entries the
// base oid (which must itself be in the pack — our stored packs are
// self-contained; receive-pack advertises no-thin).
func extractObject(src packSource, idx *PackIndex, algo model.HashAlgo, oid model.OID) (model.ObjectType, []byte, error) {
	off, ok := idx.Entries[oid]
	if !ok {
		return "", nil, fmt.Errorf("%w: %s not in pack idx", model.ErrObjectNotFound, oid)
	}
	return readObjectAt(src, idx, int64(off), algo, 0)
}

func readObjectAt(src packSource, idx *PackIndex, off int64, algo model.HashAlgo, depth int) (model.ObjectType, []byte, error) {
	// Pack layout: 12-byte header ... objects ... 20-byte trailer.
	if off < 12 || off >= src.Size()-20 {
		return "", nil, fmt.Errorf("extract: offset %d out of pack bounds", off)
	}
	if depth > deltaChainLimit {
		return "", nil, fmt.Errorf("extract: delta chain deeper than %d", deltaChainLimit)
	}
	// The header window: size varint (≤ 20B with 7-bit groups) plus, for
	// deltas, the ofs offset (≤ 20B) or base oid (20/32B) — 64 bytes covers
	// every legal encoding with slack.
	hdr := make([]byte, 64)
	n, err := src.ReadAt(hdr, off)
	if n == 0 {
		return "", nil, fmt.Errorf("extract: read header at %d: %w", off, err)
	}
	hdr = hdr[:n]

	// Object header: first byte = (type << 4) | size[3:0], continuation
	// bytes carry 7 more size bits each.
	pos := 0
	b := hdr[pos]
	pos++
	typ := (b >> 4) & 7
	size := int64(b & 0x0f)
	for shift := uint(4); b&0x80 != 0; shift += 7 {
		if pos >= len(hdr) {
			return "", nil, fmt.Errorf("extract: truncated size varint")
		}
		b = hdr[pos]
		pos++
		size |= int64(b&0x7f) << shift
	}

	switch typ {
	case packObjCommit, packObjTree, packObjBlob, packObjTag:
		data, err := inflateAt(src, off+int64(pos), size)
		if err != nil {
			return "", nil, fmt.Errorf("extract: inflate: %w", err)
		}
		return packObjType[typ], data, nil

	case packObjOfs:
		// Negative offset to the base's header, git's "offset encoding":
		// 7 bits per byte, +1 on every continuation.
		if pos >= len(hdr) {
			return "", nil, fmt.Errorf("extract: truncated ofs-delta offset")
		}
		b = hdr[pos]
		pos++
		rel := int64(b & 0x7f)
		for b&0x80 != 0 {
			if pos >= len(hdr) {
				return "", nil, fmt.Errorf("extract: truncated ofs-delta offset")
			}
			b = hdr[pos]
			pos++
			rel = ((rel + 1) << 7) | int64(b&0x7f)
		}
		if rel > off {
			return "", nil, fmt.Errorf("extract: ofs-delta base before pack start")
		}
		baseType, base, err := readObjectAt(src, idx, off-rel, algo, depth+1)
		if err != nil {
			return "", nil, err
		}
		delta, err := inflateAt(src, off+int64(pos), size)
		if err != nil {
			return "", nil, fmt.Errorf("extract: inflate delta: %w", err)
		}
		out, err := applyDelta(base, delta)
		if err != nil {
			return "", nil, err
		}
		return baseType, out, nil

	case packObjRef:
		width := RawOIDWidth(algo)
		if pos+width > len(hdr) {
			return "", nil, fmt.Errorf("extract: truncated ref-delta base")
		}
		baseOID := model.OID(hex.EncodeToString(hdr[pos : pos+width]))
		pos += width
		baseOff, ok := idx.Entries[baseOID]
		if !ok {
			return "", nil, fmt.Errorf("extract: ref-delta base %s not in pack", baseOID)
		}
		baseType, base, err := readObjectAt(src, idx, int64(baseOff), algo, depth+1)
		if err != nil {
			return "", nil, err
		}
		delta, err := inflateAt(src, off+int64(pos), size)
		if err != nil {
			return "", nil, fmt.Errorf("extract: inflate delta: %w", err)
		}
		out, err := applyDelta(base, delta)
		if err != nil {
			return "", nil, err
		}
		return baseType, out, nil

	default:
		return "", nil, fmt.Errorf("extract: reserved object type %d", typ)
	}
}

// inflateAt decompresses one zlib stream starting at pos; the header's
// declared size must materialize exactly (a short stream means corruption).
// The stream is read in windows from the source — only the object's own
// bytes transit memory.
func inflateAt(src packSource, pos int64, size int64) ([]byte, error) {
	if size < 0 || size > src.Size() {
		return nil, fmt.Errorf("declared size %d exceeds pack", size)
	}
	zr, err := zlib.NewReader(io.NewSectionReader(src, pos, src.Size()-pos))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out := make([]byte, size)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, err
	}
	return out, nil
}

// applyDelta applies a git delta buffer to its base (pack-format.txt "The
// delta data format"): two size varints, then a program of copy-from-base
// (opcode MSB set; present bits select offset/size bytes) and
// insert-literal opcodes.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	need := func(n int) error {
		if pos+n > len(delta) {
			return fmt.Errorf("delta: truncated at %d", pos)
		}
		return nil
	}
	readVarint := func() (int64, error) {
		var v int64
		var shift uint
		for {
			if err := need(1); err != nil {
				return 0, err
			}
			b := delta[pos]
			pos++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta: base size %d != actual %d", srcSize, len(base))
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 { // copy from base: offset bytes bits 0-3, size bytes bits 4-6
			var cpOff, cpSize int64
			if op&0x01 != 0 {
				if err := need(1); err != nil {
					return nil, err
				}
				cpOff = int64(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				if err := need(1); err != nil {
					return nil, err
				}
				cpOff |= int64(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				if err := need(1); err != nil {
					return nil, err
				}
				cpOff |= int64(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				if err := need(1); err != nil {
					return nil, err
				}
				cpOff |= int64(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				if err := need(1); err != nil {
					return nil, err
				}
				cpSize = int64(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				if err := need(1); err != nil {
					return nil, err
				}
				cpSize |= int64(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				if err := need(1); err != nil {
					return nil, err
				}
				cpSize |= int64(delta[pos]) << 16
				pos++
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff < 0 || cpSize < 0 || cpOff+cpSize > int64(len(base)) {
				return nil, fmt.Errorf("delta: copy [%d,%d) out of base (%d)", cpOff, cpOff+cpSize, len(base))
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 { // insert literal
			if err := need(int(op)); err != nil {
				return nil, err
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta: reserved opcode 0")
		}
	}
	if int64(len(out)) != dstSize {
		return nil, fmt.Errorf("delta: result size %d != declared %d", len(out), dstSize)
	}
	return out, nil
}
