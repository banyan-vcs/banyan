package main

// First-boot account setup (docs/gateway/authorization.md §2a): a fresh
// deployment with no configured users gets an administrator with a
// generated password, printed once to the terminal — the hash is what
// persists, so the operator must note it (or change it right after the
// first login, which is the printed advice). Demo mode skips this: its
// seeding owns the account table (internal/demoseed).
import (
	"context"
	"log"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
)

// bootstrapAdmin creates the initial administrator when the deployment has
// no users at all — neither the account table nor the config file. The
// generated password goes to the log (the terminal) exactly once.
func bootstrapAdminAccount(ctx context.Context, ms meta.MetaStore) {
	us, ok := ms.(meta.UserStore)
	if !ok {
		return
	}
	users, err := us.Users(ctx)
	if err != nil {
		log.Printf("auth: account table unreadable, skipping administrator bootstrap: %v", err)
		return
	}
	if len(users) > 0 {
		return
	}
	pw := auth.RandomPassword()
	hash, err := auth.HashPassword(pw)
	if err != nil {
		log.Printf("auth: cannot hash the bootstrap password: %v", err)
		return
	}
	if err := us.PutUser(ctx, meta.User{Name: "admin", Password: hash, Admin: true, CreatedAt: time.Now().Unix()}); err != nil {
		log.Printf("auth: administrator bootstrap failed: %v", err)
		return
	}
	log.Printf("auth: initialized administrator 'admin' — generated password: %s", pw)
	log.Printf("auth: note it now (only the hash is stored) and change it from the web console's Settings after first login")
}
