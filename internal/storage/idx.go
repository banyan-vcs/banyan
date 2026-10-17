package storage

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/banyan-vcs/banyan/internal/model"
)

// git idx v2 编解码——格式基准：git 源码
// Documentation/technical/pack-format.txt（"pack index format"节）。
//
//	 magic: \377tOc   version: 2
//	 fanout[256]      (big-endian uint32 累积计数)
//	 oids[]           (排序，raw 定宽)
//	 crc32[]          (每对象 4 字节)
//	 offsets[]        (uint32；MSB=1 时为 8 字节大偏移表索引)
//	 [large offsets[]]
//	 pack checksum + idx checksum

const idxMagic = "\xfftOc"

// PackIndex 是解析后的 idx 内容：OID → pack 内偏移。
type PackIndex struct {
	PackOID  model.OID // pack trailer checksum（分片主键）
	Entries  map[model.OID]uint32
	OIDOrder []model.OID // 排序序（遍历用）
}

// RawOIDWidth 按 hash 算法返回 raw oid 字节数。
func RawOIDWidth(algo model.HashAlgo) int {
	if algo == model.SHA256 {
		return 32
	}
	return 20
}

// EncodeIdx 生成 idx v2 字节流（骨架不含 crc 精确校验，填 0；
// M1 接入真实 crc32 —— 需与 pack 解码联动计算）。
func EncodeIdx(algo model.HashAlgo, packOID model.OID, offsets map[model.OID]uint32) ([]byte, error) {
	width := RawOIDWidth(algo)
	oids := make([]string, 0, len(offsets))
	for o := range offsets {
		oids = append(oids, string(o))
	}
	sort.Strings(oids)

	out := make([]byte, 0, 4+4+256*4+len(oids)*(width+8))
	out = append(out, idxMagic...)
	out = binary.BigEndian.AppendUint32(out, 2)

	// fanout：按首字节累积。
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
	for range oids {
		out = binary.BigEndian.AppendUint32(out, 0) // TODO(M1): 真实 crc32
	}
	var large []byte
	for _, o := range oids {
		off := offsets[model.OID(o)]
		if off < 0x80000000 {
			out = binary.BigEndian.AppendUint32(out, off)
		} else {
			out = binary.BigEndian.AppendUint32(out, 0x80000000|uint32(len(large)/8))
			large = binary.BigEndian.AppendUint64(large, uint64(off))
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

// DecodeIdx 解析 idx v2。
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
	pi := &PackIndex{Entries: map[model.OID]uint32{}}
	pos := 8 + 256*4
	for i := 0; i < n; i++ {
		oid := model.OID(hex.EncodeToString(b[pos : pos+width]))
		pos += width
		pi.OIDOrder = append(pi.OIDOrder, oid)
	}
	pos += n * 4 // 跳过 crc32 段
	for i := 0; i < n; i++ {
		pi.Entries[pi.OIDOrder[i]] = binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
	}
	// 尾部 pack checksum 的解析 M1 补全（当前由调用方以分片名提供 PackOID）。
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
