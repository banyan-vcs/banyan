// Package cliproto is the thin CLI's native protocol lane (docs/client/
// cli.md §4, docs/gateway/service-api.md §3.3): the banyan-owned framing
// both ends of the CLI API speak. It is deliberately NOT the git wire —
// the CLI is our own client and optimizes for itself (binary frames,
// zlib records, delta downlink), while generic git clients keep the
// protocol-v0/v2 compatibility surface untouched.
//
// Versioning: every framing carries a magic line with its version; the
// server answers a request in the highest version it shares with the
// client's "format" hint, and the parser sniffs the magic — a newer CLI
// against an older server silently gets v1 (or JSON), an older CLI never
// sees records it cannot parse. The /api/v1/proto endpoint advertises
// the server's capabilities so clients can skip the trial round trip.
package cliproto

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

const (
	MagicV1 = "banyan-fetch-1"
	MagicV2 = "banyan-fetch-2"

	ContentTypeV1 = "application/x-banyan-fetch-trees"
	ContentTypeV2 = "application/x-banyan-fetch-trees-2"
)

const (
	KindTree  byte = 't'
	KindBlob  byte = 'b'
	KindDelta byte = 'd'
)

// FetchResult is the client-facing shape of a fetch-trees response:
// string OIDs (hex), byte payloads, delta records (v2 only).
type FetchResult struct {
	Branch  string            `json:"branch"`
	Head    string            `json:"head"`
	Tree    string            `json:"tree"`
	Full    bool              `json:"full"`
	Trees   map[string][]byte `json:"trees"`
	Blobs   map[string][]byte `json:"blobs"`
	Deltas  map[string]Delta  `json:"deltas,omitempty"`
	Pending []string          `json:"pending"`
}

// Delta is one REF_DELTA-style downlink record: the changed blob's delta
// against a base the client provably holds (the haveTree diff saw the old
// entry). Only the v2 framing carries them.
type Delta struct {
	Base  string // hex oid of the base blob
	Size  int    // result size
	Delta []byte // the delta program (storage.ApplyDelta applies it)
}

// AlgoName renders the wire name of a hash algorithm (frame headers).
func AlgoName(algo model.HashAlgo) string {
	if algo == model.SHA256 {
		return "sha256"
	}
	return "sha1"
}

func algoName(algo model.HashAlgo) string { return AlgoName(algo) }

func algoWidth(algo model.HashAlgo) int {
	return storage.RawOIDWidth(algo)
}

// WriteV1 renders a FetchResult in the v1 framing (docs/operations/
// performance.md P1-2): magic, JSON header line, then raw records
// [kind('t'|'b')][raw oid][uvarint size][content].
func WriteV1(w io.Writer, res *FetchResult, algo model.HashAlgo) error {
	return writeFrame(w, res, algo, MagicV1, false)
}

// WriteV2 renders a FetchResult in the v2 framing (docs/operations/
// performance.md §9 A1+C): same skeleton, per-record zlib payloads, plus
// 'd' delta records whose frame is [raw base oid][uvarint result size]
// [delta program]. Records are oid-sorted for byte-stable responses.
func WriteV2(w io.Writer, res *FetchResult, algo model.HashAlgo) error {
	return writeFrame(w, res, algo, MagicV2, true)
}

func writeFrame(w io.Writer, res *FetchResult, algo model.HashAlgo, magic string, v2 bool) error {
	bw := bufio.NewWriterSize(w, 64*1024)
	hdr := map[string]any{
		"branch":  res.Branch,
		"head":    res.Head,
		"tree":    res.Tree,
		"full":    res.Full,
		"pending": res.Pending,
		"algo":    algoName(algo),
	}
	if v2 {
		hdr["z"] = 2
	}
	hdrRaw, err := json.Marshal(hdr)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(bw, "%s\n%s\n", magic, hdrRaw); err != nil {
		return err
	}
	width := algoWidth(algo)
	zw := zlib.NewWriter(io.Discard)
	var zbuf bytes.Buffer
	compress := func(content []byte) ([]byte, error) {
		zbuf.Reset()
		zw.Reset(&zbuf)
		if _, err := zw.Write(content); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return zbuf.Bytes(), nil
	}
	var rec []byte
	writeRecord := func(kind byte, oid string, content []byte) error {
		payload := content
		if v2 {
			var err error
			if payload, err = compress(content); err != nil {
				return err
			}
		}
		raw, err := hex.DecodeString(oid)
		if err != nil || len(raw) != width {
			return fmt.Errorf("fetch frame: bad oid %q", oid)
		}
		rec = rec[:0]
		rec = append(rec, kind)
		rec = append(rec, raw...)
		rec = binary.AppendUvarint(rec, uint64(len(payload)))
		rec = append(rec, payload...)
		_, err = bw.Write(rec)
		return err
	}
	for _, oid := range sortedKeys(res.Trees) {
		if err := writeRecord(KindTree, oid, res.Trees[oid]); err != nil {
			return err
		}
	}
	for _, oid := range sortedKeys(res.Blobs) {
		if err := writeRecord(KindBlob, oid, res.Blobs[oid]); err != nil {
			return err
		}
	}
	if v2 {
		for _, oid := range sortedKeys(res.Deltas) {
			d := res.Deltas[oid]
			baseRaw, err := hex.DecodeString(d.Base)
			if err != nil || len(baseRaw) != width {
				return fmt.Errorf("fetch frame: bad delta base %q", d.Base)
			}
			frame := make([]byte, 0, width+10+len(d.Delta))
			frame = append(frame, baseRaw...)
			frame = binary.AppendUvarint(frame, uint64(d.Size))
			frame = append(frame, d.Delta...)
			if err := writeRecord(KindDelta, oid, frame); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

// Parse decodes a fetch-trees body, sniffing the magic line for the
// version — the client accepts whatever the server negotiated (v2, v1)
// and fails loudly on anything else. JSON bodies are the caller's
// fallback (the Content-Type said so before we got here).
func Parse(r io.Reader) (*FetchResult, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	magic, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	switch line := magic[:len(magic)-1]; line {
	case MagicV2:
		return parseRecords(br, true)
	case MagicV1:
		return parseRecords(br, false)
	default:
		return nil, fmt.Errorf("fetch frame: bad magic %q", line)
	}
}

func parseRecords(br *bufio.Reader, v2 bool) (*FetchResult, error) {
	hdrLine, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var hdr struct {
		Branch  string   `json:"branch"`
		Head    string   `json:"head"`
		Tree    string   `json:"tree"`
		Full    bool     `json:"full"`
		Pending []string `json:"pending"`
		Algo    string   `json:"algo"`
	}
	if err := json.Unmarshal([]byte(hdrLine), &hdr); err != nil {
		return nil, fmt.Errorf("fetch frame: header: %w", err)
	}
	oidWidth := algoWidth(model.SHA1)
	if hdr.Algo == "sha256" {
		oidWidth = algoWidth(model.SHA256)
	}
	out := &FetchResult{
		Branch: hdr.Branch, Head: hdr.Head, Tree: hdr.Tree, Full: hdr.Full,
		Pending: hdr.Pending, Trees: map[string][]byte{}, Blobs: map[string][]byte{},
	}
	if v2 {
		out.Deltas = map[string]Delta{}
	}
	oidBuf := make([]byte, oidWidth)
	for {
		kind, err := br.ReadByte()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(br, oidBuf); err != nil {
			return nil, err
		}
		size, err := binary.ReadUvarint(br)
		if err != nil {
			return nil, err
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil, err
		}
		oid := hex.EncodeToString(oidBuf)
		content := payload
		if v2 {
			zr, zerr := zlib.NewReader(bytes.NewReader(payload))
			if zerr != nil {
				return nil, fmt.Errorf("fetch frame: record %s: zlib: %w", oid, zerr)
			}
			content, err = io.ReadAll(zr)
			zr.Close()
			if err != nil {
				return nil, fmt.Errorf("fetch frame: record %s: inflate: %w", oid, err)
			}
		}
		switch kind {
		case KindTree:
			out.Trees[oid] = content
		case KindBlob:
			out.Blobs[oid] = content
		case KindDelta:
			if !v2 {
				return nil, fmt.Errorf("fetch frame: delta record in v1 framing")
			}
			if len(content) < oidWidth {
				return nil, fmt.Errorf("fetch frame: delta record %s: short frame", oid)
			}
			base := hex.EncodeToString(content[:oidWidth])
			rest, resultSize := content[oidWidth:], uint64(0)
			for shift, n := uint(0), 0; ; shift, n = shift+7, n+1 {
				if n >= len(rest) {
					return nil, fmt.Errorf("fetch frame: delta record %s: short size varint", oid)
				}
				b := rest[n]
				resultSize |= uint64(b&0x7f) << shift
				if b&0x80 == 0 {
					rest = rest[n+1:]
					break
				}
			}
			out.Deltas[oid] = Delta{Base: base, Size: int(resultSize), Delta: rest}
		default:
			return nil, fmt.Errorf("fetch frame: bad record kind %q", kind)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
