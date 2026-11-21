// groups.go — the groups API face (docs/design/data-model.md §15):
// /api/v1/groups (list with the assignment table, create) and
// /api/v1/groups/<name> (delete). The per-repo label lives with the repo
// surface at /api/v1/repos/<repo>/group (handled in server.go's repo sub).
// Groups are organizational metadata only — they rename nothing and gate
// nothing — so creation and the label move ride the repo-creation gate
// (write rank), and reads answer any authenticated member.
package rpc

import (
	"net/http"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func (s *Server) groupStore() meta.GroupStore {
	gs, _ := s.Meta.(meta.GroupStore)
	return gs
}

// handleGroups serves the group collection.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	gs := s.groupStore()
	if gs == nil {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "groups not available on this deployment's metadata plane"}})
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		user, ok := s.actingUser(w, r)
		if !ok {
			return
		}
		if !s.requireWriteRank(w, r) {
			return
		}
		if err := gs.CreateGroup(r.Context(), meta.GroupInfo{
			Name: req.Name, Description: req.Description, CreatedBy: user, CreatedAt: time.Now().Unix(),
		}); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": req.Name})
	default:
		groups, err := gs.ListGroups(r.Context())
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		assignments, err := gs.RepoGroups(r.Context())
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		if groups == nil {
			groups = []meta.GroupInfo{} // JSON null would crash the console (the token-layer precedent)
		}
		if assignments == nil {
			assignments = map[string]string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "repoGroups": assignments})
	}
}

// handleRepoGroup serves GET/PUT /api/v1/repos/<repo>/group — the repo's
// group label. The repo must exist (404 otherwise); PUT's group must exist
// too (NOT_FOUND via the store's ErrGroupUnknown) and "" clears the label.
func (s *Server) handleRepoGroup(w http.ResponseWriter, r *http.Request, repo string) {
	gs := s.groupStore()
	if gs == nil {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "groups not available on this deployment's metadata plane"}})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if _, err := s.Meta.GetRepo(r.Context(), repo); err != nil {
			writeServiceErr(w, err)
			return
		}
		assignments, err := gs.RepoGroups(r.Context())
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"repo": repo, "group": assignments[repo]})
	case http.MethodPut:
		var req struct {
			Group string `json:"group"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if !s.requireWriteRank(w, r) {
			return
		}
		if _, err := s.Meta.GetRepo(r.Context(), repo); err != nil {
			writeServiceErr(w, err)
			return
		}
		if err := gs.SetRepoGroup(r.Context(), repo, req.Group); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"repo": repo, "group": req.Group})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or PUT"}})
	}
}

// handleGroupSub serves /api/v1/groups/<name>.
func (s *Server) handleGroupSub(w http.ResponseWriter, r *http.Request) {
	gs := s.groupStore()
	if gs == nil {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "groups not available on this deployment's metadata plane"}})
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/groups/"), "/")
	// The member table rides one segment deeper: /api/v1/groups/<name>/members.
	if rest, ok := strings.CutSuffix(name, "/members"); ok {
		if rest == "" || strings.Contains(rest, "/") {
			writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "want /api/v1/groups/<name>/members"}})
			return
		}
		s.handleGroupMembers(w, r, rest)
		return
	}
	if name == "" || strings.Contains(name, "/") {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "want /api/v1/groups/<name>"}})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if !s.requireWriteRank(w, r) {
			return
		}
		if err := gs.DeleteGroup(r.Context(), name); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "DELETE"}})
	}
}
