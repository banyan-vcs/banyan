// User-managed SSH public keys in the metadata plane (docs/design/data-model.md
// §17): the console's key table behind the git-over-SSH transport. One key
// belongs to one account (the fingerprint is the primary key, so a key can
// never vouch for two users); identification is by wire-format key blob —
// exactly what auth.Service.IdentifyKey receives from the SSH handshake.
// SSHKeyStore follows MemberStore: deliberately NOT part of the MetaStore
// composite; consumers type-assert so remote-meta gateways compile unchanged.
package meta

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
)

// ErrSSHKeyInvalid is the store-level validation refusal (empty
// fingerprint, user, or blob) — client input, so the JSON surface answers
// INVALID_ARG (the member precedent).
var ErrSSHKeyInvalid = errors.New("invalid ssh key")

// ErrSSHKeyExists answers an add whose fingerprint is already registered —
// one key, one account; the second registration bounces whoever asks.
var ErrSSHKeyExists = errors.New("ssh key already registered")

// ErrSSHKeyUnknown answers deletions of keys that are not there (wrong
// fingerprint, or someone else's key — the delete path is owner-scoped).
var ErrSSHKeyUnknown = errors.New("ssh key unknown")

// SSHKey is one row of the key table. Blob carries the base64 wire-format
// key (what an authorized_keys line spells after the algorithm) and is
// never echoed on the JSON face — the fingerprint names the key there.
type SSHKey struct {
	Fingerprint string `json:"fingerprint" gorm:"column:fingerprint;primaryKey"`
	User        string `json:"user" gorm:"column:username;not null"`
	Type        string `json:"type" gorm:"column:key_type;not null"`
	Blob        string `json:"-" gorm:"column:key_blob;not null"`
	Comment     string `json:"comment" gorm:"column:key_comment;not null"`
	AddedAt     int64  `json:"addedAt" gorm:"column:added_at;not null"`
}

// SSHKeyStore manages the user key table. Validation of the key material
// itself (parsing, fingerprinting) is the caller's job — the rpc layer
// parses authorized_keys lines; the store only keeps what it is given.
type SSHKeyStore interface {
	// AddSSHKey stores one key (ErrSSHKeyExists when the fingerprint is
	// taken — by anyone: one key, one account).
	AddSSHKey(ctx context.Context, key SSHKey) error
	// SSHKeys answers the user's keys, newest first.
	SSHKeys(ctx context.Context, user string) ([]SSHKey, error)
	// DeleteSSHKey removes one of the user's keys by fingerprint
	// (ErrSSHKeyUnknown when it is not theirs).
	DeleteSSHKey(ctx context.Context, user, fingerprint string) error
	// IdentifySSHKey answers the account a wire-format key blob belongs
	// to; "" when the key is unknown. The SSH transport's handshake lookup.
	IdentifySSHKey(ctx context.Context, keyBlob []byte) (string, error)
}

// --- memory implementation ---

func (m *MemoryMetaStore) AddSSHKey(_ context.Context, key SSHKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key.Fingerprint == "" || key.User == "" || key.Blob == "" {
		return fmt.Errorf("%w: fingerprint, user and blob required", ErrSSHKeyInvalid)
	}
	if _, taken := m.sshKeys[key.Fingerprint]; taken {
		return fmt.Errorf("%w: %s", ErrSSHKeyExists, key.Fingerprint)
	}
	c := key
	m.sshKeys[key.Fingerprint] = &c
	return nil
}

func (m *MemoryMetaStore) SSHKeys(_ context.Context, user string) ([]SSHKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []SSHKey{}
	for _, k := range m.sshKeys {
		if k.User == user {
			out = append(out, *k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AddedAt != out[j].AddedAt {
			return out[i].AddedAt > out[j].AddedAt
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out, nil
}

func (m *MemoryMetaStore) DeleteSSHKey(_ context.Context, user, fingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.sshKeys[fingerprint]; ok && k.User == user {
		delete(m.sshKeys, fingerprint)
		return nil
	}
	return fmt.Errorf("%w: %s/%s", ErrSSHKeyUnknown, user, fingerprint)
}

func (m *MemoryMetaStore) IdentifySSHKey(_ context.Context, keyBlob []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	blob := base64.StdEncoding.EncodeToString(keyBlob)
	for _, k := range m.sshKeys {
		if k.Blob == blob {
			return k.User, nil
		}
	}
	return "", nil
}

// --- SQL (GORM) implementation ---

func (s *SQLMetaStore) AddSSHKey(ctx context.Context, key SSHKey) error {
	if key.Fingerprint == "" || key.User == "" || key.Blob == "" {
		return fmt.Errorf("%w: fingerprint, user and blob required", ErrSSHKeyInvalid)
	}
	var one sshKeyRow
	if err := s.orm.WithContext(ctx).Where("fingerprint = ?", key.Fingerprint).Limit(1).Find(&one).Error; err != nil {
		return err
	}
	if one.Fingerprint != "" {
		return fmt.Errorf("%w: %s", ErrSSHKeyExists, key.Fingerprint)
	}
	return s.orm.WithContext(ctx).Create(&sshKeyRow{SSHKey: key}).Error
}

func (s *SQLMetaStore) SSHKeys(ctx context.Context, user string) ([]SSHKey, error) {
	var rows []sshKeyRow
	if err := s.orm.WithContext(ctx).Where("username = ?", user).Order("added_at DESC, fingerprint").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]SSHKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SSHKey)
	}
	return out, nil
}

func (s *SQLMetaStore) DeleteSSHKey(ctx context.Context, user, fingerprint string) error {
	res := s.orm.WithContext(ctx).Where("username = ? AND fingerprint = ?", user, fingerprint).Delete(&sshKeyRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %s/%s", ErrSSHKeyUnknown, user, fingerprint)
	}
	return nil
}

func (s *SQLMetaStore) IdentifySSHKey(ctx context.Context, keyBlob []byte) (string, error) {
	var one sshKeyRow
	if err := s.orm.WithContext(ctx).Where("key_blob = ?", base64.StdEncoding.EncodeToString(keyBlob)).Limit(1).Find(&one).Error; err != nil {
		return "", err
	}
	return one.User, nil
}
