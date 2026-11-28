// mergerequestcomments.go — the merge-request discussion ledger: comments (optionally
// anchored to a diff line), replies, and review submissions
// (docs/design/merge-requests.md §review). One table, one shape: a comment
// is a plain remark or a line-anchored thread head; a review is the
// conclusion bar's submission — a verdict plus an optional body. The verdict
// approve/request-changes is the merge gate's second lock (the service
// computes "latest review per user" from this ledger; the store stays dumb).
// MergeRequestCommentStore follows MergeRequestStore: NOT part of the MetaStore composite;
// consumers type-assert so remote-meta gateways compile unchanged.
package meta

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Comment kinds.
const (
	CommentKindComment = "comment"
	CommentKindReview  = "review"
)

// Review verdicts (kind=review only).
const (
	VerdictApprove        = "approve"
	VerdictRequestChanges = "request-changes"
	VerdictComment        = "comment"
)

// ErrCommentInvalid is the store-level validation refusal (client input,
// translated to INVALID_ARG at the JSON face).
var ErrCommentInvalid = errors.New("invalid merge request comment")

// MergeRequestComment is one discussion entry. Identity is (Repo, IID, ID) — the ID
// allocates per merge request, monotonic, ascending in time; replies carry
// the head's ID in ParentID and inherit its anchor (a reply's own anchor
// fields stay empty by validation).
type MergeRequestComment struct {
	ID         int64  `json:"id"`
	Repo       string `json:"repo"`
	IID        int64  `json:"iid" gorm:"column:iid"` // IID would snake-case to "i_id" (the registered trap)
	ParentID   int64  `json:"parent_id,omitempty"`   // 0 = thread head
	Author     string `json:"author"`
	Body       string `json:"body"`
	Kind       string `json:"kind"`                                          // comment | review
	Verdict    string `json:"verdict,omitempty"`                             // reviews only: approve | request-changes | comment
	Path       string `json:"path,omitempty"`                                // anchor: repo-relative path
	Line       int    `json:"line,omitempty"`                                // anchor: line number on Side
	Side       string `json:"side,omitempty"`                                // anchor: old | new
	AnchorOID  string `json:"anchor_oid,omitempty" gorm:"column:anchor_oid"` // anchor: source tip at comment time (OID would snake-case to "o_id" — the registered trap)
	Resolved   bool   `json:"resolved"`                                      // thread heads only
	ResolvedBy string `json:"resolved_by,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// Valid is the store-level shape check (parent-existence is the service's
// to verify — it needs a read anyway).
func (c MergeRequestComment) Valid() error {
	if c.Repo == "" || c.Author == "" || c.IID <= 0 {
		return fmt.Errorf("%w: repo, iid and author required", ErrCommentInvalid)
	}
	switch c.Kind {
	case CommentKindComment:
		if c.Verdict != "" {
			return fmt.Errorf("%w: plain comments carry no verdict", ErrCommentInvalid)
		}
		if c.Body == "" {
			return fmt.Errorf("%w: body required", ErrCommentInvalid)
		}
	case CommentKindReview:
		switch c.Verdict {
		case VerdictApprove, VerdictRequestChanges, VerdictComment:
		default:
			return fmt.Errorf("%w: review verdict must be approve|request-changes|comment", ErrCommentInvalid)
		}
		if c.Path != "" || c.Line != 0 {
			return fmt.Errorf("%w: reviews are not line-anchored", ErrCommentInvalid)
		}
	default:
		return fmt.Errorf("%w: kind must be comment|review", ErrCommentInvalid)
	}
	if c.ParentID < 0 {
		return fmt.Errorf("%w: parent id must be >= 0", ErrCommentInvalid)
	}
	if c.ParentID != 0 && (c.Path != "" || c.Line != 0 || c.Side != "" || c.AnchorOID != "") {
		return fmt.Errorf("%w: replies inherit the head's anchor", ErrCommentInvalid)
	}
	if c.Line < 0 {
		return fmt.Errorf("%w: line must be >= 0", ErrCommentInvalid)
	}
	if c.Line > 0 || c.Path != "" || c.Side != "" || c.AnchorOID != "" {
		// An anchor is all-or-nothing: path + line + side + the tip it read.
		if c.Path == "" || c.Line == 0 || (c.Side != "old" && c.Side != "new") || c.AnchorOID == "" {
			return fmt.Errorf("%w: an anchor needs path, line, side and anchor oid together", ErrCommentInvalid)
		}
	}
	return nil
}

// MergeRequestCommentStore manages the discussion ledger. All methods are safe for
// concurrent use; CreateMergeRequestComment allocates the per-request ID under the
// store's serialization (the CreateMergeRequest allocation protocol, replayed).
type MergeRequestCommentStore interface {
	// CreateMergeRequestComment stores one entry, filling ID. The caller sets
	// timestamps; the store validates. Replies must reference an existing
	// head of the same request (ErrCommentUnknown otherwise).
	CreateMergeRequestComment(ctx context.Context, c MergeRequestComment) (MergeRequestComment, error)
	// ListMergeRequestComments answers the request's discussion, ID-ascending — the
	// timeline order. ErrMergeRequestUnknown for a missing request.
	ListMergeRequestComments(ctx context.Context, repo string, iid int64) ([]MergeRequestComment, error)
	// SetMergeRequestCommentResolved flips a thread head's resolved flag (replies and
	// missing heads refuse; idempotent for the flag's current value).
	SetMergeRequestCommentResolved(ctx context.Context, repo string, iid, id int64, resolved bool, by string) error
}

// ErrCommentUnknown answers resolve attempts at entries that do not exist
// (or are replies, which cannot resolve).
var ErrCommentUnknown = errors.New("merge request comment unknown")

// --- memory implementation ---

func (m *MemoryMetaStore) CreateMergeRequestComment(_ context.Context, in MergeRequestComment) (MergeRequestComment, error) {
	if err := in.Valid(); err != nil {
		return MergeRequestComment{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mergeRequests[in.Repo][in.IID]; !ok {
		return MergeRequestComment{}, fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, in.Repo, in.IID)
	}
	if in.ParentID != 0 {
		if _, ok := m.mergeRequestComments[in.Repo][in.IID][in.ParentID]; !ok || m.mergeRequestComments[in.Repo][in.IID][in.ParentID].ParentID != 0 {
			return MergeRequestComment{}, fmt.Errorf("%w: %s!%d c%d", ErrCommentUnknown, in.Repo, in.IID, in.ParentID)
		}
	}
	key := in.Repo + "\x00" + fmt.Sprint(in.IID)
	m.mergeRequestNextCommentID[key]++
	in.ID = m.mergeRequestNextCommentID[key]
	if m.mergeRequestComments[in.Repo] == nil {
		m.mergeRequestComments[in.Repo] = map[int64]map[int64]*MergeRequestComment{}
	}
	if m.mergeRequestComments[in.Repo][in.IID] == nil {
		m.mergeRequestComments[in.Repo][in.IID] = map[int64]*MergeRequestComment{}
	}
	c := in
	m.mergeRequestComments[in.Repo][in.IID][in.ID] = &c
	return in, nil
}

func (m *MemoryMetaStore) ListMergeRequestComments(_ context.Context, repo string, iid int64) ([]MergeRequestComment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mergeRequests[repo][iid]; !ok {
		return nil, fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
	}
	out := make([]MergeRequestComment, 0, len(m.mergeRequestComments[repo][iid]))
	for _, c := range m.mergeRequestComments[repo][iid] {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryMetaStore) SetMergeRequestCommentResolved(_ context.Context, repo string, iid, id int64, resolved bool, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.mergeRequestComments[repo][iid][id]
	if !ok || c.ParentID != 0 {
		return fmt.Errorf("%w: %s!%d c%d", ErrCommentUnknown, repo, iid, id)
	}
	c.Resolved = resolved
	c.ResolvedBy = by
	return nil
}

// --- SQL implementation (sqlite / mysql / postgres share it) ---

// mergeRequestCommentCreateTries bounds the per-request ID allocation retry — the
// CreateMergeRequest protocol on the smaller key.
const mergeRequestCommentCreateTries = 10

func (s *SQLMetaStore) CreateMergeRequestComment(ctx context.Context, in MergeRequestComment) (MergeRequestComment, error) {
	if err := in.Valid(); err != nil {
		return MergeRequestComment{}, err
	}
	for try := 0; try < mergeRequestCommentCreateTries; try++ {
		var id int64
		err := s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if _, err := mergeRequestMustExist(tx, in.Repo, in.IID); err != nil {
				return err
			}
			if in.ParentID != 0 {
				var parent mergeRequestCommentRow
				if err := txq(tx).Where("repo = ? AND iid = ? AND id = ?", in.Repo, in.IID, in.ParentID).
					Take(&parent).Error; err != nil {
					if errors.Is(err, gorm.ErrRecordNotFound) {
						return fmt.Errorf("%w: %s!%d c%d", ErrCommentUnknown, in.Repo, in.IID, in.ParentID)
					}
					return err
				}
				// Only thread heads take replies — a reply to a reply is a
				// reshape the timeline never agreed to.
				if parent.ParentID != 0 {
					return fmt.Errorf("%w: %s!%d c%d", ErrCommentUnknown, in.Repo, in.IID, in.ParentID)
				}
			}
			q := txq(tx).Model(&mergeRequestCommentRow{}).Select("id").
				Where("repo = ? AND iid = ?", in.Repo, in.IID)
			if s.d.rowLock != "" {
				q = q.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			q = q.Order("id DESC").Limit(1)
			if err := q.Scan(&id).Error; err != nil {
				return err
			}
			in.ID = id + 1
			row := commentRow(in)
			return txq(tx).Create(&row).Error
		})
		if err != nil {
			if errors.Is(err, ErrMergeRequestUnknown) || errors.Is(err, ErrCommentUnknown) || !mergeRequestAllocationRetryable(err) {
				return MergeRequestComment{}, err
			}
			// lost the MAX+1 race; recompute
			continue
		}
		return in, nil
	}
	return MergeRequestComment{}, fmt.Errorf("merge request comment id allocation raced %d times", mergeRequestCommentCreateTries)
}

func (s *SQLMetaStore) ListMergeRequestComments(ctx context.Context, repo string, iid int64) ([]MergeRequestComment, error) {
	if _, err := s.MergeRequestByIID(ctx, repo, iid); err != nil {
		return nil, err
	}
	var rows []mergeRequestCommentRow
	if err := s.orm.WithContext(ctx).Where("repo = ? AND iid = ?", repo, iid).
		Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MergeRequestComment, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.comment())
	}
	return out, nil
}

func (s *SQLMetaStore) SetMergeRequestCommentResolved(ctx context.Context, repo string, iid, id int64, resolved bool, by string) error {
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row mergeRequestCommentRow
		q := txq(tx).Where("repo = ? AND iid = ? AND id = ?", repo, iid, id)
		if s.d.rowLock != "" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: %s!%d c%d", ErrCommentUnknown, repo, iid, id)
			}
			return err
		}
		if row.ParentID != 0 { // replies never resolve
			return fmt.Errorf("%w: %s!%d c%d", ErrCommentUnknown, repo, iid, id)
		}
		// bool through the Updates map hits the PG pitfall the row's int seam
		// exists for — the encode must see the int, not the bool.
		return txq(tx).Model(&mergeRequestCommentRow{}).
			Where("repo = ? AND iid = ? AND id = ?", repo, iid, id).
			Updates(map[string]any{"resolved": boolInt(resolved), "resolved_by": by}).Error
	})
}

// mergeRequestMustExist answers ErrMergeRequestUnknown inside a transaction's tx surface.
func mergeRequestMustExist(tx *gorm.DB, repo string, iid int64) (mergeRequestRow, error) {
	var row mergeRequestRow
	if err := txq(tx).Where("repo = ? AND iid = ?", repo, iid).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return mergeRequestRow{}, fmt.Errorf("%w: %s!%d", ErrMergeRequestUnknown, repo, iid)
		}
		return mergeRequestRow{}, err
	}
	return row, nil
}
