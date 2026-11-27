// mrs.go — the merge-request API face (docs/design/merge-requests.md):
// /api/v1/repos/<repo>/merge-requests[/<iid>[/<action>]], the branch
// protection rules at /api/v1/repos/<repo>/branch-rules[/<branch>], and
// the workspace-wide list at /api/v1/merge-requests (the console sidebar's
// cross-repo view). Read routes answer any authenticated caller; every
// mutation needs push rank, and the merge action additionally mirrors the
// target rule's direct-push semantics (a protected target merges at owner
// rank, the exact permission a direct push would demand). The mr-only
// level takes no extra rank gate here by design: its gate IS the approval
// count, enforced in the service.
package rpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// actingUser answers the caller's username. The nil-policy dev plane has
// no identities — "local" stands in (the same shape the dev-mode push path
// serves); anonymous callers on an authed plane are rejected.
func (s *Server) actingUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.Auth == nil {
		return "local", true
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() {
		writeJSON(w, http.StatusUnauthorized, errEnvelope{Error: errBody{Code: "UNAUTHENTICATED", Message: "login first"}})
		return "", false
	}
	return id.User, true
}

// mrOnly reports whether the branch carries an mr-only rule — shared by
// the commit and ref-CAS endpoints with the git wire's receive-pack gate
// (nobody pushes an mr-only branch directly, docs/design/merge-requests.md).
func (s *Server) mrOnly(ctx context.Context, repo, branch string) bool {
	rs, ok := s.Meta.(meta.BranchRuleStore)
	if !ok || branch == "" {
		return false
	}
	rule, has, err := rs.BranchRuleOf(ctx, repo, branch)
	return err == nil && has && rule.Level == meta.RuleMROnly
}

// handleMergeRequests serves the repo's MR collection and per-request
// actions. parts is the path after /api/v1/repos/ (["<repo>",
// "merge-requests", ...]).
// rejectMROnly answers the 403 every direct write to an mr-only branch gets.
func (s *Server) rejectMROnly(w http.ResponseWriter, branch string) {
	writeJSON(w, http.StatusForbidden, errEnvelope{Error: errBody{
		Code: "FORBIDDEN", Message: branch + " is mr-only: merge via a merge request"}})
}

// handleMergeRequestsAll serves GET /api/v1/merge-requests — the
// workspace-wide list behind the console sidebar's merge-requests item:
// every request across the repos the caller can see, newest update first.
// Ledger fields only — mergeability is a per-request computation (merge
// base + tree merge) and stays on the detail page.
func (s *Server) handleMergeRequestsAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET"}})
		return
	}
	st, ok := s.Meta.(meta.MRStore)
	if !ok {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "merge requests not available on this deployment's metadata plane"}})
		return
	}
	repos, err := s.Svc.ListRepos(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	state := r.URL.Query().Get("state")
	out := []meta.MRInfo{}
	for _, repo := range repos {
		mrs, err := st.ListMRs(r.Context(), repo.ID, state)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		out = append(out, mrs...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	writeJSON(w, http.StatusOK, map[string]any{"merge_requests": out})
}

func (s *Server) handleMergeRequests(w http.ResponseWriter, r *http.Request, parts []string) {
	repo := parts[0]
	rest := parts[2:] // iid / action segments; empty = the collection
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet: // list
		mrs, err := s.Svc.ListMRs(r.Context(), repo, r.URL.Query().Get("state"))
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "merge_requests": mrs})

	case len(rest) == 0 && r.Method == http.MethodPost: // open
		user, ok := s.actingUser(w, r)
		if !ok {
			return
		}
		if !s.requirePush(w, r, repo, "", "") {
			return
		}
		var req struct {
			Source      string `json:"source"`
			Target      string `json:"target"`
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		mr, err := s.Svc.OpenMR(r.Context(), repo, req.Source, req.Target, req.Title, req.Description, user)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, mr)

	case len(rest) == 1 && r.Method == http.MethodGet: // detail
		iid, ok := parseIid(w, rest[0])
		if !ok {
			return
		}
		view, err := s.Svc.MRDetail(r.Context(), repo, iid)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)

	case len(rest) == 2 && rest[1] == "approve" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
		user, ok := s.actingUser(w, r)
		if !ok {
			return
		}
		if !s.requirePush(w, r, repo, "", "") {
			return
		}
		iid, ok := parseIid(w, rest[0])
		if !ok {
			return
		}
		if r.Method == http.MethodPost {
			if err := s.Svc.ApproveMR(r.Context(), repo, iid, user, time.Now().Unix()); err != nil {
				writeServiceErr(w, err)
				return
			}
		} else if err := s.Svc.UnapproveMR(r.Context(), repo, iid, user); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case len(rest) == 2 && rest[1] == "comments" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		// The discussion ledger (docs/design/merge-requests.md §review):
		// GET answers the timeline's data set (ledger + timestamped
		// approvals) behind the detail route's read posture; POST appends a
		// comment or reply at push rank — the same floor the MR's own
		// creation demands.
		iid, ok := parseIid(w, rest[0])
		if !ok {
			return
		}
		if r.Method == http.MethodGet {
			comments, approvals, err := s.Svc.MRDiscussion(r.Context(), repo, iid)
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			if comments == nil {
				comments = []meta.MRComment{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"comments": comments, "approval_events": approvals})
			return
		}
		user, ok := s.actingUser(w, r)
		if !ok {
			return
		}
		if !s.requirePush(w, r, repo, "", "") {
			return
		}
		var req struct {
			Body     string `json:"body"`
			Path     string `json:"path"`
			Side     string `json:"side"`
			Line     int    `json:"line"`
			ParentID int64  `json:"parent_id"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		c, err := s.Svc.AddMRComment(r.Context(), repo, iid, user, req.Body, req.Path, req.Side, req.Line, req.ParentID)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, c)

	case len(rest) == 4 && rest[1] == "comments" && rest[3] == "resolve" && r.Method == http.MethodPost:
		// Resolve/unresolve a thread head (replies refuse in the service).
		user, ok := s.actingUser(w, r)
		if !ok {
			return
		}
		if !s.requirePush(w, r, repo, "", "") {
			return
		}
		iid, ok := parseIid(w, rest[0])
		if !ok {
			return
		}
		id, err := strconv.ParseInt(rest[2], 10, 64)
		if err != nil || id <= 0 {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "comment id must be a positive integer"}})
			return
		}
		var req struct {
			Resolved bool `json:"resolved"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if err := s.Svc.ResolveMRComment(r.Context(), repo, iid, id, user, req.Resolved); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case len(rest) == 2 && rest[1] == "review" && r.Method == http.MethodPost:
		// The conclusion bar's submission: verdict approve|request-changes|
		// comment plus an optional body. Push rank here too — a verdict is
		// a stronger statement than a remark.
		user, ok := s.actingUser(w, r)
		if !ok {
			return
		}
		if !s.requirePush(w, r, repo, "", "") {
			return
		}
		iid, ok := parseIid(w, rest[0])
		if !ok {
			return
		}
		var req struct {
			Verdict string `json:"verdict"`
			Body    string `json:"body"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		c, err := s.Svc.SubmitMRReview(r.Context(), repo, iid, user, req.Verdict, req.Body)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, c)

	case len(rest) == 2 && rest[1] == "merge" && r.Method == http.MethodPost:
		user, ok := s.actingUser(w, r)
		if !ok {
			return
		}
		iid, ok := parseIid(w, rest[0])
		if !ok {
			return
		}
		// The merge gate mirrors the direct-push semantics of whatever
		// protects the target — with one precedence: an mr-only rule
		// SUPERSEDES other protection at the merge path (its declared way
		// in is the MR with N approvals; demanding owner rank on top would
		// make mr-only meaningless under a default-protected main). Every
		// other case takes the branch-aware CanPush: owner rank on
		// snapshot- or rule-protected targets, ordinary rank otherwise.
		target, err := s.Svc.MRTarget(r.Context(), repo, iid)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		gate := target
		if rs, ok := s.Meta.(meta.BranchRuleStore); ok {
			if rule, has, rerr := rs.BranchRuleOf(r.Context(), repo, target); rerr == nil && has && rule.Level == meta.RuleMROnly {
				gate = "" // ordinary push rank; the approval count is the gate
			}
		}
		if !s.requirePush(w, r, repo, "", gate) {
			return
		}
		var req struct {
			Message string `json:"message"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) // optional body
		}
		mr, err := s.Svc.MergeMR(r.Context(), repo, iid, user, req.Message, time.Now().Unix())
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, mr)

	case len(rest) == 2 && (rest[1] == "close" || rest[1] == "reopen") && r.Method == http.MethodPost:
		if !s.requirePush(w, r, repo, "", "") {
			return
		}
		iid, ok := parseIid(w, rest[0])
		if !ok {
			return
		}
		var mr meta.MRInfo
		var err error
		if rest[1] == "close" {
			mr, err = s.Svc.CloseMR(r.Context(), repo, iid, time.Now().Unix())
		} else {
			mr, err = s.Svc.ReopenMR(r.Context(), repo, iid, time.Now().Unix())
		}
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, mr)

	default:
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND",
			Message: "want /repos/<repo>/merge-requests[/<iid>[/approve|merge|close|reopen|comments[/<id>/resolve]|review]]"}})
	}
}

// handleBranchRules serves the protection rules: any authenticated member
// reads (the settings page), admins write (the placement/force-push gate).
func (s *Server) handleBranchRules(w http.ResponseWriter, r *http.Request, parts []string) {
	repo := parts[0]
	rs, ok := s.Meta.(meta.BranchRuleStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, errEnvelope{Error: errBody{Code: "NO_STORE", Message: "branch rules not available"}})
		return
	}
	if !s.actingRead(w, r) {
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		rules, err := rs.BranchRules(r.Context(), repo)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "rules": rules})
	case len(parts) >= 3 && r.Method == http.MethodPut:
		if !s.requireAdmin(w, r, repo) {
			return
		}
		// Everything after the marker is the branch name — a name may carry
		// slashes (release/v2), and no other sub-route lives below, so the
		// whole tail addresses the branch unambiguously.
		branch := strings.Join(parts[2:], "/")
		var req struct {
			Level     string `json:"level"`
			Approvals int    `json:"approvals"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		approvals := req.Approvals
		if approvals == 0 && req.Level == meta.RuleMROnly {
			approvals = 1 // the mr-only default when the caller omits it
		}
		if err := rs.PutBranchRule(r.Context(), meta.BranchRule{Repo: repo, Branch: branch, Level: req.Level, Approvals: approvals}); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, meta.BranchRule{Repo: repo, Branch: branch, Level: req.Level, Approvals: approvals})
	case len(parts) >= 3 && r.Method == http.MethodDelete:
		if !s.requireAdmin(w, r, repo) {
			return
		}
		if err := rs.DeleteBranchRule(r.Context(), repo, strings.Join(parts[2:], "/")); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "want /repos/<repo>/branch-rules[/<branch>]"}})
	}
}

// actingRead gates the read routes: authenticated on an authed plane, open
// on the dev plane (the nil-policy behavior every read route keeps).
func (s *Server) actingRead(w http.ResponseWriter, r *http.Request) bool {
	if s.Auth == nil {
		return true
	}
	if s.Auth.Identify(r).Anonymous() {
		writeJSON(w, http.StatusUnauthorized, errEnvelope{Error: errBody{Code: "UNAUTHENTICATED", Message: "login first"}})
		return false
	}
	return true
}

func parseIid(w http.ResponseWriter, s string) (int64, bool) {
	iid, err := strconv.ParseInt(s, 10, 64)
	if err != nil || iid <= 0 {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "merge request id must be a positive integer"}})
		return 0, false
	}
	return iid, true
}
