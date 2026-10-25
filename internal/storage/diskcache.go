package storage

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
)

// DiskPackCache is the gateway's on-disk pack cache (docs/03's first cache
// tier, docs/11 P2-1/B6): whole packs fetched from storage nodes persist
// across gateway restarts, so the first clone after a restart serves from
// local disk instead of re-transferring megabytes per pack.
//
// Content addressing carries the correctness story: the file is named by the
// pack OID, whose trailer checksum was verified before the node ever
// confirmed the write — so a present file IS the pack, and reads trust it
// exactly like LocalShardStore trusts its own files. Eviction is LRU by
// total bytes; a restart rebuilds the index by rescanning the directory
// (fresh-insertion order, cheapest deterministic policy).
type DiskPackCache struct {
	Root     string
	MaxBytes int64

	mu    sync.Mutex
	lru   *list.List // diskEntry, front = most recently used
	index map[string]*list.Element
	bytes int64
}

type diskEntry struct {
	key  string // "<repo>/<packOID>"
	size int64
}

// NewDiskPackCache opens (and rescans) the cache directory.
func NewDiskPackCache(root string, maxBytes int64) (*DiskPackCache, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 30 // 1 GiB default
	}
	c := &DiskPackCache{Root: root, MaxBytes: maxBytes, lru: list.New(), index: map[string]*list.Element{}}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	// Rebuild the index from disk: every file present is a valid cached pack.
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		slash := strings.LastIndex(rel, "/")
		if slash < 1 || !strings.HasSuffix(rel, ".pack") {
			return nil // foreign file; leave it alone
		}
		key := rel[:slash] + "/" + strings.TrimSuffix(rel[slash+1:], ".pack")
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		c.index[key] = c.lru.PushFront(diskEntry{key: key, size: info.Size()})
		c.bytes += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.evictLocked()
	return c, nil
}

// safeKey guards the path construction: repo names reach the cache from
// request-derived strings, so anything path-shaped is refused (not cached).
func safeKey(repo string, packOID model.OID) (string, bool) {
	if repo == "" || packOID.IsZero() || strings.ContainsAny(repo, "/\\") || repo == "." || repo == ".." || strings.ContainsAny(string(packOID), "/\\") {
		return "", false
	}
	return repo + "/" + string(packOID), true
}

func (c *DiskPackCache) path(key string) string {
	repo, pack, _ := strings.Cut(key, "/")
	return filepath.Join(c.Root, filepath.FromSlash(repo), pack+".pack")
}

// Get returns the cached pack bytes, touching LRU order.
func (c *DiskPackCache) Get(repo string, packOID model.OID) ([]byte, bool) {
	key, ok := safeKey(repo, packOID)
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	el, hit := c.index[key]
	c.mu.Unlock()
	if !hit {
		return nil, false
	}
	raw, err := os.ReadFile(c.path(key))
	if err != nil {
		// Vanished underneath us (operator cleanup, eviction race): drop it.
		c.mu.Lock()
		c.lru.Remove(el)
		delete(c.index, key)
		c.mu.Unlock()
		return nil, false
	}
	c.mu.Lock()
	c.lru.MoveToFront(el)
	c.mu.Unlock()
	return raw, true
}

// Put stores a pack atomically (tmp + rename) and evicts past the cap.
// Best-effort: a failure to cache never fails the read path.
func (c *DiskPackCache) Put(repo string, packOID model.OID, raw []byte) error {
	key, ok := safeKey(repo, packOID)
	if !ok {
		return nil
	}
	path := c.path(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, dup := c.index[key]; dup {
		c.lru.MoveToFront(el)
		return nil
	}
	c.index[key] = c.lru.PushFront(diskEntry{key: key, size: int64(len(raw))})
	c.bytes += int64(len(raw))
	c.evictLocked()
	return nil
}

func (c *DiskPackCache) evictLocked() {
	for c.bytes > c.MaxBytes && c.lru.Len() > 0 {
		oldest := c.lru.Back()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(diskEntry)
		c.lru.Remove(oldest)
		delete(c.index, entry.key)
		c.bytes -= entry.size
		_ = os.Remove(c.path(entry.key))
	}
}

// Stats reports (entries, bytes) — observability + tests.
func (c *DiskPackCache) Stats() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.bytes
}

// GetCapped returns the cached pack bytes only when the file is at most
// maxBytes (the whole-pack memoize guard): a big cached pack is not "cheap"
// — loading it would park the entire pack in gateway memory. An over-cap or
// missing entry answers false without touching LRU order.
func (c *DiskPackCache) GetCapped(repo string, packOID model.OID, maxBytes int64) ([]byte, bool) {
	key, ok := safeKey(repo, packOID)
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	el, hit := c.index[key]
	if hit && el.Value.(diskEntry).size > maxBytes {
		c.mu.Unlock()
		return nil, false
	}
	c.mu.Unlock()
	if !hit {
		return nil, false
	}
	return c.Get(repo, packOID)
}

// Open streams a cached pack straight from disk (exact-pack verbatim
// serve): disk → consumer, no memory transit. A hit touches LRU order.
func (c *DiskPackCache) Open(repo string, packOID model.OID) (*os.File, int64, bool) {
	key, ok := safeKey(repo, packOID)
	if !ok {
		return nil, 0, false
	}
	c.mu.Lock()
	el, hit := c.index[key]
	if hit {
		c.lru.MoveToFront(el)
	}
	c.mu.Unlock()
	if !hit {
		return nil, 0, false
	}
	f, err := os.Open(c.path(key))
	if err != nil {
		return nil, 0, false
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, false
	}
	return f, st.Size(), true
}

// SizeHit reports a cached pack's size from the index, no I/O, no LRU touch.
func (c *DiskPackCache) SizeHit(repo string, packOID model.OID) (int64, bool) {
	key, ok := safeKey(repo, packOID)
	if !ok {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, hit := c.index[key]
	if !hit {
		return 0, false
	}
	return el.Value.(diskEntry).size, true
}

// CachePut is an in-progress streaming cache write (the tee target of
// OpenPack's write-through): chunks land in a temp file; Complete promotes
// atomically into the cache, Abort discards.
type CachePut struct {
	c    *DiskPackCache
	key  string
	path string
	f    *os.File
	n    int64
	done bool
}

// BeginPut opens a temp cache file for a streaming write.
func (c *DiskPackCache) BeginPut(repo string, packOID model.OID) (*CachePut, error) {
	key, ok := safeKey(repo, packOID)
	if !ok {
		return nil, fmt.Errorf("cache: unsafe key")
	}
	path := c.path(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp := path + ".streaming"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	return &CachePut{c: c, key: key, path: tmp, f: f}, nil
}

func (p *CachePut) Write(b []byte) (int, error) {
	n, err := p.f.Write(b)
	p.n += int64(n)
	return n, err
}

// Complete lands the cached file and registers it, evicting past the cap
// (possibly this very entry if it alone exceeds MaxBytes — honest).
func (p *CachePut) Complete() error {
	p.done = true
	if err := p.f.Close(); err != nil {
		_ = os.Remove(p.path)
		return err
	}
	final := p.c.path(p.key)
	if err := os.Rename(p.path, final); err != nil {
		_ = os.Remove(p.path)
		return err
	}
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, dup := c.index[p.key]; dup {
		c.lru.MoveToFront(el)
		return nil
	}
	c.index[p.key] = c.lru.PushFront(diskEntry{key: p.key, size: p.n})
	c.bytes += p.n
	c.evictLocked()
	return nil
}

// Abort discards the temp file (the consumer broke off mid-stream).
func (p *CachePut) Abort() {
	if p.done {
		return
	}
	p.f.Close()
	_ = os.Remove(p.path)
}
