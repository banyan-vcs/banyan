package storage

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/banyan-vcs/banyan/internal/model"
)

// SpoolVerifiedCopy streams r into dst while hashing everything except the
// trailing hash-width bytes (the pack trailer): a trailing window stays out
// of the hasher, so at EOF the hasher holds the checksum of the body and the
// window holds the trailer — the pack integrity check without ever holding
// the pack. Returns the total bytes written, the hex body checksum and
// whether the trailer matched it. Memory is O(window + read buffer).
func SpoolVerifiedCopy(dst io.Writer, r io.Reader, algo model.HashAlgo) (n int64, sum string, trailerOK bool, err error) {
	width := 20
	var h hash.Hash = sha1.New()
	if algo == model.SHA256 {
		width = 32
		h = sha256.New()
	}
	pending := make([]byte, 0, width+64*1024)
	buf := make([]byte, 64*1024)
	var total int64
	for {
		rn, rerr := r.Read(buf)
		if rn > 0 {
			wn, werr := dst.Write(buf[:rn])
			total += int64(wn)
			if werr != nil {
				return total, "", false, werr
			}
			if wn != rn {
				return total, "", false, io.ErrShortWrite
			}
			pending = append(pending, buf[:rn]...)
			if len(pending) > width {
				h.Write(pending[:len(pending)-width])
				copy(pending, pending[len(pending)-width:])
				pending = pending[:width]
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return total, "", false, rerr
		}
	}
	if total < int64(width)+12 { // pack header (12) + trailer
		return total, "", false, fmt.Errorf("pack too short (%d bytes)", total)
	}
	trailer := h.Sum(nil)
	ok := len(pending) == width
	for i := 0; ok && i < width; i++ {
		ok = pending[i] == trailer[i]
	}
	return total, hex.EncodeToString(trailer), ok, nil
}

// algoOfPackOID derives the hash algo from the pack OID itself (40 hex = sha1,
// 64 hex = sha256): the landing path has no algo channel of its own, and the
// OID width is unambiguous.
func algoOfPackOID(oid model.OID) model.HashAlgo {
	if len(oid) == 64 {
		return model.SHA256
	}
	return model.SHA1
}
