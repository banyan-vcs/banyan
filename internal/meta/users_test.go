package meta

// User store conformance (docs/design/data-model.md §18): the same battery
// on every backend. The load-bearing behaviors: upsert semantics (bootstrap
// and password change rewrite in place), lookup misses, name-sorted
// listing, and the admin flag riding along both ways. The hash is opaque
// here — auth owns the crypto.
import (
	"context"
	"errors"
	"testing"
)

func TestUserStoreConformance(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			ms, done := o(t)
			defer done()
			us, ok := ms.(UserStore)
			if !ok {
				t.Fatalf("backend %s does not implement UserStore", name)
			}
			ctx := context.Background()

			// --- put, lookup, list ---
			if err := us.PutUser(ctx, User{Name: "admin", Password: "h1", Admin: true, CreatedAt: 1}); err != nil {
				t.Fatalf("put admin: %v", err)
			}
			if err := us.PutUser(ctx, User{Name: "alice", Password: "h2", CreatedAt: 2}); err != nil {
				t.Fatalf("put alice: %v", err)
			}
			if _, err := us.UserByName(ctx, "nobody"); !errors.Is(err, ErrUserUnknown) {
				t.Fatalf("unknown name must bounce: %v", err)
			}
			admin, err := us.UserByName(ctx, "admin")
			if err != nil || admin.Name != "admin" || !admin.Admin || admin.Password != "h1" {
				t.Fatalf("admin row: %+v err=%v", admin, err)
			}
			all, err := us.Users(ctx)
			if err != nil || len(all) != 2 || all[0].Name != "admin" || all[1].Name != "alice" || all[1].Admin {
				t.Fatalf("listing: %+v err=%v", all, err)
			}

		// --- upsert: the password change rewrites in place, the flag sticks ---
		if err := us.PutUser(ctx, User{Name: "alice", Password: "h3", CreatedAt: 3}); err != nil {
			t.Fatalf("reseed alice: %v", err)
		}
		alice, err := us.UserByName(ctx, "alice")
		if err != nil || alice.Password != "h3" || alice.Admin {
			t.Fatalf("alice after reseed: %+v err=%v", alice, err)
		}
		if all, _ := us.Users(ctx); len(all) != 2 {
			t.Fatalf("upsert must not add rows: %d", len(all))
		}

		// --- the must-change flag rides the row both ways (admin reset
		// raises it; the user's own change rewrites the row without it) ---
		alice.MustChangePassword = true
		if err := us.PutUser(ctx, alice); err != nil {
			t.Fatalf("raise must-change: %v", err)
		}
		if alice, err = us.UserByName(ctx, "alice"); err != nil || !alice.MustChangePassword {
			t.Fatalf("must-change must stick: %+v err=%v", alice, err)
		}
		if err := us.PutUser(ctx, User{Name: "alice", Password: "h4", CreatedAt: 4}); err != nil {
			t.Fatalf("user change clears: %v", err)
		}
		if alice, err = us.UserByName(ctx, "alice"); err != nil || alice.MustChangePassword {
			t.Fatalf("a fresh rewrite carries no flag: %+v err=%v", alice, err)
		}

			// --- validation ---
			if err := us.PutUser(ctx, User{Name: "", Password: "h"}); !errors.Is(err, ErrUserInvalid) {
				t.Fatalf("empty name must bounce: %v", err)
			}
			if err := us.PutUser(ctx, User{Name: "a b", Password: "h"}); !errors.Is(err, ErrUserInvalid) {
				t.Fatalf("whitespace name must bounce: %v", err)
			}
			if err := us.PutUser(ctx, User{Name: "x"}); !errors.Is(err, ErrUserInvalid) {
				t.Fatalf("missing hash must bounce: %v", err)
			}
		})
	}
}
