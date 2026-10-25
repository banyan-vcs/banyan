package storage

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
)

func TestSpoolVerifiedCopy(t *testing.T) {
	body := make([]byte, 300_000)
	for i := range body {
		body[i] = byte(i * 7)
	}
	mk := func(sum []byte) []byte { return append(append([]byte{}, body...), sum...) }

	sum1 := sha1.Sum(body)
	sum256 := sha256.Sum256(body)

	// sha1, good trailer
	var dst bytes.Buffer
	n, got, ok, err := SpoolVerifiedCopy(&dst, bytes.NewReader(mk(sum1[:])), model.SHA1)
	if err != nil || !ok || n != int64(len(body)+20) || got != hex.EncodeToString(sum1[:]) {
		t.Fatalf("sha1 ok path: n=%d ok=%v err=%v got=%s", n, ok, err, got)
	}
	if !bytes.Equal(dst.Bytes(), mk(sum1[:])) {
		t.Fatal("roundtrip bytes differ")
	}

	// sha256, good trailer
	_, got, ok, err = SpoolVerifiedCopy(&bytes.Buffer{}, bytes.NewReader(mk(sum256[:])), model.SHA256)
	if err != nil || !ok || got != hex.EncodeToString(sum256[:]) {
		t.Fatalf("sha256 ok path: ok=%v err=%v", ok, err)
	}

	// corrupt trailer: checksum reported, trailerOK false, no error
	bad := mk(sum1[:])
	bad[len(bad)-1] ^= 0xff
	_, _, ok, err = SpoolVerifiedCopy(&bytes.Buffer{}, bytes.NewReader(bad), model.SHA1)
	if err != nil || ok {
		t.Fatalf("bad trailer: ok=%v err=%v", ok, err)
	}

	// too short
	if _, _, _, err := SpoolVerifiedCopy(&bytes.Buffer{}, bytes.NewReader([]byte("short")), model.SHA1); err == nil ||
		!strings.Contains(err.Error(), "too short") {
		t.Fatalf("short input: err=%v", err)
	}
}

func TestReceivePackRoundtrip(t *testing.T) {
	store := NewLocalShardStore(t.TempDir())
	_, _, _, pack := buildTestPack(t)
	oid := PackChecksum(pack)
	r := func() *bytes.Reader { return bytes.NewReader(pack) }

	if err := store.ReceivePack("r", oid, int64(len(pack)), r(), nil); err != nil {
		t.Fatalf("receive: %v", err)
	}
	f, size, err := store.OpenPack("r", oid)
	if err != nil || size != int64(len(pack)) {
		t.Fatalf("open: err=%v size=%d", err, size)
	}
	got := make([]byte, size)
	if _, err := f.Read(got); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if !bytes.Equal(got, pack) {
		t.Fatal("streamed bytes differ from input")
	}
	if sz, err := store.PackFileSize("r", oid); err != nil || sz != int64(len(pack)) {
		t.Fatalf("PackFileSize: %d %v", sz, err)
	}

	// wrong OID → identity check fails, nothing lands
	if err := store.ReceivePack("r", model.OID(strings.Repeat("f", len(string(oid)))), int64(len(pack)), r(), nil); err == nil ||
		!strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("oid mismatch: err=%v", err)
	}
	// corrupt body → trailer check fails
	corrupt := append([]byte{}, pack...)
	corrupt[len(corrupt)-3] ^= 0xff
	if err := store.ReceivePack("r", oid, int64(len(corrupt)), bytes.NewReader(corrupt), nil); err == nil {
		t.Fatal("corrupt pack accepted")
	}
	// size mismatch (declared Content-Length)
	if err := store.ReceivePack("r", oid, int64(len(pack))-1, r(), nil); err == nil ||
		!strings.Contains(err.Error(), "short copy") {
		t.Fatalf("size mismatch: err=%v", err)
	}
	// leftover temp files must not accumulate
	ents, _ := os.ReadDir(filepath.Join(store.Root, "r", "packs", string(oid)[:2]))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".recv-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// PackChecksum returns the pack's trailer checksum as its OID.
func PackChecksum(pack []byte) model.OID {
	sum := sha1.Sum(pack[:len(pack)-20])
	return model.OID(hex.EncodeToString(sum[:]))
}

func TestDiskCacheStreaming(t *testing.T) {
	c, err := NewDiskPackCache(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	put, err := c.BeginPut("r", model.OID(strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put.Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	// mid-stream abort leaves nothing behind
	put.Abort()
	if _, ok := c.SizeHit("r", model.OID(strings.Repeat("a", 40))); ok {
		t.Fatal("aborted put is registered")
	}

	oid := model.OID(strings.Repeat("b", 40))
	put, err = c.BeginPut("r", oid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put.Write([]byte("pack-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := put.Complete(); err != nil {
		t.Fatal(err)
	}
	if size, ok := c.SizeHit("r", oid); !ok || size != 10 {
		t.Fatalf("SizeHit: %d %v", size, ok)
	}
	if raw, ok := c.GetCapped("r", oid, 32<<20); !ok || string(raw) != "pack-bytes" {
		t.Fatalf("GetCapped under cap: %q %v", raw, ok)
	}
	if _, ok := c.GetCapped("r", oid, 4); ok {
		t.Fatal("GetCapped over cap must miss")
	}
	f, size, ok := c.Open("r", oid)
	if !ok || size != 10 {
		t.Fatalf("Open: %d %v", size, ok)
	}
	f.Close()
}
