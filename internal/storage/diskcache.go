package storage

import (
	"container/list"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/banyan-vcs/banyan/internal/model"
)

// DiskPackCache is the gateway's on-disk pack cache (docs/02's first cache
// tier, docs/12 P2-1/B6): whole packs fetched from storage nodes persist
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
