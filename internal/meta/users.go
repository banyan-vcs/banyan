// The account table in the metadata plane (docs/design/data-model.md §18):
// server-managed users with bcrypt password hashes — the backing for the
// console's password self-service and banyand's first-boot administrator.
// The hash is opaque to the store (auth owns the crypto); one flag rides
// along — is_admin — so the policy layer can answer management rank without
// a second table. UserStore follows MemberStore: deliberately NOT part of
// the MetaStore composite; consumers type-assert so remote-meta gateways
// compile unchanged.
package meta

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm/clause"
)

// ErrUserUnknown answers lookups of names that are not in the table.
var ErrUserUnknown = errors.New("user unknown")

// ErrUserInvalid is the validation refusal (empty name, whitespace).
var ErrUserInvalid = errors.New("invalid user")

// User is one row of the account table. PasswordHash carries a bcrypt
// hash and never crosses the JSON face (json:"-"). MustChangePassword is
// raised by an administrator's reset (the random password must be traded
// for a chosen one at first login) and cleared by the user's own change.
type User struct {
	Name               string `json:"name" gorm:"column:username;primaryKey"`
	Password           string `json:"-" gorm:"column:password_hash;not null"`
	Admin              bool   `json:"admin" gorm:"column:is_admin;not null"`
	MustChangePassword bool   `json:"mustChangePassword" gorm:"column:must_change_password;not null"`
	AvatarURL          string `json:"avatarUrl,omitempty" gorm:"column:avatar_url;not null;default:''"` // data: or https: URL; empty = name-initial default
	CreatedAt          int64  `json:"createdAt" gorm:"column:created_at;not null"`
}

// UserStore manages the account table. Creating or re-seeding an account
// is an upsert (bootstrap, password change); there is no delete — the
// table's writers are bootstrap and self-service, not user administration.
type UserStore interface {
	// PutUser stores one account (upsert by name).
	PutUser(ctx context.Context, u User) error
	// UserByName answers one account (ErrUserUnknown).
	UserByName(ctx context.Context, name string) (User, error)
	// Users answers every account, name-sorted.
	Users(ctx context.Context) ([]User, error)
}

// MaxAvatarURL bounds the avatar string: the MySQL column is a sized
// VARCHAR (TEXT cannot DEFAULT there), so the cap is one number every
// backend shares — 4000 chars of data: URL is a crisp 64px round-trip.
const MaxAvatarURL = 4000

func userValid(u User) error {
	if u.Name == "" || len(u.Name) > 64 || strings.ContainsAny(u.Name, " \t\n") {
		return fmt.Errorf("%w: name required, no whitespace, at most 64 bytes", ErrUserInvalid)
	}
	if u.Password == "" {
		return fmt.Errorf("%w: password hash required", ErrUserInvalid)
	}
	if len(u.AvatarURL) > MaxAvatarURL {
		return fmt.Errorf("%w: avatar too large (max %d chars)", ErrUserInvalid, MaxAvatarURL)
	}
	if u.AvatarURL != "" && !strings.HasPrefix(u.AvatarURL, "data:image/") && !strings.HasPrefix(u.AvatarURL, "https://") {
		return fmt.Errorf("%w: avatar must be a data:image/ or https:// URL", ErrUserInvalid)
	}
	return nil
}

// --- memory implementation ---

func (m *MemoryMetaStore) PutUser(_ context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := userValid(u); err != nil {
		return err
	}
	c := u
	m.users[u.Name] = &c
	return nil
}

func (m *MemoryMetaStore) UserByName(_ context.Context, name string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[name]; ok {
		return *u, nil
	}
	return User{}, fmt.Errorf("%w: %s", ErrUserUnknown, name)
}

func (m *MemoryMetaStore) Users(_ context.Context) ([]User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// --- SQL (GORM) implementation ---

func (s *SQLMetaStore) PutUser(ctx context.Context, u User) error {
	if err := userValid(u); err != nil {
		return err
	}
	row := userRow{Name: u.Name, Password: u.Password, AvatarURL: u.AvatarURL, CreatedAt: u.CreatedAt}
	if u.Admin {
		row.Admin = 1
	}
	if u.MustChangePassword {
		row.MustChange = 1
	}
	return s.orm.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func (s *SQLMetaStore) UserByName(ctx context.Context, name string) (User, error) {
	var one userRow
	if err := s.orm.WithContext(ctx).Where("username = ?", name).Limit(1).Find(&one).Error; err != nil {
		return User{}, err
	}
	if one.Name == "" {
		return User{}, fmt.Errorf("%w: %s", ErrUserUnknown, name)
	}
	return one.user(), nil
}

func (s *SQLMetaStore) Users(ctx context.Context) ([]User, error) {
	var rows []userRow
	if err := s.orm.WithContext(ctx).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]User, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.user())
	}
	return out, nil
}

func (r userRow) user() User {
	return User{Name: r.Name, Password: r.Password, Admin: r.Admin != 0, MustChangePassword: r.MustChange != 0, AvatarURL: r.AvatarURL, CreatedAt: r.CreatedAt}
}
