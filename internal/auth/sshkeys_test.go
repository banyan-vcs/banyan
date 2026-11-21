package auth

// KeyPolicy (docs/gateway/authorization.md §7a): the user key table answers
// the SSH handshake as the key's owner regardless of the username the
// connection claims (the hosted-git convention), keys not in the table
// fall through to the inner policy (config keys keep working), the inner
// policy's own rules keep applying underneath, and a broken table falls
// through too — the inner layer still decides admission, never an error.
import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func TestKeyPolicy(t *testing.T) {
	ms := meta.NewMemoryMetaStore()
	ctx := context.Background()
	// alice registers one key (blob "alice-key"); carol none. The inner
	// policy carries a config key for bob (blob "bob-key") — the coexistence
	// the decorator must preserve.
	if err := ms.AddSSHKey(ctx, meta.SSHKey{
		Fingerprint: "SHA256:alice", User: "alice", Type: "ssh-ed25519",
		Blob: base64.StdEncoding.EncodeToString([]byte("alice-key")), AddedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	inner := &StaticPolicy{
		Users: map[string]string{"alice": "a", "bob": "b", "root": "r"},
		Keys: map[string][]string{
			"bob": {"ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("bob-key")) + " bob@desk"},
		},
	}
	p := &KeyPolicy{Inner: inner, Keys: ms}

	// --- the table answers as the key's owner, whatever the connection claims ---
	for _, claim := range []string{"alice", "git", "bob"} {
		if id := p.IdentifyKey(claim, []byte("alice-key")); id.User != "alice" {
			t.Errorf("alice's key as %q: got %q want alice", claim, id.User)
		}
	}
	// --- a config key still works through the fall-through ---
	if id := p.IdentifyKey("bob", []byte("bob-key")); id.User != "bob" {
		t.Errorf("bob's config key: got %q want bob", id.User)
	}
	// ...but only under its owner's claimed name (the static policy's rule)
	if id := p.IdentifyKey("alice", []byte("bob-key")); !id.Anonymous() {
		t.Errorf("bob's config key claimed as alice must stay the inner policy's refusal: got %q", id.User)
	}
	// --- an unknown key stays anonymous ---
	if id := p.IdentifyKey("alice", []byte("nobody-key")); !id.Anonymous() {
		t.Errorf("unknown key: got %q", id.User)
	}

	// --- the inner rules keep applying under a key-authenticated identity ---
	inner.ProtectedBranches = []string{"main"}
	if p.CanPush(Identity{User: "alice"}, "demo", "", "feature") != true {
		t.Error("alice (key-authenticated) pushes like any authenticated user on an open branch")
	}
	if p.CanPush(Identity{User: "alice"}, "demo", "", "main") != false {
		t.Error("inner protected branch must keep applying")
	}
	inner.Admins = []string{"root"}
	if ac, ok := interface{}(p).(AdminChecker); !ok {
		t.Fatal("KeyPolicy must forward AdminChecker")
	} else if ac.CanAdmin(Identity{User: "alice"}, "") {
		t.Error("the key table never widens the management plane")
	} else if !ac.CanAdmin(Identity{User: "root"}, "") {
		t.Error("inner admins stay admins")
	}

	// --- a broken table falls through to the inner policy, never errors open ---
	p.Keys = brokenSSHKeyStore{}
	if id := p.IdentifyKey("bob", []byte("bob-key")); id.User != "bob" {
		t.Errorf("broken table must fall through to the inner policy: got %q", id.User)
	}
	if id := p.IdentifyKey("alice", []byte("alice-key")); !id.Anonymous() {
		t.Errorf("broken table must not authenticate the table's keys: got %q", id.User)
	}
}

type brokenSSHKeyStore struct{}

func (brokenSSHKeyStore) AddSSHKey(context.Context, meta.SSHKey) error  { return errors.New("boom") }
func (brokenSSHKeyStore) SSHKeys(context.Context, string) ([]meta.SSHKey, error) {
	return nil, errors.New("boom")
}
func (brokenSSHKeyStore) DeleteSSHKey(context.Context, string, string) error { return errors.New("boom") }
func (brokenSSHKeyStore) IdentifySSHKey(context.Context, []byte) (string, error) {
	return "", errors.New("boom")
}
