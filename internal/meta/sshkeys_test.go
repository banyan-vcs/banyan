package meta

// SSH key store conformance (docs/design/data-model.md §17): the same
// battery on every backend. The load-bearing behaviors: one key, one
// account (the fingerprint is taken globally, not per user), identification
// by wire-format blob (the SSH handshake's lookup), newest-first listing,
// and owner-scoped deletion.
import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
)

func TestSSHKeyStoreConformance(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			ms, done := o(t)
			defer done()
			ks, ok := ms.(SSHKeyStore)
			if !ok {
				t.Fatalf("backend %s does not implement SSHKeyStore", name)
			}
			ctx := context.Background()

			k1 := SSHKey{Fingerprint: "SHA256:one", User: "alice", Type: "ssh-ed25519", Blob: "QUJDRA==", Comment: "laptop", AddedAt: 2}
			k2 := SSHKey{Fingerprint: "SHA256:two", User: "alice", Type: "ssh-ed25519", Blob: "REVG", Comment: "desk", AddedAt: 3}
			k3 := SSHKey{Fingerprint: "SHA256:three", User: "bob", Type: "ssh-rsa", Blob: "R0hJSg==", Comment: "", AddedAt: 1}
			for _, k := range []SSHKey{k1, k2, k3} {
				if err := ks.AddSSHKey(ctx, k); err != nil {
					t.Fatalf("add %s: %v", k.Fingerprint, err)
				}
			}

			// --- one key, one account: the second registration bounces —
			// whoever asks, whatever username it names ---
			if err := ks.AddSSHKey(ctx, SSHKey{Fingerprint: "SHA256:one", User: "bob", Type: "ssh-ed25519", Blob: "QUJDRA=="}); !errors.Is(err, ErrSSHKeyExists) {
				t.Fatalf("duplicate fingerprint must bounce: %v", err)
			}
			if err := ks.AddSSHKey(ctx, SSHKey{Fingerprint: "", User: "bob", Type: "x", Blob: "y"}); !errors.Is(err, ErrSSHKeyInvalid) {
				t.Fatalf("empty fingerprint must bounce: %v", err)
			}

			// --- listing: newest first, per user ---
			alice, err := ks.SSHKeys(ctx, "alice")
			if err != nil || len(alice) != 2 || alice[0].Fingerprint != "SHA256:two" || alice[1].Fingerprint != "SHA256:one" {
				t.Fatalf("alice keys: %+v err=%v", alice, err)
			}
			if got := alice[0].Comment; got != "desk" {
				t.Fatalf("comment must ride along: %q", got)
			}

			// --- identification by wire-format blob (decode the fixtures:
			// the handshake presents raw bytes, the table stores base64) ---
			for b64, want := range map[string]string{"QUJDRA==": "alice", "REVG": "alice", "R0hJSg==": "bob"} {
				raw, err := base64.StdEncoding.DecodeString(b64)
				if err != nil {
					t.Fatal(err)
				}
				user, err := ks.IdentifySSHKey(ctx, raw)
				if err != nil || user != want {
					t.Fatalf("identify %s: got %q err=%v want %q", b64, user, err, want)
				}
			}
			if user, err := ks.IdentifySSHKey(ctx, []byte("not-a-key")); err != nil || user != "" {
				t.Fatalf("unknown blob must answer empty: got %q err=%v", user, err)
			}

			// --- deletion is owner-scoped ---
			if err := ks.DeleteSSHKey(ctx, "bob", "SHA256:one"); !errors.Is(err, ErrSSHKeyUnknown) {
				t.Fatalf("bob must not delete alice's key: %v", err)
			}
			if err := ks.DeleteSSHKey(ctx, "alice", "SHA256:missing"); !errors.Is(err, ErrSSHKeyUnknown) {
				t.Fatalf("unknown fingerprint must bounce: %v", err)
			}
			if err := ks.DeleteSSHKey(ctx, "alice", "SHA256:one"); err != nil {
				t.Fatalf("delete: %v", err)
			}
			raw1, _ := base64.StdEncoding.DecodeString("QUJDRA==")
			if user, _ := ks.IdentifySSHKey(ctx, raw1); user != "" {
				t.Fatalf("deleted key must not identify: got %q", user)
			}
			// The fingerprint is free again for anyone now.
			if err := ks.AddSSHKey(ctx, SSHKey{Fingerprint: "SHA256:one", User: "bob", Type: "ssh-ed25519", Blob: "QUJDRA==", AddedAt: 9}); err != nil {
				t.Fatalf("re-add after delete: %v", err)
			}
		})
	}
}
