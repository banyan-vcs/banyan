// Server-managed passwords (docs/gateway/authorization.md §2a): bcrypt
// hashes in the metadata-plane account table, consulted by StaticPolicy
// before the config-file table. Hashing lives here — the store keeps the
// hash opaque. The verified-credential cache exists because the web tier
// re-sends the session's Basic header on EVERY proxied call: bcrypt is
// intentionally slow (~100ms), so a verified pair rides a short TTL
// instead of re-hashing per request. A password change takes effect when
// the TTL lapses (≤ verifiedTTL).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword answers the bcrypt hash for storage (bootstrap, password
// change, the demo seeder).
func HashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// verifyPassword answers whether plain matches the stored hash.
func verifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// CheckPassword is verifyPassword for consumers outside the package (the
// password-change endpoint re-verifies the current credential).
func CheckPassword(hash, plain string) bool {
	return verifyPassword(hash, plain)
}

// RandomPassword answers a fresh url-safe secret (first-boot administrator;
// crypto/rand). 16 chars of the base64url alphabet ≈ 96 bits.
func RandomPassword() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failing is unrecoverable here; a panic at bootstrap is
		// louder and safer than a guessable password.
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// CredentialInvalidator is the optional password-change hook: the wired
// policy drops its verified-credential cache when a password rotates, so
// the old value dies immediately instead of at TTL lapse. Decorators
// forward it inward.
type CredentialInvalidator interface {
	InvalidateCredentials()
}

// verifiedTTL bounds how long a verified (user, password) pair is trusted
// without re-hashing. Short on purpose: it is also the worst-case window
// in which a changed password's OLD value still authenticates.
const verifiedTTL = 60 * time.Second

// verifiedCap bounds the cache; a full map resets rather than grows
// unbounded (a reset only costs re-hashing, never correctness).
const verifiedCap = 1024

// credentialCache is the TTL cache behind StaticPolicy.Identify.
type credentialCache struct {
	mu      sync.Mutex
	entries map[[32]byte]time.Time // sha256(user ∥ pass) → expiry
	now     func() time.Time       // tests
}

func (c *credentialCache) get(key [32]byte) bool {
	if c.entries == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[key]
	if !ok {
		return false
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	if !now.Before(exp) {
		delete(c.entries, key)
		return false
	}
	return true
}

func (c *credentialCache) put(key [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[[32]byte]time.Time)
	}
	if len(c.entries) >= verifiedCap {
		c.entries = make(map[[32]byte]time.Time) // crude bound: reset, re-hash on miss
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	c.entries[key] = now.Add(verifiedTTL)
}

func credKey(user, pass string) [32]byte {
	return sha256.Sum256([]byte(user + "\x00" + pass))
}

// identifyStored resolves HTTP Basic against the account table. The table
// OWNS the names it carries: a known name with a wrong password answers
// anonymous with handled=true — no fall-through to a same-name config
// entry. Only names the table does not know (or a store error) fall
// through, so config-file accounts keep working alongside managed ones.
func (p *StaticPolicy) identifyStored(user, pass string) (id Identity, handled bool) {
	if p.UserSource == nil {
		return Identity{}, false
	}
	key := credKey(user, pass)
	if p.verified.get(key) {
		return Identity{User: user, Name: user}, true
	}
	u, err := p.UserSource.UserByName(context.Background(), user)
	if err != nil {
		return Identity{}, false // unknown name (or store error): config may know it
	}
	if !verifyPassword(u.Password, pass) {
		return Identity{}, true
	}
	p.verified.put(key)
	return Identity{User: user, Name: user}, true
}

// InvalidateCredentials drops the verified-credential cache (a password
// change calls it through the wiring that can reach the policy).
func (p *StaticPolicy) InvalidateCredentials() {
	p.verified.mu.Lock()
	p.verified.entries = nil
	p.verified.mu.Unlock()
}
