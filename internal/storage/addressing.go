// Repository addressing modes (docs/03 §6): how a repo name becomes the
// on-disk / on-wire storage key every pack path is nested under.
//
//	named  (default)  key = repo name. Human-readable storage trees; a
//	                   rename (or group migration) must move data on every
//	                   holder — RenameRepo fans the move out first and flips
//	                   the registry last.
//	hashed             key = "@hashed/<xx>/<yy>/<sha256(storageID)>" derived
//	                   from the repo's IMMUTABLE storage id, so renames are a
//	                   registry-only operation — the storage layer never
//	                   moves a byte and never learns a name. The gateway
//	                   resolves name → key through this Addressing (i.e. the
//	                   mapping lives at the gateway/meta seam, not on disk).
package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// NewStorageID mints the immutable per-repo storage identity (32 hex chars).
func NewStorageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", sha256.Sum256([]byte(fmt.Sprintf("%p", &b))))
	}
	return hex.EncodeToString(b[:])
}

// HashedRepoPath lays a storage id out GitLab-@hashed style: two one-byte
// fanout levels then the full digest, keeping any single directory small.
func HashedRepoPath(storageID string) string {
	sum := sha256.Sum256([]byte(storageID))
	h := hex.EncodeToString(sum[:])
	return "@hashed/" + h[0:2] + "/" + h[2:4] + "/" + h
}

// Addressing resolves repo names to storage keys. Nil (and mode "named") is
// the identity mapping — the pre-existing behavior every deployment defaults
// to. Hashed mode caches name→key forever: keys derive from the immutable
// storage id, so entries can never go stale.
type Addressing struct {
	Mode string // "named" | "hashed"; anything else behaves as named
	Meta MetaStore

	mu   sync.Mutex
	keys map[string]string
}

// NewAddressing builds an addressing for a mode; nil is returned for the
// named default so callers pay nothing for it.
func NewAddressing(mode string, meta MetaStore) *Addressing {
	if mode != "hashed" {
		return nil
	}
	return &Addressing{Mode: mode, Meta: meta, keys: map[string]string{}}
}

// Key maps a repo name to its storage key (docs/03 §6).
func (a *Addressing) Key(ctx context.Context, repo string) (string, error) {
	if a == nil || a.Mode != "hashed" {
		return repo, nil
	}
	a.mu.Lock()
	key, ok := a.keys[repo]
	a.mu.Unlock()
	if ok {
		return key, nil
	}
	info, err := a.Meta.GetRepo(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("addressing %s: %w", repo, err)
	}
	if info.StorageID == "" {
		// Rows without a storage id predate the column (or a store skipped
		// the backfill). Refuse rather than guess a key: a wrong guess
		// strands every pack of the repo.
		return "", fmt.Errorf("addressing %s: no storage id (run the sqlite backfill / recreate the repo)", repo)
	}
	key = HashedRepoPath(info.StorageID)
	a.mu.Lock()
	a.keys[repo] = key
	a.mu.Unlock()
	return key, nil
}
