// Single-object extraction from pack bytes via idx offsets (docs/12 P1-1,
// B2): parse the object header at the located offset, inflate just that
// entry, and resolve OFS/REF delta chains recursively — instead of
// transferring and decoding the whole pack for one object. Format reference:
// pack-format.txt ("Pack file format" and "The delta data format").
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

// ExtractObject reads one object out of a pack using its idx. The object
// header at the idx offset carries the type and inflated size; ofs-delta
// entries encode a negative offset to their base, ref-delta entries the
// base oid (which must itself be in the pack — our stored packs are
// self-contained; receive-pack advertises no-thin).
func ExtractObject(raw []byte, idx *PackIndex, algo model.HashAlgo, oid model.OID) (model.ObjectType, []byte, error) {
	off, ok := idx.Entries[oid]
	if !ok {
		return "", nil, fmt.Errorf("%w: %s not in pack idx", model.ErrObjectNotFound, oid)
	}
	return readObjectAt(raw, idx, int64(off), algo, 0)
}

func readObjectAt(raw []byte, idx *PackIndex, off int64, algo model.HashAlgo, depth int) (model.ObjectType, []byte, error) {
	// Pack layout: 12-byte header ... objects ... 20-byte trailer.
	if off < 12 || off >= int64(len(raw))-20 {
		return "", nil, fmt.Errorf("extract: offset %d out of pack bounds", off)
	}
	if depth > deltaChainLimit {
		return "", nil, fmt.Errorf("extract: delta chain deeper than %d", deltaChainLimit)
	}
	pos := int(off)

	// Object header: first byte = (type << 4) | size[3:0], continuation
	// bytes carry 7 more size bits each.
	b := raw[pos]
	pos++
	typ := (b >> 4) & 7
	size := int64(b & 0x0f)
	for shift := uint(4); b&0x80 != 0; shift += 7 {
		if pos >= len(raw) {
			return "", nil, fmt.Errorf("extract: truncated size varint")
		}
		b = raw[pos]
		pos++
		size |= int64(b&0x7f) << shift
	}

	switch typ {
	case packObjCommit, packObjTree, packObjBlob, packObjTag:
		data, err := inflateAt(raw, pos, size)
		if err != nil {
			return "", nil, fmt.Errorf("extract: inflate: %w", err)
		}
		return packObjType[typ], data, nil

	case packObjOfs:
		// Negative offset to the base's header, git's "offset encoding":
		// 7 bits per byte, +1 on every continuation.
		if pos >= len(raw) {
			return "", nil, fmt.Errorf("extract: truncated ofs-delta offset")
		}
		b = raw[pos]
		pos++
		rel := int64(b & 0x7f)
		for b&0x80 != 0 {
			if pos >= len(raw) {
				return "", nil, fmt.Errorf("extract: truncated ofs-delta offset")
			}
			b = raw[pos]
			pos++
			rel = ((rel + 1) << 7) | int64(b&0x7f)
		}
		if rel > off {
			return "", nil, fmt.Errorf("extract: ofs-delta base before pack start")
		}
		baseType, base, err := readObjectAt(raw, idx, off-rel, algo, depth+1)
		if err != nil {
			return "", nil, err
		}
		delta, err := inflateAt(raw, pos, size)
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
		if pos+width > len(raw) {
			return "", nil, fmt.Errorf("extract: truncated ref-delta base")
		}
		baseOID := model.OID(hex.EncodeToString(raw[pos : pos+width]))
		pos += width
		baseOff, ok := idx.Entries[baseOID]
		if !ok {
			return "", nil, fmt.Errorf("extract: ref-delta base %s not in pack", baseOID)
		}
		baseType, base, err := readObjectAt(raw, idx, int64(baseOff), algo, depth+1)
		if err != nil {
			return "", nil, err
		}
		delta, err := inflateAt(raw, pos, size)
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
func inflateAt(raw []byte, pos int, size int64) ([]byte, error) {
	if size < 0 || size > int64(len(raw)) {
		return nil, fmt.Errorf("declared size %d exceeds pack", size)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw[pos:]))
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
