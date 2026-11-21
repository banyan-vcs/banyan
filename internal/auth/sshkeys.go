// User-managed SSH keys as a decorator over any auth.Service
// (docs/gateway/authorization.md §7a, docs/design/data-model.md §17): the
// metadata-plane key table answers the SSH handshake before the inner
// policy's config-file keys. A registered key authenticates as its OWNER —
// the username the SSH connection claims is advisory (the hosted-git
// convention: URLs say git@ or alice@, the key alone decides the account).
// Keys not in the table delegate to the inner policy unchanged, so static
// config keys keep working alongside console-managed ones.
//
//	KeyPolicy{Inner: MemberPolicy{Inner: TokenPolicy{Inner: StaticPolicy{…}}}, Keys: ms}
//
// SSH keys are UNSCOPED credentials — they carry no token ceiling — which
// is why the rpc layer gates adding one behind write rank (a read-scoped
// PAT must not mint itself a full-rank key).
package auth

import (
	"context"
	"net/http"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// KeyPolicy wraps an inner Service with the user key table. All methods
// are safe for concurrent use.
type KeyPolicy struct {
	Inner Service
	Keys  meta.SSHKeyStore
}

// Identify delegates: the key table is an SSH credential, HTTP Basic
// never touches it.
func (p *KeyPolicy) Identify(h *http.Request) Identity {
	return p.Inner.Identify(h)
}

// IdentifyKey: the table's owner answers first (a store error or a miss
// falls through to the inner policy — availability, with the inner layer
// still deciding admission).
func (p *KeyPolicy) IdentifyKey(user string, keyBlob []byte) Identity {
	if owner, err := p.Keys.IdentifySSHKey(context.Background(), keyBlob); err == nil && owner != "" {
		return Identity{User: owner, Name: owner}
	}
	return p.Inner.IdentifyKey(user, keyBlob)
}

func (p *KeyPolicy) VisibleRefs(id Identity, repo string, refs []Ref) []Ref {
	return p.Inner.VisibleRefs(id, repo, refs)
}

func (p *KeyPolicy) CanFetch(id Identity, repo, sub string) bool {
	return p.Inner.CanFetch(id, repo, sub)
}

func (p *KeyPolicy) CanPush(id Identity, repo, sub, branch string) bool {
	return p.Inner.CanPush(id, repo, sub, branch)
}

// CanAdmin (auth.AdminChecker): the inner policy's admins — the key table
// authenticates, it never widens the management plane.
func (p *KeyPolicy) CanAdmin(id Identity, repo string) bool {
	if ac, ok := p.Inner.(AdminChecker); ok {
		return ac.CanAdmin(id, repo)
	}
	return false
}
