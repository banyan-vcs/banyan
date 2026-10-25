package storage

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/banyan-vcs/banyan/internal/model"
)

// git idx v2 encode/decode — format reference: git source
// Documentation/technical/pack-format.txt ("pack index format" section).
//
//	magic: \377tOc   version: 2
//	fanout[256]      (big-endian uint32 cumulative counts)
//	oids[]           (sorted, fixed raw width)
//	crc32[]          (4 bytes per object)
//	offsets[]        (uint32; MSB=1 marks index into the 8-byte large-offset table)
//	[large offsets[]]
//	pack checksum + idx checksum
//
// Offsets are 64-bit end to end: packs beyond 4GiB are first-class (the
// large-offset table is how idx v2 expresses them; git has no single-pack
// size limit), so the in-memory maps and both codecs carry uint64.

const idxMagic = "\xfftOc"

// PackIndex is the parsed idx content: OID → offset within the pack.
type PackIndex struct {
	PackOID  model.OID // pack trailer checksum (shard primary key)
	Entries  map[model.OID]uint64
	CRCs     map[model.OID]uint32
	OIDOrder []model.OID // sorted order (for iteration)
}

// RawOIDWidth returns the raw oid byte width for the hash algorithm.
func RawOIDWidth(algo model.HashAlgo) int {
	if algo == model.SHA256 {
		return 32
	}
	return 20
}

// EncodeIdx generates an idx v2 byte stream. CRCs are the per-object crc32
// values collected at decode time (the Parser's observer feed — the same
// source git's index-pack uses); a nil map writes zeros.
func EncodeIdx(algo model.HashAlgo, packOID model.OID, offsets map[model.OID]uint64, crcs map[model.OID]uint32) ([]byte, error) {
	width := RawOIDWidth(algo)
	oids := make([]string, 0, len(offsets))
	for o := range offsets {
		oids = append(oids, string(o))
	}
	sort.Strings(oids)

	out := make([]byte, 0, 4+4+256*4+len(oids)*(width+8))
	out = append(out, idxMagic...)
	out = binary.BigEndian.AppendUint32(out, 2)

	// fanout: cumulative counts by first byte.
	var fanout [256]uint32
	for _, o := range oids {
		fanout[hexFirstByte(o)]++
	}
	var acc uint32
	for i := 0; i < 256; i++ {
		acc += fanout[i]
		out = binary.BigEndian.AppendUint32(out, acc)
	}
	for _, o := range oids {
		raw, err := hex.DecodeString(o)
		if err != nil || len(raw) != width {
			return nil, fmt.Errorf("encode idx: bad oid %q", o)
		}
		out = append(out, raw...)
	}
	for _, o := range oids {
		if crcs == nil {
			out = binary.BigEndian.AppendUint32(out, 0)
		} else {
			out = binary.BigEndian.AppendUint32(out, crcs[model.OID(o)])
		}
	}
	var large []byte
	for _, o := range oids {
		off := offsets[model.OID(o)]
		if off < 0x80000000 {
			out = binary.BigEndian.AppendUint32(out, uint32(off))
		} else {
			out = binary.BigEndian.AppendUint32(out, 0x80000000|uint32(len(large)/8))
			large = binary.BigEndian.AppendUint64(large, off)
		}
	}
	out = append(out, large...)
	packRaw, err := hex.DecodeString(string(packOID))
	if err != nil || len(packRaw) != width {
		return nil, fmt.Errorf("encode idx: bad pack oid")
	}
	out = append(out, packRaw...)
	out = append(out, make([]byte, width)...) // idx 自身 checksum，TODO(M1)
	return out, nil
}

// DecodeIdx parses an idx v2, resolving the large-offset table (MSB=1 in
// the 4-byte offsets column indexes the trailing 8-byte entries).
func DecodeIdx(b []byte, algo model.HashAlgo) (*PackIndex, error) {
	width := RawOIDWidth(algo)
	if len(b) < 8+256*4 || string(b[:4]) != idxMagic {
		return nil, fmt.Errorf("decode idx: bad magic")
	}
	if v := binary.BigEndian.Uint32(b[4:8]); v != 2 {
		return nil, fmt.Errorf("decode idx: unsupported version %d", v)
	}
	n := int(binary.BigEndian.Uint32(b[8+255*4:]))
	need := 8 + 256*4 + n*(width+8)
	if len(b) < need {
		return nil, fmt.Errorf("decode idx: truncated")
	}
	pi := &PackIndex{Entries: map[model.OID]uint64{}, CRCs: map[model.OID]uint32{}}
	pos := 8 + 256*4
	for i := 0; i < n; i++ {
		oid := model.OID(hex.EncodeToString(b[pos : pos+width]))
		pos += width
		pi.OIDOrder = append(pi.OIDOrder, oid)
	}
	for i := 0; i < n; i++ {
		pi.CRCs[pi.OIDOrder[i]] = binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
	}
	largeBase := int64(pos + n*4) // the 4-byte column ends; the 8-byte table follows
	for i := 0; i < n; i++ {
		col := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
		if col&0x80000000 == 0 {
			pi.Entries[pi.OIDOrder[i]] = uint64(col)
			continue
		}
		at := largeBase + int64(col&0x7fffffff)*8
		if at+8 > int64(len(b)) {
			return nil, fmt.Errorf("decode idx: large offset out of range")
		}
		pi.Entries[pi.OIDOrder[i]] = binary.BigEndian.Uint64(b[at : at+8])
	}
	// Trailing pack checksum parsing is deferred past M1 (callers currently
	// supply PackOID via the shard name).
	return pi, nil
}

func hexFirstByte(hexStr string) byte {
	v := hexStr[:2]
	b, _ := hex.DecodeString(v)
	if len(b) == 0 {
		return 0
	}
	return b[0]
}
