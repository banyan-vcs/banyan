package console

import (
	"context"
	"fmt"
	"time"
)

// Lightweight code review (docs/17 §review): a review is anchored — repo
// project + subtree path — and its whole lifecycle (visibility, commenting,
// verdicts) runs through the same effective-role chain as git access, so
// two subprojects' reviews never see or gate each other.

type Review struct {
	ID          int64
	ProjectID   int64
	Anchor      string // "" = project root; else clean subtree path
	Title       string
	Description string
	AuthorID    int64
	BaseRef     string // integration side (default main)
	SourceRef   string // the proposed change (a branch of the anchored view)
	Status      string // open | approved | rejected | closed
	CreatedAt   int64
	UpdatedAt   int64
}

const (
	ReviewOpen     = "open"
	ReviewApproved = "approved"
	ReviewRejected = "rejected"
	ReviewClosed   = "closed"
)

// ReviewEvent is one append-only entry of a review's timeline: a comment or
// a state transition (the transition kinds double as the actor's verdict).
type ReviewEvent struct {
	ID        int64
	ReviewID  int64
	Kind      string // comment | approve | reject | close
	AuthorID  int64
	Username  string // joined for reads
	Body      string
	CreatedAt int64
}

const reviewSchema = `
CREATE TABLE IF NOT EXISTS reviews (
  id          INTEGER PRIMARY KEY,
  project_id  INTEGER NOT NULL,
  anchor      TEXT NOT NULL DEFAULT '',
  title       TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  author_id   INTEGER NOT NULL,
  base_ref    TEXT NOT NULL,
  source_ref  TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'open',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reviews_project ON reviews(project_id);
CREATE TABLE IF NOT EXISTS review_events (
  id         INTEGER PRIMARY KEY,
  review_id  INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  author_id  INTEGER NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_review_events_review ON review_events(review_id);
`

func (d *DB) CreateReview(ctx context.Context, r Review) (Review, error) {
	if r.Title == "" || r.SourceRef == "" || r.BaseRef == "" {
		return Review{}, fmt.Errorf("%w: title, base_ref and source_ref required", ErrInvalid)
	}
	if len(r.Title) > 200 || len(r.Description) > 8000 {
		return Review{}, fmt.Errorf("%w: title ≤ 200, description ≤ 8000 chars", ErrInvalid)
	}
	if r.Status == "" {
		r.Status = ReviewOpen
	}
	now := time.Now().Unix()
	r.CreatedAt, r.UpdatedAt = now, now
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO reviews(project_id, anchor, title, description, author_id, base_ref, source_ref, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ProjectID, r.Anchor, r.Title, r.Description, r.AuthorID, r.BaseRef, r.SourceRef, r.Status, now, now)
	if err != nil {
		return Review{}, err
	}
	r.ID, _ = res.LastInsertId()
	return r, nil
}

func (d *DB) ReviewByID(ctx context.Context, id int64) (Review, error) {
	var r Review
	err := d.sql.QueryRowContext(ctx, `
		SELECT id, project_id, anchor, title, description, author_id, base_ref, source_ref, status, created_at, updated_at
		FROM reviews WHERE id=?`, id).
		Scan(&r.ID, &r.ProjectID, &r.Anchor, &r.Title, &r.Description, &r.AuthorID, &r.BaseRef, &r.SourceRef, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Review{}, normNodeErr(err)
	}
	return r, nil
}

// ListProjectReviews returns every review of a project; anchor-scoped
// visibility filtering happens in the server layer (it needs the caller).
func (d *DB) ListProjectReviews(ctx context.Context, projectID int64) ([]Review, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, anchor, title, author_id, base_ref, source_ref, status, created_at, updated_at
		FROM reviews WHERE project_id=? ORDER BY id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.ID, &r.Anchor, &r.Title, &r.AuthorID, &r.BaseRef, &r.SourceRef, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.ProjectID = projectID
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SetReviewStatus(ctx context.Context, id int64, status string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE reviews SET status=?, updated_at=? WHERE id=?`, status, time.Now().Unix(), id)
	return err
}

func (d *DB) AddReviewEvent(ctx context.Context, reviewID, authorID int64, kind, body string) (ReviewEvent, error) {
	if len(body) > 8000 {
		return ReviewEvent{}, fmt.Errorf("%w: comment ≤ 8000 chars", ErrInvalid)
	}
	now := time.Now().Unix()
	res, err := d.sql.ExecContext(ctx,
		`INSERT INTO review_events(review_id, kind, author_id, body, created_at) VALUES (?,?,?,?,?)`,
		reviewID, kind, authorID, body, now)
	if err != nil {
		return ReviewEvent{}, err
	}
	id, _ := res.LastInsertId()
	if _, err := d.sql.ExecContext(ctx, `UPDATE reviews SET updated_at=? WHERE id=?`, now, reviewID); err != nil {
		return ReviewEvent{}, err
	}
	return ReviewEvent{ID: id, ReviewID: reviewID, Kind: kind, AuthorID: authorID, Body: body, CreatedAt: now}, nil
}

func (d *DB) ListReviewEvents(ctx context.Context, reviewID int64) ([]ReviewEvent, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT e.id, e.review_id, e.kind, e.author_id, u.username, e.body, e.created_at
		FROM review_events e LEFT JOIN users u ON u.id=e.author_id
		WHERE e.review_id=? ORDER BY e.id`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewEvent
	for rows.Next() {
		var e ReviewEvent
		if err := rows.Scan(&e.ID, &e.ReviewID, &e.Kind, &e.AuthorID, &e.Username, &e.Body, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
