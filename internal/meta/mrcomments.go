// mrcomments.go — the merge-request discussion ledger: comments (optionally
// anchored to a diff line), replies, and review submissions
// (docs/design/merge-requests.md §review). One table, one shape: a comment
// is a plain remark or a line-anchored thread head; a review is the
// conclusion bar's submission — a verdict plus an optional body. The verdict
// approve/request-changes is the merge gate's second lock (the service
// computes "latest review per user" from this ledger; the store stays dumb).
// MRCommentStore follows MRStore: NOT part of the MetaStore composite;
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

// MRComment is one discussion entry. Identity is (Repo, IID, ID) — the ID
// allocates per merge request, monotonic, ascending in time; replies carry
// the head's ID in ParentID and inherit its anchor (a reply's own anchor
// fields stay empty by validation).
type MRComment struct {
	ID         int64  `json:"id"`
	Repo       string `json:"repo"`
	IID        int64  `json:"iid" gorm:"column:iid"` // IID would snake-case to "i_id" (the registered trap)
	ParentID   int64  `json:"parent_id,omitempty"`   // 0 = thread head
	Author     string `json:"author"`
	Body       string `json:"body"`
	Kind       string `json:"kind"`                // comment | review
	Verdict    string `json:"verdict,omitempty"`   // reviews only: approve | request-changes | comment
	Path       string `json:"path,omitempty"`      // anchor: repo-relative path
	Line       int    `json:"line,omitempty"`      // anchor: line number on Side
	Side       string `json:"side,omitempty"`      // anchor: old | new
	AnchorOID  string `json:"anchor_oid,omitempty" gorm:"column:anchor_oid"` // anchor: source tip at comment time (OID would snake-case to "o_id" — the registered trap)
	Resolved   bool   `json:"resolved"`             // thread heads only
	ResolvedBy string `json:"resolved_by,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// Valid is the store-level shape check (parent-existence is the service's
// to verify — it needs a read anyway).
func (c MRComment) Valid() error {
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

// MRCommentStore manages the discussion ledger. All methods are safe for
// concurrent use; CreateMRComment allocates the per-request ID under the
// store's serialization (the CreateMR allocation protocol, replayed).
type MRCommentStore interface {
	// CreateMRComment stores one entry, filling ID. The caller sets
	// timestamps; the store validates. Replies must reference an existing
	// head of the same request (ErrCommentUnknown otherwise).
	CreateMRComment(ctx context.Context, c MRComment) (MRComment, error)
	// ListMRComments answers the request's discussion, ID-ascending — the
	// timeline order. ErrMRUnknown for a missing request.
	ListMRComments(ctx context.Context, repo string, iid int64) ([]MRComment, error)
	// SetMRCommentResolved flips a thread head's resolved flag (replies and
	// missing heads refuse; idempotent for the flag's current value).
	SetMRCommentResolved(ctx context.Context, repo string, iid, id int64, resolved bool, by string) error
}

// ErrCommentUnknown answers resolve attempts at entries that do not exist
// (or are replies, which cannot resolve).
var ErrCommentUnknown = errors.New("merge request comment unknown")

// --- memory implementation ---

func (m *MemoryMetaStore) CreateMRComment(_ context.Context, in MRComment) (MRComment, error) {
	if err := in.Valid(); err != nil {
		return MRComment{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mrs[in.Repo][in.IID]; !ok {
		return MRComment{}, fmt.Errorf("%w: %s!%d", ErrMRUnknown, in.Repo, in.IID)
	}
	if in.ParentID != 0 {
		if _, ok := m.mrComments[in.Repo][in.IID][in.ParentID]; !ok || m.mrComments[in.Repo][in.IID][in.ParentID].ParentID != 0 {
			return MRComment{}, fmt.Errorf("%w: %s!%d c%d", ErrCommentUnknown, in.Repo, in.IID, in.ParentID)
		}
	}
	key := in.Repo + "\x00" + fmt.Sprint(in.IID)
	m.mrNextCommentID[key]++
	in.ID = m.mrNextCommentID[key]
	if m.mrComments[in.Repo] == nil {
		m.mrComments[in.Repo] = map[int64]map[int64]*MRComment{}
	}
	if m.mrComments[in.Repo][in.IID] == nil {
		m.mrComments[in.Repo][in.IID] = map[int64]*MRComment{}
	}
	c := in
	m.mrComments[in.Repo][in.IID][in.ID] = &c
	return in, nil
}

func (m *MemoryMetaStore) ListMRComments(_ context.Context, repo string, iid int64) ([]MRComment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mrs[repo][iid]; !ok {
		return nil, fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
	}
	out := make([]MRComment, 0, len(m.mrComments[repo][iid]))
	for _, c := range m.mrComments[repo][iid] {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryMetaStore) SetMRCommentResolved(_ context.Context, repo string, iid, id int64, resolved bool, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.mrComments[repo][iid][id]
	if !ok || c.ParentID != 0 {
		return fmt.Errorf("%w: %s!%d c%d", ErrCommentUnknown, repo, iid, id)
	}
	c.Resolved = resolved
	c.ResolvedBy = by
	return nil
}

// --- SQL implementation (sqlite / mysql / postgres share it) ---

// mrCommentCreateTries bounds the per-request ID allocation retry — the
// CreateMR protocol on the smaller key.
const mrCommentCreateTries = 10

func (s *SQLMetaStore) CreateMRComment(ctx context.Context, in MRComment) (MRComment, error) {
	if err := in.Valid(); err != nil {
		return MRComment{}, err
	}
	for try := 0; try < mrCommentCreateTries; try++ {
		var id int64
		err := s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if _, err := mrMustExist(tx, in.Repo, in.IID); err != nil {
				return err
			}
			if in.ParentID != 0 {
				var parent mrCommentRow
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
			q := txq(tx).Model(&mrCommentRow{}).Select("id").
				Where("repo = ? AND iid = ?", in.Repo, in.IID)
			if s.d.rowLock != "" {
				q = q.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			q = q.Order("id DESC").Limit(1)
			if err := q.Scan(&id).Error; err != nil {
				return err
			}
			in.ID = id + 1
			return txq(tx).Create(&mrCommentRow{MRComment: in}).Error
		})
		if err != nil {
			if errors.Is(err, ErrMRUnknown) || errors.Is(err, ErrCommentUnknown) || !mrAllocationRetryable(err) {
				return MRComment{}, err
			}
			// lost the MAX+1 race; recompute
			continue
		}
		return in, nil
	}
	return MRComment{}, fmt.Errorf("mr comment id allocation raced %d times", mrCommentCreateTries)
}

func (s *SQLMetaStore) ListMRComments(ctx context.Context, repo string, iid int64) ([]MRComment, error) {
	if _, err := s.MRByIID(ctx, repo, iid); err != nil {
		return nil, err
	}
	var rows []mrCommentRow
	if err := s.orm.WithContext(ctx).Where("repo = ? AND iid = ?", repo, iid).
		Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MRComment, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.MRComment)
	}
	return out, nil
}

func (s *SQLMetaStore) SetMRCommentResolved(ctx context.Context, repo string, iid, id int64, resolved bool, by string) error {
	return s.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row mrCommentRow
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
		return txq(tx).Model(&mrCommentRow{}).
			Where("repo = ? AND iid = ? AND id = ?", repo, iid, id).
			Updates(map[string]any{"resolved": resolved, "resolved_by": by}).Error
	})
}

// mrMustExist answers ErrMRUnknown inside a transaction's tx surface.
func mrMustExist(tx *gorm.DB, repo string, iid int64) (mrRow, error) {
	var row mrRow
	if err := txq(tx).Where("repo = ? AND iid = ?", repo, iid).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return mrRow{}, fmt.Errorf("%w: %s!%d", ErrMRUnknown, repo, iid)
		}
		return mrRow{}, err
	}
	return row, nil
}
