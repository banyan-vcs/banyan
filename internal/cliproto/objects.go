package cliproto

// The uplink twin of the fetch framing (docs/operations/performance.md §9
// A2): object upload as binary frames with per-record zlib — the JSON
// ladder's base64 (+33%) and zero compression drop away, and a commit's
// upload bytes approach git push pack territory. Same versioning rules:
// the magic carries the version; an older server answers 400 on the
// unknown Content-Type and the client falls back to JSON once.
//
// Layout:
//
//	"banyan-objects-1\n"
//	<one JSON header line> + "\n"            {"repo":...,"algo":"sha1|sha256"}
//	*record: kind('b'|'t'|'c'|'g') <raw oid> <uvarint compressed size> <zlib data>

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const (
	MagicObjectsV1    = "banyan-objects-1"
	ContentTypeObjects = "application/x-banyan-objects-1"
)

// ObjectRecord is one uploaded object; the service layer's hash
// verification (content addressing) applies unchanged.
type ObjectRecord struct {
	OID  string
	Type string // "blob" | "tree" | "commit" | "tag"
	Data []byte
}

func objKind(typ string) (byte, bool) {
	switch typ {
	case "blob":
		return 'b', true
	case "tree":
		return 't', true
	case "commit":
		return 'c', true
	case "tag":
		return 'g', true
	}
	return 0, false
}

func objType(kind byte) (string, bool) {
	switch kind {
	case 'b':
		return "blob", true
	case 't':
		return "tree", true
	case 'c':
		return "commit", true
	case 'g':
		return "tag", true
	}
	return "", false
}

// WriteObjectsFrame renders an upload batch. algo names the oid hash
// ("sha1"/"sha256") — the width follows.
func WriteObjectsFrame(w io.Writer, repo, algo string, objs []ObjectRecord) error {
	width := 20
	if algo == "sha256" {
		width = 32
	}
	bw := bufio.NewWriterSize(w, 64*1024)
	hdr, err := json.Marshal(map[string]string{"repo": repo, "algo": algo})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(bw, "%s\n%s\n", MagicObjectsV1, hdr); err != nil {
		return err
	}
	zw := zlib.NewWriter(io.Discard)
	var zbuf bytes.Buffer
	var rec []byte
	for _, o := range objs {
		kind, ok := objKind(o.Type)
		if !ok {
			return fmt.Errorf("objects frame: bad type %q", o.Type)
		}
		raw, err := hex.DecodeString(o.OID)
		if err != nil || len(raw) != width {
			return fmt.Errorf("objects frame: bad oid %q", o.OID)
		}
		zbuf.Reset()
		zw.Reset(&zbuf)
		if _, err := zw.Write(o.Data); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		rec = rec[:0]
		rec = append(rec, kind)
		rec = append(rec, raw...)
		rec = binary.AppendUvarint(rec, uint64(zbuf.Len()))
		rec = append(rec, zbuf.Bytes()...)
		if _, err := bw.Write(rec); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ParseObjectsFrame decodes an upload batch (the server side).
func ParseObjectsFrame(r io.Reader) (repo string, objs []ObjectRecord, err error) {
	br := bufio.NewReaderSize(r, 64*1024)
	magic, err := br.ReadString('\n')
	if err != nil || magic != MagicObjectsV1+"\n" {
		return "", nil, fmt.Errorf("objects frame: bad magic %q", magic)
	}
	hdrLine, err := br.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	var hdr struct {
		Repo string `json:"repo"`
		Algo string `json:"algo"`
	}
	if err := json.Unmarshal([]byte(hdrLine), &hdr); err != nil {
		return "", nil, fmt.Errorf("objects frame: header: %w", err)
	}
	width := 20
	if hdr.Algo == "sha256" {
		width = 32
	}
	oidBuf := make([]byte, width)
	for {
		kind, err := br.ReadByte()
		if err == io.EOF {
			return hdr.Repo, objs, nil
		}
		if err != nil {
			return "", nil, err
		}
		typ, ok := objType(kind)
		if !ok {
			return "", nil, fmt.Errorf("objects frame: bad record kind %q", kind)
		}
		if _, err := io.ReadFull(br, oidBuf); err != nil {
			return "", nil, err
		}
		size, err := binary.ReadUvarint(br)
		if err != nil {
			return "", nil, err
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			return "", nil, err
		}
		zr, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			return "", nil, fmt.Errorf("objects frame: record zlib: %w", err)
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return "", nil, fmt.Errorf("objects frame: record inflate: %w", err)
		}
		objs = append(objs, ObjectRecord{OID: hex.EncodeToString(oidBuf), Type: typ, Data: data})
	}
}
