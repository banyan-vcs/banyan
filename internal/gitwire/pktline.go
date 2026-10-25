// Package gitwire implements the git compatibility layer (docs/04-git-compat.md):
// Smart HTTP v1 endpoints, upload-pack/receive-pack sessions, virtual (subtree) repo routing,
// server-side filter. Protocol behavior baseline: git sources upload-pack.c / receive-pack.c /
// http-backend.c and the protocol docs under Documentation/technical/.
package gitwire

import (
	"bufio"
	"fmt"
	"io"
)

// pkt-line encoding/decoding (gitprotocol-common semantics):
//
//	<pkt-len> <payload>     pkt-len includes the 4 length bytes themselves, hexadecimal
//	0000                    flush-pkt
//	0001                    delim-pkt (v2; reserved but unused in v1)
//	0002                    response-end (v2)

const (
	pktFlush   = 0
	pktDelim   = 1
	pktRespEnd = 2
	maxPktLen  = 65520
)

// PktKind classifies one pkt-line.
type PktKind int

const (
	PktData    PktKind = iota // data pkt (payload follows)
	PktFlush                  // flush-pkt 0000
	PktDelim                  // delim-pkt 0001 (v2 section separator)
	PktRespEnd                // response-end 0002 (v2, stateful transports)
)

// WritePacket writes one data pkt.
func WritePacket(w io.Writer, payload []byte) error {
	if len(payload) > maxPktLen-4 {
		return fmt.Errorf("pkt-line: payload too large (%d)", len(payload))
	}
	if _, err := fmt.Fprintf(w, "%04x", len(payload)+4); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// WriteFlush writes a flush-pkt.
func WriteFlush(w io.Writer) error {
	_, err := io.WriteString(w, "0000")
	return err
}

// WriteDelim writes a delim-pkt (protocol v2 section separator).
func WriteDelim(w io.Writer) error {
	_, err := io.WriteString(w, "0001")
	return err
}

// ReadPacketKind reads one pkt with its kind (flush/delim/response-end have
// no payload).
func ReadPacketKind(r *bufio.Reader) ([]byte, PktKind, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, PktData, err
	}
	var n int
	if _, err := fmt.Sscanf(string(lenBuf[:]), "%04x", &n); err != nil {
		return nil, PktData, fmt.Errorf("pkt-line: bad length %q", lenBuf)
	}
	switch n {
	case pktFlush:
		return nil, PktFlush, nil
	case pktDelim:
		return nil, PktDelim, nil
	case pktRespEnd:
		return nil, PktRespEnd, nil
	}
	if n < 4 {
		return nil, PktData, fmt.Errorf("pkt-line: invalid length %d", n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, PktData, err
	}
	return payload, PktData, nil
}

// ReadPacket reads one pkt. Returns (payload, isFlush, err).
func ReadPacket(r *bufio.Reader) ([]byte, bool, error) {
	payload, kind, err := ReadPacketKind(r)
	if err != nil || kind == PktData {
		return payload, false, err
	}
	if kind == PktFlush {
		return nil, true, nil
	}
	return nil, false, fmt.Errorf("pkt-line: unexpected delim/response-end in v1 stream")
}

// ReadPacketText reads one text pkt (strips the trailing newline).
func ReadPacketText(r *bufio.Reader) (string, bool, error) {
	b, flush, err := ReadPacket(r)
	if err != nil || flush {
		return "", flush, err
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return string(b), false, nil
}
