package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
)

// The review API (docs/17 §review): everything lives under
// /api/v1/repos/{g}/{p}[/path]/reviews. The anchor is the URL's path prefix
// — a review at services/auth belongs to that subproject alone; visibility
// and verdict rights derive from the same effective-role chain as git
// access, which is what makes sibling subprojects' reviews invisible to
// each other's non-members by construction.

func (s *Server) handleReviews(w http.ResponseWriter, r *http.Request, caller User, group, project, subPath string, tail []string) {
	ctx := r.Context()
	loc, err := s.DB.Resolve(ctx, group, project, subPath)
	if err != nil {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such project")
		return
	}

	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listReviews(w, r, caller, loc)
		case http.MethodPost:
			s.createReview(w, r, caller, loc, group, project)
		default:
			apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET/POST")
		}
		return
	}

	id, err := strconv.ParseInt(tail[0], 10, 64)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", "review id expected")
		return
	}
	rev, err := s.DB.ReviewByID(ctx, id)
	if err != nil || rev.ProjectID != loc.Project.ID {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such review")
		return
	}
	// The review's own anchor decides access — a caller reaching a review
	// through a shallower URL still needs read there, which the anchor
	// closure already guarantees; a caller without it must not learn the
	// review exists.
	if !s.canReadReview(ctx, caller, loc, rev) {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such review")
		return
	}

	if len(tail) == 1 {
		if r.Method != http.MethodGet {
			apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET")
			return
		}
		s.reviewDetail(w, r, rev)
		return
	}
	switch tail[1] {
	case "diff":
		if r.Method != http.MethodGet {
			apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET")
			return
		}
		s.reviewDiff(w, r, loc, rev)
	case "comments":
		if r.Method != http.MethodPost {
			apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "POST")
			return
		}
		s.reviewComment(w, r, caller, rev)
	case "approve", "reject", "close":
		if r.Method != http.MethodPost {
			apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "POST")
			return
		}
		s.reviewTransition(w, r, caller, loc, rev, tail[1])
	default:
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "want /reviews/<id>/(diff|comments|approve|reject|close)")
	}
}

// canReadReview: reporter+ at the review's anchor (the caller got here via
// some anchor of the same project; the review's anchor must be within the
// caller's reach).
func (s *Server) canReadReview(ctx context.Context, caller User, loc Location, rev Review) bool {
	if !isSubPrefix(loc.Sub, rev.Anchor) {
		return false // the review lives outside this URL's subtree
	}
	revLoc := Location{Group: loc.Group, Project: loc.Project, Sub: rev.Anchor}
	role, err := s.DB.EffectiveRole(ctx, caller, revLoc)
	if err != nil || role == "" {
		return false
	}
	return true
}

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request, caller User, loc Location) {
	if !requireAuth(w, r, caller) {
		return
	}
	role, err := s.DB.EffectiveRole(r.Context(), caller, loc)
	if err != nil || role == "" {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such project")
		return
	}
	all, err := s.DB.ListProjectReviews(r.Context(), loc.Project.ID)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	out := []map[string]any{}
	for _, rev := range all {
		// Listing at anchor A shows reviews anchored within A's subtree —
		// A's readers can read everything below it anyway (anchor closure).
		if !isSubPrefix(loc.Sub, rev.Anchor) {
			continue
		}
		out = append(out, reviewSummary(rev, s.usernameOf(r, rev.AuthorID)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"anchor": loc.Sub, "reviews": out})
}

// reviewSummary is the listing/detail projection shared by both views.
func reviewSummary(rev Review, author string) map[string]any {
	return map[string]any{
		"id": rev.ID, "anchor": rev.Anchor, "title": rev.Title, "author": author,
		"base_ref": rev.BaseRef, "source_ref": rev.SourceRef,
		"status": rev.Status, "created_at": rev.CreatedAt, "updated_at": rev.UpdatedAt,
	}
}

func (s *Server) usernameOf(r *http.Request, userID int64) string {
	ctx := r.Context()
	users, err := s.DB.ListUsers(ctx)
	if err != nil {
		return ""
	}
	for _, u := range users {
		if u.ID == userID {
			return u.Username
		}
	}
	return ""
}

func (s *Server) createReview(w http.ResponseWriter, r *http.Request, caller User, loc Location, group, project string) {
	if !requireAuth(w, r, caller) {
		return
	}
	ctx := r.Context()
	// Authoring a review means proposing a change: push access at the
	// anchor is the gate.
	ok, err := s.DB.CanPush(ctx, caller, loc)
	if err != nil || !ok {
		apiErr(w, http.StatusForbidden, "FORBIDDEN", "developer+ at the anchor required")
		return
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		BaseRef     string `json:"base_ref"`
		SourceRef   string `json:"source_ref"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BaseRef == "" {
		req.BaseRef = "main"
	}
	// Validate both refs against the anchored view: the source must exist.
	diff, err := s.gatewayDiff(ctx, loc, req.BaseRef, req.SourceRef)
	if err != nil {
		apiErr(w, http.StatusBadGateway, "UPSTREAM", "diff probe: "+err.Error())
		return
	}
	if diff.HeadHead == "" {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", "source ref not found: "+req.SourceRef)
		return
	}
	rev, err := s.DB.CreateReview(ctx, Review{
		ProjectID: loc.Project.ID, Anchor: loc.Sub, Title: req.Title,
		Description: req.Description, AuthorID: caller.ID,
		BaseRef: req.BaseRef, SourceRef: req.SourceRef,
	})
	if err != nil {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, reviewSummary(rev, caller.Username))
}

func (s *Server) reviewDetail(w http.ResponseWriter, r *http.Request, rev Review) {
	events, err := s.DB.ListReviewEvents(r.Context(), rev.ID)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	detail := reviewSummary(rev, s.usernameOf(r, rev.AuthorID))
	detail["description"] = rev.Description
	evs := make([]map[string]any, 0, len(events))
	for _, e := range events {
		evs = append(evs, map[string]any{
			"kind": e.Kind, "user": e.Username, "body": e.Body, "created_at": e.CreatedAt,
		})
	}
	detail["events"] = evs
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) reviewComment(w http.ResponseWriter, r *http.Request, caller User, rev Review) {
	if !requireAuth(w, r, caller) {
		return
	}
	if rev.Status == ReviewClosed {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", "review is closed")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", "body required")
		return
	}
	if _, err := s.DB.AddReviewEvent(r.Context(), rev.ID, caller.ID, "comment", req.Body); err != nil {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"ok": "commented"})
}

func (s *Server) reviewTransition(w http.ResponseWriter, r *http.Request, caller User, loc Location, rev Review, action string) {
	if !requireAuth(w, r, caller) {
		return
	}
	ctx := r.Context()
	revLoc := Location{Group: loc.Group, Project: loc.Project, Sub: rev.Anchor}
	role, err := s.DB.EffectiveRole(ctx, caller, revLoc)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	switch action {
	case "approve", "reject":
		if rev.Status != ReviewOpen {
			apiErr(w, http.StatusBadRequest, "INVALID_ARG", "review is "+rev.Status)
			return
		}
		if role.Rank() < RoleDeveloper.Rank() {
			apiErr(w, http.StatusForbidden, "FORBIDDEN", "developer+ at the anchor required")
			return
		}
		if rev.AuthorID == caller.ID {
			apiErr(w, http.StatusForbidden, "FORBIDDEN", "the author cannot review their own change")
			return
		}
		status := ReviewApproved
		if action == "reject" {
			status = ReviewRejected
		}
		if err := s.DB.SetReviewStatus(ctx, rev.ID, status); err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		if _, err := s.DB.AddReviewEvent(ctx, rev.ID, caller.ID, action, ""); err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
	case "close":
		if rev.Status == ReviewClosed {
			apiErr(w, http.StatusBadRequest, "INVALID_ARG", "already closed")
			return
		}
		if rev.AuthorID != caller.ID && role.Rank() < RoleOwner.Rank() {
			apiErr(w, http.StatusForbidden, "FORBIDDEN", "the author or an owner may close")
			return
		}
		if err := s.DB.SetReviewStatus(ctx, rev.ID, ReviewClosed); err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		if _, err := s.DB.AddReviewEvent(ctx, rev.ID, caller.ID, "close", ""); err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": ReviewClosed})
	default:
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "unknown action")
	}
}

// --- the diff view ---

// diffFile is one file of a review diff, with hunks rendered for text
// changes within the caps (binary/oversized files carry a placeholder).
type diffFile struct {
	Path     string `json:"path"`
	Status   string `json:"status"`
	BaseOID  string `json:"base_oid,omitempty"`
	HeadOID  string `json:"head_oid,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`
	Hunks    []Hunk `json:"hunks,omitempty"`
}

type upstreamDiff struct {
	Base     string `json:"base"`
	Head     string `json:"head"`
	BaseHead string `json:"base_head"`
	HeadHead string `json:"head_head"`
	Files    []struct {
		Path    string `json:"path"`
		Status  string `json:"status"`
		BaseOID string `json:"base_oid"`
		HeadOID string `json:"head_oid"`
	} `json:"files"`
}

const reviewDiffMaxFiles = 200

func (s *Server) reviewDiff(w http.ResponseWriter, r *http.Request, loc Location, rev Review) {
	ctx := r.Context()
	up, err := s.gatewayDiff(ctx, loc, rev.BaseRef, rev.SourceRef)
	if err != nil {
		apiErr(w, http.StatusBadGateway, "UPSTREAM", err.Error())
		return
	}
	files := make([]diffFile, 0, len(up.Files))
	rendered := 0
	for _, f := range up.Files {
		df := diffFile{Path: f.Path, Status: f.Status, BaseOID: f.BaseOID, HeadOID: f.HeadOID}
		if rendered < reviewDiffMaxFiles {
			var base, head []byte
			switch f.Status {
			case "modified":
				base = s.gatewayBlob(ctx, loc, f.BaseOID)
				head = s.gatewayBlob(ctx, loc, f.HeadOID)
			case "added":
				head = s.gatewayBlob(ctx, loc, f.HeadOID)
			case "deleted":
				base = s.gatewayBlob(ctx, loc, f.BaseOID)
			}
			if base != nil || head != nil {
				if hunks, ok := RenderUnifiedDiff(base, head); ok {
					df.Hunks = hunks
					rendered++
				} else {
					df.Binary = IsBinary(base) || IsBinary(head)
					df.TooLarge = !df.Binary
				}
			}
		}
		files = append(files, df)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": rev.ID, "base": up.Base, "head": up.Head,
		"base_head": up.BaseHead, "head_head": up.HeadHead, "files": files,
	})
}

// gatewayDiff asks the service layer for the anchored path-level diff.
func (s *Server) gatewayDiff(ctx context.Context, loc Location, base, head string) (*upstreamDiff, error) {
	body := fmt.Sprintf(`{"repo":%q,"sub":%q,"base":%q,"head":%q}`,
		loc.Project.RepoUID, "/"+loc.Sub, base, head)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Gateway+"/api/v1/diff", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.IdentityHeaderName, auth.SignIdentity(s.Secret, "console", time.Now()))
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out upstreamDiff
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// gatewayBlob fetches one blob's raw content (nil on any failure — the
// diff renderer then degrades that file to placeholder).
func (s *Server) gatewayBlob(ctx context.Context, loc Location, oid string) []byte {
	if oid == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.Gateway+"/api/v1/objects?repo="+loc.Project.RepoUID+"&oid="+oid, nil)
	if err != nil {
		return nil
	}
	req.Header.Set(auth.IdentityHeaderName, auth.SignIdentity(s.Secret, "console", time.Now()))
	resp, err := s.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, diffMaxContent+1))
	if err != nil {
		return nil
	}
	return data
}
