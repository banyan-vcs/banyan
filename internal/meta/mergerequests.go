// Merge requests and branch protection rules in the metadata plane
// (docs/design/merge-requests.md). The merge request ledger and the branch rules are
// cluster state: any gateway with the metadata answers, and the rule reader
// (receive-pack, the JSON ref API) sits in the same process as the writer —
// no snapshot sync, no cache to kick. MergeRequestStore and BranchRuleStore follow
// TokenRegistry: deliberately NOT part of the MetaStore composite; consumers
// type-assert (meta.(MergeRequestStore)) so remote-meta gateways compile unchanged.
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

// merge request states: open → merged (terminal, the merge commit is recorded) or
// open → closed → open (reopen; the IID is never reused either way).
const (
	MergeRequestStateOpen   = "open"
	MergeRequestStateMerged = "merged"
	MergeRequestStateClosed = "closed"
)

// Branch protection levels. "protected" keeps the existing snapshot-policy
// semantics (direct push needs owner rank); "merge-request-only" is the new strict
// level: NOBODY pushes the branch directly — not even admins — the merge
// request path is the only way in, gated by the rule's approval count.
const (
	RuleProtected        = "protected"
	RuleMergeRequestOnly = "merge-request-only"
)

// ErrMergeRequestUnknown answers lookups of merge requests that do not exist.
var ErrMergeRequestUnknown = errors.New("merge request unknown")

// ErrMergeRequestState is the optimistic-guard failure: the transition expected a
// different state (a concurrent merge/close got there first).
var ErrMergeRequestState = errors.New("merge request state changed")

// ErrMergeRequestInvalid and ErrBranchRuleInvalid mark store-level validation
// refusals — client-input problems, not server faults, so the JSON surface
// translates them to INVALID_ARG rather than INTERNAL.
var (
	ErrMergeRequestInvalid         = errors.New("invalid merge request")
	ErrBranchRuleInvalid = errors.New("invalid branch rule")
)

// MergeRequestInfo is one merge request. Identity is (Repo, IID) — the IID is
// allocated per repository, monotonic, and never reused, so merged and
// closed requests keep shareable URLs forever.
type MergeRequestInfo struct {
	Repo        string `json:"repo"`
	IID         int64  `json:"iid" gorm:"column:iid"` // IID would snake-case to "i_id" (the registered trap)
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`       // source branch name
	Target      string `json:"target"`       // target branch name
	State       string `json:"state"`        // open | merged | closed
	Author      string `json:"author"`       // opening identity (id.User)
	SourceHead  string `json:"source_head"`  // source tip at open time
	MergeCommit string `json:"merge_commit"` // set when merged
	MergedBy    string `json:"merged_by"`    // merging identity, set when merged
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

func (m MergeRequestInfo) Valid() error {
	if m.Repo == "" || m.Author == "" {
		return fmt.Errorf("%w: repo and author required", ErrMergeRequestInvalid)
	}
	if m.Source == "" || m.Target == "" || m.Source == m.Target {
		return fmt.Errorf("%w: distinct source and target branches required", ErrMergeRequestInvalid)
	}
	if m.Title == "" {
		return fmt.Errorf("%w: title required", ErrMergeRequestInvalid)
	}
	if m.State != MergeRequestStateOpen && m.State != MergeRequestStateMerged && m.State != MergeRequestStateClosed {
		return fmt.Errorf("%w: state must be open|merged|closed", ErrMergeRequestInvalid)
	}
	return nil
}

// MergeRequestStore manages the merge-request ledger. All methods are safe for
// concurrent use; CreateMergeRequest allocates the per-repo IID under the store's
// serialization (retrying the rare unique-key race), and TransitionMergeRequest is
// the optimistic state gate two racing merges cannot both pass.
type MergeRequestStore interface {
	// CreateMergeRequest stores a new open merge request, filling IID. The caller sets
	// timestamps; the store validates and serializes.
	CreateMergeRequest(ctx context.Context, m MergeRequestInfo) (MergeRequestInfo, error)
	// MergeRequestByIID answers one merge request (ErrMergeRequestUnknown otherwise).
	MergeRequestByIID(ctx context.Context, repo string, iid int64) (MergeRequestInfo, error)
	// ListMergeRequests answers the repo's merge requests, newest IID first; state "" = all.
	ListMergeRequests(ctx context.Context, repo, state string) ([]MergeRequestInfo, error)
	// TransitionMergeRequest applies patch under an optimistic guard: the update
	// lands only when the row's current state is `from` (ErrMergeRequestState
	// otherwise; ErrMergeRequestUnknown for a missing row). patch mutates the stored
	// copy — state, merge_commit, merged_by, timestamps — before the write.
	TransitionMergeRequest(ctx context.Context, repo string, iid int64, from string, patch func(*MergeRequestInfo)) (MergeRequestInfo, error)
	// AddApproval records one approval (idempotent upsert).
	AddApproval(ctx context.Context, repo string, iid int64, user string, at int64) error
	// RemoveApproval drops one approval (idempotent).
	RemoveApproval(ctx context.Context, repo string, iid int64, user string) error
	// ListApprovals answers the merge request's approvers, sorted by name.
	ListApprovals(ctx context.Context, repo string, iid int64) ([]string, error)
	// ListApprovalDetails answers the approvers WITH their timestamps —
	// the discussion timeline's event rows (approval is an event, not just
	// a count), sorted by name.
	ListApprovalDetails(ctx context.Context, repo string, iid int64) ([]MergeRequestApproval, error)
}

// MergeRequestApproval is one recorded approval with its timestamp (the timeline's
// event shape; ListApprovals stays the count-only view the merge gate reads).
type MergeRequestApproval struct {
	User string `json:"user"`
	At   int64  `json:"at"`
}

// BranchRule is one branch's protection entry. Level RuleProtected mirrors
// what the auth snapshot already enforces (kept here so the rule list is
// the one surface operators read); RuleMergeRequestOnly is enforced by the gateway's
// push gates themselves. Approvals is the required count for merging an merge request
// into the branch (0..9; 1 when the level is merge-request-only).
type BranchRule struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Level     string `json:"level"` // protected | merge-request-only
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
	case RuleProtected, RuleMergeRequestOnly:
	default:
		return fmt.Errorf("%w: level must be protected|merge-request-only", ErrBranchRuleInvalid)
	}
	if r.Approvals < 0 || r.Approvals > 9 {
		return fmt.Errorf("%w: approvals must be 0..9", ErrBranchRuleInvalid)
	}
	if r.Level == RuleMergeRequestOnly && r.Approvals == 0 {
		return fmt.Errorf("%w: merge-request-only needs at least one approval", ErrBranchRuleInvalid)
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

func (m *MemoryMetaStore) CreateMergeRequest(_ context.Context, in MergeRequestInfo) (MergeRequestInfo, error) {
	if err := in.Valid(); err != nil {
		return MergeRequestInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mergeRequests[in.Repo] == nil {
		m.mergeRequests[in.Repo] = map[int64]*MergeRequestInfo{}
	}
	m.mergeRequestNextIID[in.Repo]++
	in.IID = m.mergeRequestNextIID[in.Repo]
	m.mergeRequests[in.Repo][in.IID] = &in
	return in, nil
}

func (m *MemoryMetaStore) MergeRequestByIID(_ context.Context, repo string, iid int64) (MergeRequestInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mergeRequest, ok := m.mergeRequests[repo][iid]
	if !ok {
		return MergeRequestInfo{}, fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
	}
	return *mergeRequest, nil
}

func (m *MemoryMetaStore) ListMergeRequests(_ context.Context, repo, state string) ([]MergeRequestInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MergeRequestInfo, 0, len(m.mergeRequests[repo]))
	for _, mergeRequest := range m.mergeRequests[repo] {
		if state == "" || mergeRequest.State == state {
			out = append(out, *mergeRequest)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IID > out[j].IID })
	return out, nil
}

func (m *MemoryMetaStore) TransitionMergeRequest(_ context.Context, repo string, iid int64, from string, patch func(*MergeRequestInfo)) (MergeRequestInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mergeRequest, ok := m.mergeRequests[repo][iid]
	if !ok {
		return MergeRequestInfo{}, fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
	}
	if mergeRequest.State != from {
		return MergeRequestInfo{}, fmt.Errorf("%w: %s!%d is %s, expected %s", ErrMergeRequestState, repo, iid, mergeRequest.State, from)
	}
	patch(mergeRequest)
	if err := mergeRequest.Valid(); err != nil {
		return MergeRequestInfo{}, err
	}
	return *mergeRequest, nil
}

func (m *MemoryMetaStore) AddApproval(_ context.Context, repo string, iid int64, user string, at int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mergeRequests[repo][iid]; !ok {
		return fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
	}
	if m.mergeRequestApprovals[repo] == nil {
		m.mergeRequestApprovals[repo] = map[int64]map[string]int64{}
	}
	if m.mergeRequestApprovals[repo][iid] == nil {
		m.mergeRequestApprovals[repo][iid] = map[string]int64{}
	}
	m.mergeRequestApprovals[repo][iid][user] = at
	return nil
}

func (m *MemoryMetaStore) RemoveApproval(_ context.Context, repo string, iid int64, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mergeRequestApprovals[repo] != nil && m.mergeRequestApprovals[repo][iid] != nil {
		delete(m.mergeRequestApprovals[repo][iid], user)
	}
	return nil
}

func (m *MemoryMetaStore) ListApprovals(_ context.Context, repo string, iid int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mergeRequests[repo][iid]; !ok {
		return nil, fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
	}
	users := make([]string, 0, len(m.mergeRequestApprovals[repo][iid]))
	for u := range m.mergeRequestApprovals[repo][iid] {
		users = append(users, u)
	}
	sort.Strings(users)
	return users, nil
}

func (m *MemoryMetaStore) ListApprovalDetails(_ context.Context, repo string, iid int64) ([]MergeRequestApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mergeRequests[repo][iid]; !ok {
		return nil, fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
	}
	out := make([]MergeRequestApproval, 0, len(m.mergeRequestApprovals[repo][iid]))
	for u, at := range m.mergeRequestApprovals[repo][iid] {
		out = append(out, MergeRequestApproval{User: u, At: at})
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

// mergeRequestCreateTries bounds the CreateMergeRequest allocation retry. IID allocation is
// MAX+1 under write serialization, so every collision is a concurrent mint
// that won the race — under bursty contention (eight creators at once,
// the conformance matrix) each loser needs one try per already-lost round,
// and the MVCC backends may also surface a deadlock that costs a round.
const mergeRequestCreateTries = 10

// mergeRequestAllocationRetryable reports whether a failed CreateMergeRequest try was the
// allocation protocol's own collision and can simply run again: the lost
// MAX+1 race (unique key) or the MVCC lock contention it provokes —
// MySQL deadlock 1213 / lock-wait 1205, PostgreSQL deadlock 40P01 /
// serialization 40001 (the pg unique violation 23505 joins belt-and-braces;
// the translator already maps it to ErrDuplicatedKey).
func mergeRequestAllocationRetryable(err error) bool {
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

func (s *SQLMetaStore) CreateMergeRequest(ctx context.Context, in MergeRequestInfo) (MergeRequestInfo, error) {
	if err := in.Valid(); err != nil {
		return MergeRequestInfo{}, err
	}
	for try := 0; try < mergeRequestCreateTries; try++ {
		var iid int64
		err := s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if s.d.rowLock != "" {
				if err := txq(tx).Model(&mergeRequestRow{}).Select("iid").
					Where("repo = ?", in.Repo).Clauses(clause.Locking{Strength: "UPDATE"}).
					Order("iid DESC").Limit(1).Scan(&iid).Error; err != nil {
					return err
				}
			} else if err := txq(tx).Model(&mergeRequestRow{}).Select("iid").
				Where("repo = ?", in.Repo).Order("iid DESC").Limit(1).Scan(&iid).Error; err != nil {
				return err
			}
			in.IID = iid + 1
			return txq(tx).Create(&mergeRequestRow{MergeRequestInfo: in}).Error
		})
		if err != nil && !mergeRequestAllocationRetryable(err) {
			return MergeRequestInfo{}, err
		}
		if err == nil {
			return in, nil
		}
		// lost the MAX+1 race or the lock contention it provokes; recompute
	}
	return MergeRequestInfo{}, fmt.Errorf("merge request iid allocation raced %d times", mergeRequestCreateTries)
}

func (s *SQLMetaStore) MergeRequestByIID(ctx context.Context, repo string, iid int64) (MergeRequestInfo, error) {
	var row mergeRequestRow
	err := s.orm.WithContext(ctx).Where("repo = ? AND iid = ?", repo, iid).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return MergeRequestInfo{}, fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
	}
	if err != nil {
		return MergeRequestInfo{}, err
	}
	return row.MergeRequestInfo, nil
}

func (s *SQLMetaStore) ListMergeRequests(ctx context.Context, repo, state string) ([]MergeRequestInfo, error) {
	q := s.orm.WithContext(ctx).Model(&mergeRequestRow{}).Where("repo = ?", repo)
	if state != "" {
		q = q.Where("state = ?", state)
	}
	var rows []mergeRequestRow
	if err := q.Order("iid DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MergeRequestInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.MergeRequestInfo)
	}
	return out, nil
}

func (s *SQLMetaStore) TransitionMergeRequest(ctx context.Context, repo string, iid int64, from string, patch func(*MergeRequestInfo)) (MergeRequestInfo, error) {
	var out MergeRequestInfo
	err := s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := txq(tx).Where("repo = ? AND iid = ?", repo, iid)
		if s.d.rowLock != "" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var row mergeRequestRow
		if err := q.Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
			}
			return err
		}
		if row.MergeRequestInfo.State != from {
			return fmt.Errorf("%w: %s!%d is %s, expected %s", ErrMergeRequestState, repo, iid, row.MergeRequestInfo.State, from)
		}
		patch(&row.MergeRequestInfo)
		if err := row.MergeRequestInfo.Valid(); err != nil {
			return err
		}
		out = row.MergeRequestInfo
		return txq(tx).Model(&mergeRequestRow{}).Where("repo = ? AND iid = ?", repo, iid).Updates(row.MergeRequestInfo.updates()).Error
	})
	if err != nil {
		return MergeRequestInfo{}, err
	}
	return out, nil
}

func (s *SQLMetaStore) AddApproval(ctx context.Context, repo string, iid int64, user string, at int64) error {
	if _, err := s.MergeRequestByIID(ctx, repo, iid); err != nil {
		return err
	}
	// Upsert by composite key: re-approving moves the timestamp, both legal.
	return s.orm.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&mergeRequestApprovalRow{Repo: repo, IID: iid, Username: user, ApprovedAt: at}).Error
}

func (s *SQLMetaStore) RemoveApproval(ctx context.Context, repo string, iid int64, user string) error {
	return s.orm.WithContext(ctx).
		Where("repo = ? AND iid = ? AND username = ?", repo, iid, user).
		Delete(&mergeRequestApprovalRow{}).Error
}

func (s *SQLMetaStore) ListApprovals(ctx context.Context, repo string, iid int64) ([]string, error) {
	if _, err := s.MergeRequestByIID(ctx, repo, iid); err != nil {
		return nil, err
	}
	var rows []mergeRequestApprovalRow
	if err := s.orm.WithContext(ctx).Model(&mergeRequestApprovalRow{}).
		Where("repo = ? AND iid = ?", repo, iid).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	users := make([]string, 0, len(rows))
	for _, r := range rows {
		users = append(users, r.Username)
	}
	return users, nil
}

func (s *SQLMetaStore) ListApprovalDetails(ctx context.Context, repo string, iid int64) ([]MergeRequestApproval, error) {
	if _, err := s.MergeRequestByIID(ctx, repo, iid); err != nil {
		return nil, err
	}
	var rows []mergeRequestApprovalRow
	if err := s.orm.WithContext(ctx).Model(&mergeRequestApprovalRow{}).
		Where("repo = ? AND iid = ?", repo, iid).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MergeRequestApproval, 0, len(rows))
	for _, r := range rows {
		out = append(out, MergeRequestApproval{User: r.Username, At: r.ApprovedAt})
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
