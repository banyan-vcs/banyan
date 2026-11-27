// Merge requests and branch protection rules in the metadata plane
// (docs/design/merge-requests.md). The MR ledger and the branch rules are
// cluster state: any gateway with the metadata answers, and the rule reader
// (receive-pack, the JSON ref API) sits in the same process as the writer —
// no snapshot sync, no cache to kick. MRStore and BranchRuleStore follow
// TokenRegistry: deliberately NOT part of the MetaStore composite; consumers
// type-assert (meta.(MRStore)) so remote-meta gateways compile unchanged.
package meta

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gmysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MR states: open → merged (terminal, the merge commit is recorded) or
// open → closed → open (reopen; the IID is never reused either way).
const (
	MRStateOpen   = "open"
	MRStateMerged = "merged"
	MRStateClosed = "closed"
)

// Branch protection levels. "protected" keeps the existing snapshot-policy
// semantics (direct push needs owner rank); "mr-only" is the new strict
// level: NOBODY pushes the branch directly — not even admins — the merge
// request path is the only way in, gated by the rule's approval count.
const (
	RuleProtected = "protected"
	RuleMROnly    = "mr-only"
)

// ErrMRUnknown answers lookups of merge requests that do not exist.
var ErrMRUnknown = errors.New("merge request unknown")

// ErrMRState is the optimistic-guard failure: the transition expected a
// different state (a concurrent merge/close got there first).
var ErrMRState = errors.New("merge request state changed")

// ErrMRInvalid and ErrBranchRuleInvalid mark store-level validation
// refusals — client-input problems, not server faults, so the JSON surface
// translates them to INVALID_ARG rather than INTERNAL.
var (
	ErrMRInvalid         = errors.New("invalid merge request")
	ErrBranchRuleInvalid = errors.New("invalid branch rule")
)

// MRInfo is one merge request. Identity is (Repo, IID) — the IID is
// allocated per repository, monotonic, and never reused, so merged and
// closed requests keep shareable URLs forever.
type MRInfo struct {
	Repo        string `json:"repo"`
	IID         int64  `json:"iid" gorm:"column:iid"` // IID would snake-case to "i_id" (the registered trap)
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`           // source branch name
	Target      string `json:"target"`           // target branch name
	State       string `json:"state"`            // open | merged | closed
	Author      string `json:"author"`           // opening identity (id.User)
	SourceHead  string `json:"source_head"`      // source tip at open time
	MergeCommit string `json:"merge_commit"`     // set when merged
	MergedBy    string `json:"merged_by"`        // merging identity, set when merged
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

func (m MRInfo) Valid() error {
	if m.Repo == "" || m.Author == "" {
		return fmt.Errorf("%w: repo and author required", ErrMRInvalid)
	}
	if m.Source == "" || m.Target == "" || m.Source == m.Target {
		return fmt.Errorf("%w: distinct source and target branches required", ErrMRInvalid)
	}
	if m.Title == "" {
		return fmt.Errorf("%w: title required", ErrMRInvalid)
	}
	if m.State != MRStateOpen && m.State != MRStateMerged && m.State != MRStateClosed {
		return fmt.Errorf("%w: state must be open|merged|closed", ErrMRInvalid)
	}
	return nil
}

// MRStore manages the merge-request ledger. All methods are safe for
// concurrent use; CreateMR allocates the per-repo IID under the store's
// serialization (retrying the rare unique-key race), and TransitionMR is
// the optimistic state gate two racing merges cannot both pass.
type MRStore interface {
	// CreateMR stores a new open MR, filling IID. The caller sets
	// timestamps; the store validates and serializes.
	CreateMR(ctx context.Context, m MRInfo) (MRInfo, error)
	// MRByIID answers one MR (ErrMRUnknown otherwise).
	MRByIID(ctx context.Context, repo string, iid int64) (MRInfo, error)
	// ListMRs answers the repo's MRs, newest IID first; state "" = all.
	ListMRs(ctx context.Context, repo, state string) ([]MRInfo, error)
	// TransitionMR applies patch under an optimistic guard: the update
	// lands only when the row's current state is `from` (ErrMRState
	// otherwise; ErrMRUnknown for a missing row). patch mutates the stored
	// copy — state, merge_commit, merged_by, timestamps — before the write.
	TransitionMR(ctx context.Context, repo string, iid int64, from string, patch func(*MRInfo)) (MRInfo, error)
	// AddApproval records one approval (idempotent upsert).
	AddApproval(ctx context.Context, repo string, iid int64, user string, at int64) error
	// RemoveApproval drops one approval (idempotent).
	RemoveApproval(ctx context.Context, repo string, iid int64, user string) error
	// ListApprovals answers the MR's approvers, sorted by name.
	ListApprovals(ctx context.Context, repo string, iid int64) ([]string, error)
	// ListApprovalDetails answers the approvers WITH their timestamps —
	// the discussion timeline's event rows (approval is an event, not just
	// a count), sorted by name.
	ListApprovalDetails(ctx context.Context, repo string, iid int64) ([]MRApproval, error)
}

// MRApproval is one recorded approval with its timestamp (the timeline's
// event shape; ListApprovals stays the count-only view the merge gate reads).
type MRApproval struct {
	User string `json:"user"`
	At   int64  `json:"at"`
}

// BranchRule is one branch's protection entry. Level RuleProtected mirrors
// what the auth snapshot already enforces (kept here so the rule list is
// the one surface operators read); RuleMROnly is enforced by the gateway's
// push gates themselves. Approvals is the required count for merging an MR
// into the branch (0..9; 1 when the level is mr-only).
type BranchRule struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Level     string `json:"level"` // protected | mr-only
	Approvals int    `json:"approvals"`
}

func (r BranchRule) Valid() error {
	if r.Repo == "" || r.Branch == "" {
		return fmt.Errorf("%w: repo and branch required", ErrBranchRuleInvalid)
	}
	if strings.ContainsAny(r.Branch, " \t\n") || strings.HasPrefix(r.Branch, "-") {
		return fmt.Errorf("%w: malformed branch name", ErrBranchRuleInvalid)
	}
	switch r.Level {
	case RuleProtected, RuleMROnly:
	default:
		return fmt.Errorf("%w: level must be protected|mr-only", ErrBranchRuleInvalid)
	}
	if r.Approvals < 0 || r.Approvals > 9 {
		return fmt.Errorf("%w: approvals must be 0..9", ErrBranchRuleInvalid)
	}
	if r.Level == RuleMROnly && r.Approvals == 0 {
		return fmt.Errorf("%w: mr-only needs at least one approval", ErrBranchRuleInvalid)
	}
	return nil
}

// BranchRuleStore manages per-branch protection rules.
type BranchRuleStore interface {
	// PutBranchRule upserts by (repo, branch).
	PutBranchRule(ctx context.Context, r BranchRule) error
	// BranchRules answers the repo's rules, branch-sorted.
	BranchRules(ctx context.Context, repo string) ([]BranchRule, error)
	// BranchRuleOf is the push gate's hot path: one rule, (BranchRule{},
	// false) when none.
	BranchRuleOf(ctx context.Context, repo, branch string) (BranchRule, bool, error)
	// DeleteBranchRule removes one rule (missing = no-op).
	DeleteBranchRule(ctx context.Context, repo, branch string) error
}

// --- memory implementation ---

func (m *MemoryMetaStore) CreateMR(_ context.Context, in MRInfo) (MRInfo, error) {
	if err := in.Valid(); err != nil {
		return MRInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mrs[in.Repo] == nil {
		m.mrs[in.Repo] = map[int64]*MRInfo{}
	}
	m.mrNextIID[in.Repo]++
	in.IID = m.mrNextIID[in.Repo]
	m.mrs[in.Repo][in.IID] = &in
	return in, nil
}

func (m *MemoryMetaStore) MRByIID(_ context.Context, repo string, iid int64) (MRInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mr, ok := m.mrs[repo][iid]
	if !ok {
		return MRInfo{}, fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
	}
	return *mr, nil
}

func (m *MemoryMetaStore) ListMRs(_ context.Context, repo, state string) ([]MRInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MRInfo, 0, len(m.mrs[repo]))
	for _, mr := range m.mrs[repo] {
		if state == "" || mr.State == state {
			out = append(out, *mr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IID > out[j].IID })
	return out, nil
}

func (m *MemoryMetaStore) TransitionMR(_ context.Context, repo string, iid int64, from string, patch func(*MRInfo)) (MRInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mr, ok := m.mrs[repo][iid]
	if !ok {
		return MRInfo{}, fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
	}
	if mr.State != from {
		return MRInfo{}, fmt.Errorf("%w: %s!%d is %s, expected %s", ErrMRState, repo, iid, mr.State, from)
	}
	patch(mr)
	if err := mr.Valid(); err != nil {
		return MRInfo{}, err
	}
	return *mr, nil
}

func (m *MemoryMetaStore) AddApproval(_ context.Context, repo string, iid int64, user string, at int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mrs[repo][iid]; !ok {
		return fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
	}
	if m.mrApprovals[repo] == nil {
		m.mrApprovals[repo] = map[int64]map[string]int64{}
	}
	if m.mrApprovals[repo][iid] == nil {
		m.mrApprovals[repo][iid] = map[string]int64{}
	}
	m.mrApprovals[repo][iid][user] = at
	return nil
}

func (m *MemoryMetaStore) RemoveApproval(_ context.Context, repo string, iid int64, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mrApprovals[repo] != nil && m.mrApprovals[repo][iid] != nil {
		delete(m.mrApprovals[repo][iid], user)
	}
	return nil
}

func (m *MemoryMetaStore) ListApprovals(_ context.Context, repo string, iid int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mrs[repo][iid]; !ok {
		return nil, fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
	}
	users := make([]string, 0, len(m.mrApprovals[repo][iid]))
	for u := range m.mrApprovals[repo][iid] {
		users = append(users, u)
	}
	sort.Strings(users)
	return users, nil
}

func (m *MemoryMetaStore) ListApprovalDetails(_ context.Context, repo string, iid int64) ([]MRApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mrs[repo][iid]; !ok {
		return nil, fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
	}
	out := make([]MRApproval, 0, len(m.mrApprovals[repo][iid]))
	for u, at := range m.mrApprovals[repo][iid] {
		out = append(out, MRApproval{User: u, At: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}

func (m *MemoryMetaStore) PutBranchRule(_ context.Context, r BranchRule) error {
	if err := r.Valid(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.branchRules[r.Repo] == nil {
		m.branchRules[r.Repo] = map[string]BranchRule{}
	}
	m.branchRules[r.Repo][r.Branch] = r
	return nil
}

func (m *MemoryMetaStore) BranchRules(_ context.Context, repo string) ([]BranchRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]BranchRule, 0, len(m.branchRules[repo]))
	for _, r := range m.branchRules[repo] {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Branch < out[j].Branch })
	return out, nil
}

func (m *MemoryMetaStore) BranchRuleOf(_ context.Context, repo, branch string) (BranchRule, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.branchRules[repo][branch]
	return r, ok, nil
}

func (m *MemoryMetaStore) DeleteBranchRule(_ context.Context, repo, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.branchRules[repo] != nil {
		delete(m.branchRules[repo], branch)
	}
	return nil
}

// --- SQL implementation (sqlite / mysql / postgres share it) ---

// mrCreateTries bounds the CreateMR allocation retry. IID allocation is
// MAX+1 under write serialization, so every collision is a concurrent mint
// that won the race — under bursty contention (eight creators at once,
// the conformance matrix) each loser needs one try per already-lost round,
// and the MVCC backends may also surface a deadlock that costs a round.
const mrCreateTries = 10

// mrAllocationRetryable reports whether a failed CreateMR try was the
// allocation protocol's own collision and can simply run again: the lost
// MAX+1 race (unique key) or the MVCC lock contention it provokes —
// MySQL deadlock 1213 / lock-wait 1205, PostgreSQL deadlock 40P01 /
// serialization 40001 (the pg unique violation 23505 joins belt-and-braces;
// the translator already maps it to ErrDuplicatedKey).
func mrAllocationRetryable(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var my *gmysql.MySQLError
	if errors.As(err, &my) && (my.Number == 1213 || my.Number == 1205) {
		return true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "40P01", "40001", "23505":
			return true
		}
	}
	return false
}

func (s *SQLMetaStore) CreateMR(ctx context.Context, in MRInfo) (MRInfo, error) {
	if err := in.Valid(); err != nil {
		return MRInfo{}, err
	}
	for try := 0; try < mrCreateTries; try++ {
		var iid int64
		err := s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if s.d.rowLock != "" {
				if err := txq(tx).Model(&mrRow{}).Select("iid").
					Where("repo = ?", in.Repo).Clauses(clause.Locking{Strength: "UPDATE"}).
					Order("iid DESC").Limit(1).Scan(&iid).Error; err != nil {
					return err
				}
			} else if err := txq(tx).Model(&mrRow{}).Select("iid").
				Where("repo = ?", in.Repo).Order("iid DESC").Limit(1).Scan(&iid).Error; err != nil {
				return err
			}
			in.IID = iid + 1
			return txq(tx).Create(&mrRow{MRInfo: in}).Error
		})
		if err != nil && !mrAllocationRetryable(err) {
			return MRInfo{}, err
		}
		if err == nil {
			return in, nil
		}
		// lost the MAX+1 race or the lock contention it provokes; recompute
	}
	return MRInfo{}, fmt.Errorf("mr iid allocation raced %d times", mrCreateTries)
}

func (s *SQLMetaStore) MRByIID(ctx context.Context, repo string, iid int64) (MRInfo, error) {
	var row mrRow
	err := s.orm.WithContext(ctx).Where("repo = ? AND iid = ?", repo, iid).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return MRInfo{}, fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
	}
	if err != nil {
		return MRInfo{}, err
	}
	return row.MRInfo, nil
}

func (s *SQLMetaStore) ListMRs(ctx context.Context, repo, state string) ([]MRInfo, error) {
	q := s.orm.WithContext(ctx).Model(&mrRow{}).Where("repo = ?", repo)
	if state != "" {
		q = q.Where("state = ?", state)
	}
	var rows []mrRow
	if err := q.Order("iid DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MRInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.MRInfo)
	}
	return out, nil
}

func (s *SQLMetaStore) TransitionMR(ctx context.Context, repo string, iid int64, from string, patch func(*MRInfo)) (MRInfo, error) {
	var out MRInfo
	err := s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := txq(tx).Where("repo = ? AND iid = ?", repo, iid)
		if s.d.rowLock != "" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var row mrRow
		if err := q.Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
			}
			return err
		}
		if row.MRInfo.State != from {
			return fmt.Errorf("%w: %s!%d is %s, expected %s", ErrMRState, repo, iid, row.MRInfo.State, from)
		}
		patch(&row.MRInfo)
		if err := row.MRInfo.Valid(); err != nil {
			return err
		}
		out = row.MRInfo
		return txq(tx).Model(&mrRow{}).Where("repo = ? AND iid = ?", repo, iid).Updates(row.MRInfo.updates()).Error
	})
	if err != nil {
		return MRInfo{}, err
	}
	return out, nil
}

func (s *SQLMetaStore) AddApproval(ctx context.Context, repo string, iid int64, user string, at int64) error {
	if _, err := s.MRByIID(ctx, repo, iid); err != nil {
		return err
	}
	// Upsert by composite key: re-approving moves the timestamp, both legal.
	return s.orm.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&mrApprovalRow{Repo: repo, IID: iid, Username: user, ApprovedAt: at}).Error
}

func (s *SQLMetaStore) RemoveApproval(ctx context.Context, repo string, iid int64, user string) error {
	return s.orm.WithContext(ctx).
		Where("repo = ? AND iid = ? AND username = ?", repo, iid, user).
		Delete(&mrApprovalRow{}).Error
}

func (s *SQLMetaStore) ListApprovals(ctx context.Context, repo string, iid int64) ([]string, error) {
	if _, err := s.MRByIID(ctx, repo, iid); err != nil {
		return nil, err
	}
	var rows []mrApprovalRow
	if err := s.orm.WithContext(ctx).Model(&mrApprovalRow{}).
		Where("repo = ? AND iid = ?", repo, iid).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	users := make([]string, 0, len(rows))
	for _, r := range rows {
		users = append(users, r.Username)
	}
	return users, nil
}

func (s *SQLMetaStore) ListApprovalDetails(ctx context.Context, repo string, iid int64) ([]MRApproval, error) {
	if _, err := s.MRByIID(ctx, repo, iid); err != nil {
		return nil, err
	}
	var rows []mrApprovalRow
	if err := s.orm.WithContext(ctx).Model(&mrApprovalRow{}).
		Where("repo = ? AND iid = ?", repo, iid).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MRApproval, 0, len(rows))
	for _, r := range rows {
		out = append(out, MRApproval{User: r.Username, At: r.ApprovedAt})
	}
	return out, nil
}

func (s *SQLMetaStore) PutBranchRule(ctx context.Context, r BranchRule) error {
	if err := r.Valid(); err != nil {
		return err
	}
	return s.orm.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&branchRuleRow{Repo: r.Repo, Branch: r.Branch, Level: r.Level, Approvals: r.Approvals}).Error
}

func (s *SQLMetaStore) BranchRules(ctx context.Context, repo string) ([]BranchRule, error) {
	var rows []branchRuleRow
	if err := s.orm.WithContext(ctx).Where("repo = ?", repo).Order("branch").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]BranchRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, BranchRule{Repo: r.Repo, Branch: r.Branch, Level: r.Level, Approvals: r.Approvals})
	}
	return out, nil
}

func (s *SQLMetaStore) BranchRuleOf(ctx context.Context, repo, branch string) (BranchRule, bool, error) {
	var row branchRuleRow
	err := s.orm.WithContext(ctx).Where("repo = ? AND branch = ?", repo, branch).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return BranchRule{}, false, nil
	}
	if err != nil {
		return BranchRule{}, false, err
	}
	return BranchRule{Repo: row.Repo, Branch: row.Branch, Level: row.Level, Approvals: row.Approvals}, true, nil
}

func (s *SQLMetaStore) DeleteBranchRule(ctx context.Context, repo, branch string) error {
	return s.orm.WithContext(ctx).
		Where("repo = ? AND branch = ?", repo, branch).Delete(&branchRuleRow{}).Error
}
