package rpc

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// fetch-trees binary framing (docs/05 §3.3; docs/11 P1-2, B4): the JSON
// response carried tree/blob bytes as base64 inside a map — a flat +33% on
// the thin client's dominant transfer. The thin CLI is our own consumer, so
// the wire switches to binary frames negotiated per request ("format":"bin");
// the JSON response stays for compatibility and non-CLI consumers.
//
// Layout:
//
//	"banyan-fetch-1\n"                       magic line
//	<one JSON header line> + "\n"            branch/head/tree/full/pending/algo
//	*record: kind('t'|'b') <raw oid> <uvarint size> <content>
//
// Records run to end of body; the header line carries everything the client
// needs before the first record.
const fetchBinaryMagic = "banyan-fetch-1"

const (
	fetchKindTree byte = 't'
	fetchKindBlob byte = 'b'
)

func fetchAlgoName(algo model.HashAlgo) string {
	if algo == model.SHA256 {
		return "sha256"
	}
	return "sha1"
}

// writeFetchBinary renders a FetchResult in the binary framing.
func writeFetchBinary(w io.Writer, res *service.FetchResult, algo model.HashAlgo) error {
	bw := bufio.NewWriterSize(w, 64*1024)
	hdr, err := json.Marshal(map[string]any{
		"branch":  res.Branch,
		"head":    res.Head,
		"tree":    res.Tree,
		"full":    res.Full,
		"pending": res.Pending,
		"algo":    fetchAlgoName(algo),
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(bw, "%s\n%s\n", fetchBinaryMagic, hdr); err != nil {
		return err
	}
	width := storage.RawOIDWidth(algo)
	var rec []byte
	writeRecord := func(kind byte, oid string, content []byte) error {
		raw, err := hex.DecodeString(oid)
		if err != nil || len(raw) != width {
			return fmt.Errorf("fetch binary: bad oid %q", oid)
		}
		rec = rec[:0]
		rec = append(rec, kind)
		rec = append(rec, raw...)
		rec = binary.AppendUvarint(rec, uint64(len(content)))
		rec = append(rec, content...)
		_, err = bw.Write(rec)
		return err
	}
	for oid, content := range res.Trees {
		if err := writeRecord(fetchKindTree, oid, content); err != nil {
			return err
		}
	}
	for oid, content := range res.Blobs {
		if err := writeRecord(fetchKindBlob, oid, content); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ParseFetchBinary decodes the binary framing (thin CLI side). Returns the
// header plus the tree/blob maps keyed by hex oid.
func ParseFetchBinary(r io.Reader) (branch, head, tree string, full bool, pending []string, trees, blobs map[string][]byte, err error) {
	br := bufio.NewReaderSize(r, 64*1024)
	magic, err := br.ReadString('\n')
	if err != nil || strings.TrimSuffix(magic, "\n") != fetchBinaryMagic {
		return "", "", "", false, nil, nil, nil, fmt.Errorf("fetch binary: bad magic %q", magic)
	}
	hdrLine, err := br.ReadString('\n')
	if err != nil {
		return "", "", "", false, nil, nil, nil, err
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
		return "", "", "", false, nil, nil, nil, fmt.Errorf("fetch binary: header: %w", err)
	}
	width := storage.RawOIDWidth(model.SHA1)
	if hdr.Algo == "sha256" {
		width = storage.RawOIDWidth(model.SHA256)
	}
	trees, blobs = map[string][]byte{}, map[string][]byte{}
	oidBuf := make([]byte, width)
	for {
		kind, err := br.ReadByte()
		if err == io.EOF {
			return hdr.Branch, hdr.Head, hdr.Tree, hdr.Full, hdr.Pending, trees, blobs, nil
		}
		if err != nil {
			return "", "", "", false, nil, nil, nil, err
		}
		if _, err := io.ReadFull(br, oidBuf); err != nil {
			return "", "", "", false, nil, nil, nil, err
		}
		size, err := binary.ReadUvarint(br)
		if err != nil {
			return "", "", "", false, nil, nil, nil, err
		}
		content := make([]byte, size)
		if _, err := io.ReadFull(br, content); err != nil {
			return "", "", "", false, nil, nil, nil, err
		}
		oid := hex.EncodeToString(oidBuf)
		switch kind {
		case fetchKindTree:
			trees[oid] = content
		case fetchKindBlob:
			blobs[oid] = content
		default:
			return "", "", "", false, nil, nil, nil, fmt.Errorf("fetch binary: bad record kind %q", kind)
		}
	}
}
