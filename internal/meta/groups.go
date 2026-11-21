// Repository groups in the metadata plane (docs/design/data-model.md §15).
// A group is a pure organizational container — it owns no code, carries no
// permission semantics, and never changes a repo's physical name or its
// /api/v1/repos/<name> addressing; assignment is one optional label per
// repo (the parent-hierarchy precedent: organizational metadata only).
// GroupStore follows MRStore: deliberately NOT part of the MetaStore
// composite; consumers type-assert (meta.(GroupStore)) so remote-meta
// gateways compile unchanged.
package meta

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrGroupUnknown answers lookups and deletions of groups that do not exist.
var ErrGroupUnknown = errors.New("group unknown")

// ErrGroupExists marks a duplicate CreateGroup.
var ErrGroupExists = errors.New("group already exists")

// ErrGroupInvalid is the store-level validation refusal (bad name) — a
// client-input problem, so the JSON surface translates it to INVALID_ARG
// rather than INTERNAL (the MR/rule sentinel precedent).
var ErrGroupInvalid = errors.New("invalid group")

// GroupInfo is one group. Names follow the repo rules (single path
// segment); CreatedBy records the creator for display and the delete gate.
type GroupInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedBy   string `json:"createdBy"`
	CreatedAt   int64  `json:"createdAt"`
}

// Valid applies the name rules shared with repositories: one path segment,
// no whitespace, not "-" (keeps group rows visually distinct from flags).
func (g GroupInfo) Valid() error {
	if g.Name == "" || strings.Contains(g.Name, "/") || strings.ContainsAny(g.Name, " \t\n") || g.Name == "-" {
		return fmt.Errorf("%w: name required, single path segment", ErrGroupInvalid)
	}
	return nil
}

// GroupStore manages groups and the repo→group assignment table.
type GroupStore interface {
	// CreateGroup stores a new group (ErrGroupExists on a duplicate,
	// ErrGroupInvalid on a bad name).
	CreateGroup(ctx context.Context, g GroupInfo) error
	// DeleteGroup drops a group and clears its assignments (missing = no-op
	// via ErrGroupUnknown so callers can distinguish).
	DeleteGroup(ctx context.Context, name string) error
	// ListGroups answers all groups, name-sorted.
	ListGroups(ctx context.Context) ([]GroupInfo, error)
	// SetRepoGroup assigns a repo to a group; group "" clears the label.
	// The group must exist (ErrGroupUnknown); the repo's existence is the
	// caller's check (the rpc layer GETs it first — the store cannot see
	// repos it does not own).
	SetRepoGroup(ctx context.Context, repo, group string) error
	// RepoGroups answers the assignment table, repo → group name.
	RepoGroups(ctx context.Context) (map[string]string, error)
}

// --- memory implementation ---

func (m *MemoryMetaStore) CreateGroup(_ context.Context, g GroupInfo) error {
	if err := g.Valid(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[g.Name]; ok {
		return fmt.Errorf("%w: %s", ErrGroupExists, g.Name)
	}
	m.groups[g.Name] = &g
	return nil
}

func (m *MemoryMetaStore) DeleteGroup(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[name]; !ok {
		return fmt.Errorf("%w: %s", ErrGroupUnknown, name)
	}
	delete(m.groups, name)
	for repo, g := range m.repoGroups {
		if g == name {
			delete(m.repoGroups, repo)
		}
	}
	return nil
}

func (m *MemoryMetaStore) ListGroups(_ context.Context) ([]GroupInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]GroupInfo, 0, len(m.groups))
	for _, g := range m.groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemoryMetaStore) SetRepoGroup(_ context.Context, repo, group string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if group != "" {
		if _, ok := m.groups[group]; !ok {
			return fmt.Errorf("%w: %s", ErrGroupUnknown, group)
		}
		m.repoGroups[repo] = group
		return nil
	}
	delete(m.repoGroups, repo)
	return nil
}

func (m *MemoryMetaStore) RepoGroups(_ context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.repoGroups))
	for repo, g := range m.repoGroups {
		out[repo] = g
	}
	return out, nil
}

// --- SQL (GORM) implementation ---

func (s *SQLMetaStore) CreateGroup(ctx context.Context, g GroupInfo) error {
	if err := g.Valid(); err != nil {
		return err
	}
	err := s.orm.WithContext(ctx).Create(&groupRow{GroupInfo: g}).Error
	// TranslateError maps every dialect's unique violation to
	// ErrDuplicatedKey (the mrAllocationRetryable precedent).
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("%w: %s", ErrGroupExists, g.Name)
	}
	return err
}

func (s *SQLMetaStore) DeleteGroup(ctx context.Context, name string) error {
	res := s.orm.WithContext(ctx).Where("name = ?", name).Delete(&groupRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", ErrGroupUnknown, name)
	}
	// Assignments of a deleted group clear in the same breath — a repo
	// never points at a missing group.
	return s.orm.WithContext(ctx).Where("group_name = ?", name).Delete(&repoGroupRow{}).Error
}

func (s *SQLMetaStore) ListGroups(ctx context.Context) ([]GroupInfo, error) {
	var rows []groupRow
	if err := s.orm.WithContext(ctx).Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]GroupInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GroupInfo)
	}
	return out, nil
}

func (s *SQLMetaStore) SetRepoGroup(ctx context.Context, repo, group string) error {
	if group == "" {
		return s.orm.WithContext(ctx).Where("repo = ?", repo).Delete(&repoGroupRow{}).Error
	}
	var one groupRow
	if err := s.orm.WithContext(ctx).Where("name = ?", group).Limit(1).Find(&one).Error; err != nil {
		return err
	}
	if one.Name == "" {
		return fmt.Errorf("%w: %s", ErrGroupUnknown, group)
	}
	return s.orm.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&repoGroupRow{Repo: repo, GroupName: group}).Error
}

func (s *SQLMetaStore) RepoGroups(ctx context.Context) (map[string]string, error) {
	var rows []repoGroupRow
	if err := s.orm.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Repo] = r.GroupName
	}
	return out, nil
}
