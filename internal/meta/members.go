// Group and repository members in the metadata plane (docs/design/data-model.md
// §16): the permission ledger behind the console's member management. A
// repository's effective membership is its direct member table plus, when the
// repo carries a group label, the group's member table — inherited rows keep
// their source so the UI can mark them; the effective rank is the MAX of the
// two, and a direct grant must rank strictly ABOVE the inherited one (the
// repository level adds privilege, it never duplicates or narrows the group's
// grant). MemberStore follows GroupStore: deliberately NOT part of the
// MetaStore composite; consumers type-assert so remote-meta gateways compile
// unchanged.
package meta

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm/clause"
)

// The member ranks, weakest to strongest. The names align with the
// authorization doc's vocabulary (viewer reads, developer pushes and merges,
// owner manages the member tables themselves — deployment admins still
// outrank everything through the policy layer).
const (
	RankViewer    = "viewer"
	RankDeveloper = "developer"
	RankOwner     = "owner"
)

// RankVal answers the rank's ordering; "" (not a member) and unknown ranks
// sort below viewer, so the policy gate never mistakes a stranger for a
// viewer.
func RankVal(rank string) int {
	switch rank {
	case RankOwner:
		return 2
	case RankDeveloper:
		return 1
	case RankViewer:
		return 0
	default:
		return -1
	}
}

// ValidRank answers whether the string is a rank at all.
func ValidRank(rank string) bool {
	return rank == RankViewer || rank == RankDeveloper || rank == RankOwner
}

// ErrMemberInvalid is the store-level validation refusal (empty user, bad
// rank) — client input, so the JSON surface answers INVALID_ARG (the group
// precedent).
var ErrMemberInvalid = errors.New("invalid member")

// ErrMemberUnknown answers removals of members that do not exist.
var ErrMemberUnknown = errors.New("member unknown")

// ErrMemberRank is the inheritance guard: a direct repository grant must rank
// strictly above what the user inherits from the repo's group — equal is
// redundant, lower is a silent narrowing, both bounce.
var ErrMemberRank = errors.New("rank not above the inherited grant")

// Member is one row of either member table. The columns spell their names
// out (username, member_rank) — USER is reserved on PostgreSQL and RANK on
// MySQL 8+ (the GROUPS pitfall's siblings).
type Member struct {
	User    string `json:"user" gorm:"column:username;primaryKey"`
	Rank    string `json:"rank" gorm:"column:member_rank;not null"`
	AddedBy string `json:"addedBy" gorm:"column:added_by;not null"`
	AddedAt int64  `json:"addedAt" gorm:"column:added_at;not null"`
}

// EffectiveMember is one row of a repository's resolved membership:
// ViaGroup != "" marks the row as inherited from that group (direct rows
// answer "").
type EffectiveMember struct {
	Member
	ViaGroup string `json:"viaGroup"`
}

// MemberStore manages the two member tables and the resolution over them.
// Existence checks (the group, the repo) follow the SetRepoGroup precedent:
// the group must exist here, the repo is the rpc layer's check.
type MemberStore interface {
	// SetGroupMember stores one group member (rank "" removes — the group
	// must exist, ErrMemberUnknown when removing a name that is not a
	// member).
	SetGroupMember(ctx context.Context, group string, m Member) error
	// GroupMembers answers the group's member table, user-sorted.
	GroupMembers(ctx context.Context, group string) ([]Member, error)
	// SetRepoMember stores one direct repository member (rank "" removes).
	// The grant must rank strictly above the user's inherited rank from the
	// repo's group, when any (ErrMemberRank).
	SetRepoMember(ctx context.Context, repo string, m Member) error
	// RepoMembers answers the direct member table only, user-sorted.
	RepoMembers(ctx context.Context, repo string) ([]Member, error)
	// RepoEffectiveMembers answers direct plus inherited rows (inherited
	// carry ViaGroup), user-sorted.
	RepoEffectiveMembers(ctx context.Context, repo string) ([]EffectiveMember, error)
	// MemberRank resolves one user's effective rank on the repo.
	// managed reports whether the repo is member-governed at all — a direct
	// table row OR an inherited table via its group label; unmanaged repos
	// answer ("", false) so the policy layer can pass them through unchanged.
	MemberRank(ctx context.Context, user, repo string) (rank string, managed bool, err error)
}

func memberValid(m Member, removing bool) error {
	if m.User == "" || strings.ContainsAny(m.User, " \t\n") {
		return fmt.Errorf("%w: user required", ErrMemberInvalid)
	}
	if removing {
		return nil
	}
	if !ValidRank(m.Rank) {
		return fmt.Errorf("%w: rank must be viewer|developer|owner", ErrMemberInvalid)
	}
	return nil
}

// --- memory implementation ---

func (m *MemoryMetaStore) SetGroupMember(_ context.Context, group string, mem Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[group]; !ok {
		return fmt.Errorf("%w: %s", ErrGroupUnknown, group)
	}
	if err := memberValid(mem, mem.Rank == ""); err != nil {
		return err
	}
	if mem.Rank == "" {
		if _, ok := m.groupMembers[group][mem.User]; !ok {
			return fmt.Errorf("%w: %s/%s", ErrMemberUnknown, group, mem.User)
		}
		delete(m.groupMembers[group], mem.User)
		return nil
	}
	if m.groupMembers[group] == nil {
		m.groupMembers[group] = map[string]*Member{}
	}
	c := mem
	m.groupMembers[group][mem.User] = &c
	return nil
}

func (m *MemoryMetaStore) GroupMembers(_ context.Context, group string) ([]Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Member, 0, len(m.groupMembers[group]))
	for _, mem := range m.groupMembers[group] {
		out = append(out, *mem)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}

func (m *MemoryMetaStore) inheritedRank(repo, user string) (string, bool) {
	group, ok := m.repoGroups[repo]
	if !ok {
		return "", false
	}
	if mem, ok := m.groupMembers[group][user]; ok {
		return mem.Rank, true
	}
	return "", false
}

func (m *MemoryMetaStore) SetRepoMember(_ context.Context, repo string, mem Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := memberValid(mem, mem.Rank == ""); err != nil {
		return err
	}
	if inh, ok := m.inheritedRank(repo, mem.User); ok && mem.Rank != "" && RankVal(mem.Rank) <= RankVal(inh) {
		return fmt.Errorf("%w: %s inherits %s, direct grant wants strictly above", ErrMemberRank, mem.User, inh)
	}
	if mem.Rank == "" {
		if _, ok := m.repoMembers[repo][mem.User]; !ok {
			return fmt.Errorf("%w: %s/%s", ErrMemberUnknown, repo, mem.User)
		}
		delete(m.repoMembers[repo], mem.User)
		return nil
	}
	if m.repoMembers[repo] == nil {
		m.repoMembers[repo] = map[string]*Member{}
	}
	c := mem
	m.repoMembers[repo][mem.User] = &c
	return nil
}

func (m *MemoryMetaStore) RepoMembers(_ context.Context, repo string) ([]Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Member, 0, len(m.repoMembers[repo]))
	for _, mem := range m.repoMembers[repo] {
		out = append(out, *mem)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}

func (m *MemoryMetaStore) RepoEffectiveMembers(_ context.Context, repo string) ([]EffectiveMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]EffectiveMember, 0, len(m.repoMembers[repo])+len(m.groupMembers[m.repoGroups[repo]]))
	for _, mem := range m.repoMembers[repo] {
		out = append(out, EffectiveMember{Member: *mem})
	}
	if group, ok := m.repoGroups[repo]; ok {
		for _, mem := range m.groupMembers[group] {
			if _, direct := m.repoMembers[repo][mem.User]; direct {
				continue // the direct row already outranks by construction
			}
			out = append(out, EffectiveMember{Member: *mem, ViaGroup: group})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}

func (m *MemoryMetaStore) MemberRank(_ context.Context, user, repo string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	group, labeled := m.repoGroups[repo]
	managed := len(m.repoMembers[repo]) > 0 || (labeled && len(m.groupMembers[group]) > 0)
	if !managed {
		return "", false, nil
	}
	rank := ""
	if mem, ok := m.repoMembers[repo][user]; ok {
		rank = mem.Rank
	}
	if inh, ok := m.inheritedRank(repo, user); ok && RankVal(inh) > RankVal(rank) {
		rank = inh
	}
	return rank, true, nil
}

// --- SQL (GORM) implementation ---

func (s *SQLMetaStore) SetGroupMember(ctx context.Context, group string, mem Member) error {
	var one groupRow
	if err := s.orm.WithContext(ctx).Where("name = ?", group).Limit(1).Find(&one).Error; err != nil {
		return err
	}
	if one.Name == "" {
		return fmt.Errorf("%w: %s", ErrGroupUnknown, group)
	}
	if err := memberValid(mem, mem.Rank == ""); err != nil {
		return err
	}
	if mem.Rank == "" {
		res := s.orm.WithContext(ctx).Where("group_name = ? AND username = ?", group, mem.User).Delete(&groupMemberRow{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: %s/%s", ErrMemberUnknown, group, mem.User)
		}
		return nil
	}
	return s.orm.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&groupMemberRow{GroupName: group, Member: mem}).Error
}

func (s *SQLMetaStore) GroupMembers(ctx context.Context, group string) ([]Member, error) {
	var rows []groupMemberRow
	if err := s.orm.WithContext(ctx).Where("group_name = ?", group).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Member)
	}
	return out, nil
}

// repoGroupOf answers the repo's group label, "" when unlabeled.
func (s *SQLMetaStore) repoGroupOf(ctx context.Context, repo string) (string, error) {
	var one repoGroupRow
	if err := s.orm.WithContext(ctx).Where("repo = ?", repo).Limit(1).Find(&one).Error; err != nil {
		return "", err
	}
	return one.GroupName, nil
}

func (s *SQLMetaStore) SetRepoMember(ctx context.Context, repo string, mem Member) error {
	if err := memberValid(mem, mem.Rank == ""); err != nil {
		return err
	}
	group, err := s.repoGroupOf(ctx, repo)
	if err != nil {
		return err
	}
	if group != "" && mem.Rank != "" {
		var inh groupMemberRow
		if err := s.orm.WithContext(ctx).Where("group_name = ? AND username = ?", group, mem.User).Limit(1).Find(&inh).Error; err != nil {
			return err
		}
		if inh.User != "" && RankVal(mem.Rank) <= RankVal(inh.Rank) {
			return fmt.Errorf("%w: %s inherits %s, direct grant wants strictly above", ErrMemberRank, mem.User, inh.Rank)
		}
	}
	if mem.Rank == "" {
		res := s.orm.WithContext(ctx).Where("repo = ? AND username = ?", repo, mem.User).Delete(&repoMemberRow{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: %s/%s", ErrMemberUnknown, repo, mem.User)
		}
		return nil
	}
	return s.orm.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&repoMemberRow{Repo: repo, Member: mem}).Error
}

func (s *SQLMetaStore) RepoMembers(ctx context.Context, repo string) ([]Member, error) {
	var rows []repoMemberRow
	if err := s.orm.WithContext(ctx).Where("repo = ?", repo).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Member)
	}
	return out, nil
}

func (s *SQLMetaStore) RepoEffectiveMembers(ctx context.Context, repo string) ([]EffectiveMember, error) {
	direct, err := s.RepoMembers(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := make([]EffectiveMember, 0, len(direct))
	seen := make(map[string]bool, len(direct))
	for _, mem := range direct {
		out = append(out, EffectiveMember{Member: mem})
		seen[mem.User] = true
	}
	group, err := s.repoGroupOf(ctx, repo)
	if err != nil {
		return nil, err
	}
	if group != "" {
		inherited, err := s.GroupMembers(ctx, group)
		if err != nil {
			return nil, err
		}
		for _, mem := range inherited {
			if seen[mem.User] {
				continue
			}
			out = append(out, EffectiveMember{Member: mem, ViaGroup: group})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}

func (s *SQLMetaStore) MemberRank(ctx context.Context, user, repo string) (string, bool, error) {
	var directCount int64
	if err := s.orm.WithContext(ctx).Model(&repoMemberRow{}).Where("repo = ?", repo).Count(&directCount).Error; err != nil {
		return "", false, err
	}
	group, err := s.repoGroupOf(ctx, repo)
	if err != nil {
		return "", false, err
	}
	var inheritedCount int64
	if group != "" {
		if err := s.orm.WithContext(ctx).Model(&groupMemberRow{}).Where("group_name = ?", group).Count(&inheritedCount).Error; err != nil {
			return "", false, err
		}
	}
	if directCount == 0 && inheritedCount == 0 {
		return "", false, nil // unmanaged — the policy layer passes through
	}
	var rank string
	var one repoMemberRow
	if err := s.orm.WithContext(ctx).Where("repo = ? AND username = ?", repo, user).Limit(1).Find(&one).Error; err != nil {
		return "", true, err
	}
	rank = one.Rank
	if group != "" {
		var inh groupMemberRow
		if err := s.orm.WithContext(ctx).Where("group_name = ? AND username = ?", group, user).Limit(1).Find(&inh).Error; err != nil {
			return "", true, err
		}
		if RankVal(inh.Rank) > RankVal(rank) {
			rank = inh.Rank
		}
	}
	return rank, true, nil
}

// deleteGroupMembers clears the group's member table — DeleteGroup's
// same-breath hygiene (the assignment-table precedent).
func (s *SQLMetaStore) deleteGroupMembers(ctx context.Context, group string) error {
	return s.orm.WithContext(ctx).Where("group_name = ?", group).Delete(&groupMemberRow{}).Error
}
