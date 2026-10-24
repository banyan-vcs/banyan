package storage

import ()

// Git delta ENCODER (pack-format.txt "The delta data format"; the decoder
// is applyDelta in packobj.go). Mirrors diff-delta.c's structure: fixed
// 16-byte blocks of the base are hashed; the target walks forward, tries
// block candidates, extends matches (forward to the block-match limit and
// backward into the pending literal), and emits copy/insert opcodes.
//
//	<header: srcSize varint, dstSize varint>
//	*opcode: 0x80|bits → copy (bits 0-3 select offset bytes, 4-6 size bytes;
//	         size 0 means 0x10000); 1-127 → literal insert of that many bytes

const (
	deltaBlockSize = 16
	// deltaMinMatch: a copy shorter than a block is not worth its opcode.
	deltaMinMatch = 16
	// deltaMaxCopy caps one copy op (3 size bytes address 24 bits).
	deltaMaxCopy = 1 << 21
	// deltaMaxCandidates bounds hash-collision probing per position.
	deltaMaxCandidates = 16
)

func deltaHash(b []byte) uint64 {
	// FNV-1a over the block.
	var h uint64 = 14695981039346656037
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

func matchLen(a, b []byte, max int) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n > max {
		n = max
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// deltaIndex is the base's block index: hash → block start offsets.
type deltaIndex map[uint64][]int32

func buildDeltaIndex(base []byte) deltaIndex {
	idx := deltaIndex{}
	for i := 0; i+deltaBlockSize <= len(base); i += deltaBlockSize {
		h := deltaHash(base[i : i+deltaBlockSize])
		idx[h] = append(idx[h], int32(i))
	}
	return idx
}

// DiffDelta computes a git delta transforming base into target. It returns
// nil when the inputs cannot benefit (empty base, or the encoded delta is
// not smaller than the target) — the caller then stores the object whole.
func DiffDelta(base, target []byte) []byte {
	if len(base) == 0 || len(target) == 0 {
		return nil
	}
	idx := buildDeltaIndex(base)

	out := make([]byte, 0, len(target)/2+16)
	out = appendDeltaVarint(out, uint64(len(base)))
	out = appendDeltaVarint(out, uint64(len(target)))

	litStart := 0
	pos := 0
	flushLit := func(end int) {
		for litStart < end {
			n := end - litStart
			if n > 127 {
				n = 127
			}
			out = append(out, byte(n))
			out = append(out, target[litStart:litStart+n]...)
			litStart += n
		}
	}
	for pos+deltaBlockSize <= len(target) {
		h := deltaHash(target[pos : pos+deltaBlockSize])
		cands := idx[h]
		if len(cands) > 0 {
			bestLen, bestOff := 0, 0
			for i := len(cands) - 1; i >= 0 && i >= len(cands)-deltaMaxCandidates; i-- {
				cand := int(cands[i])
				if l := matchLen(base[cand:], target[pos:], deltaMaxCopy); l > bestLen {
					bestLen, bestOff = l, cand
					if bestLen >= 4096 {
						break // good enough; stop probing
					}
				}
			}
			if bestLen >= deltaMinMatch {
				// Backward extension into the pending literal.
				back := 0
				for back < pos-litStart && bestOff-back > 0 &&
					base[bestOff-back-1] == target[pos-back-1] {
					back++
				}
				flushLit(pos - back)
				copyOff := bestOff - back
				copyLen := back + bestLen
				out = appendDeltaCopy(out, copyOff, copyLen)
				pos = pos - back + copyLen
				litStart = pos
				continue
			}
		}
		pos++
	}
	flushLit(len(target))

	// Not worth it if the delta is (nearly) the target's size.
	if len(out) >= len(target) {
		return nil
	}
	return out
}

func appendDeltaVarint(out []byte, v uint64) []byte {
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// appendDeltaCopy emits one copy opcode: flag bits select WHICH byte
// positions of the offset (bits 0-3) and size (bits 4-6) are present; each
// present byte is shifted by its position on decode (see applyDelta).
// Zero-valued positions are skipped; at least one offset byte is always
// present.
func appendDeltaCopy(out []byte, off, size int) []byte {
	var op byte = 0x80
	var payload []byte
	for i := 0; i < 4; i++ {
		b := byte((off >> (8 * i)) & 0xff)
		if b != 0 || i == 0 {
			op |= 1 << i
			payload = append(payload, b)
		}
	}
	for i := 0; i < 3; i++ {
		b := byte((size >> (8 * i)) & 0xff)
		if b != 0 {
			op |= 0x10 << i
			payload = append(payload, b)
		}
	}
	out = append(out, op)
	return append(out, payload...)
}
