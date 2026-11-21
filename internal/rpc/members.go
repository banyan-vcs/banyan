// members.go — the member management API face (docs/design/data-model.md
// §16): /api/v1/groups/<name>/members (the group's table — what labeled
// repositories inherit) and /api/v1/repos/<repo>/members (the direct table
// plus the effective view, inherited rows carrying viaGroup). Reads answer
// any authenticated member (organizational metadata, the groups precedent);
// writes ride the repo-creation gate (write rank) — the member ledger is
// the management plane the deployment's admins own regardless.
package rpc

import (
	"net/http"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func (s *Server) memberStore() meta.MemberStore {
	ms, _ := s.Meta.(meta.MemberStore)
	return ms
}

func membersUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "member management not available on this deployment's metadata plane"}})
}

// memberBody is the PUT shape for both tables: rank "" removes the member.
type memberBody struct {
	User string `json:"user"`
	Rank string `json:"rank"`
}

// handleGroupMembers serves /api/v1/groups/<name>/members: GET lists,
// PUT {user, rank} sets (rank "" removes).
func (s *Server) handleGroupMembers(w http.ResponseWriter, r *http.Request, group string) {
	mstore := s.memberStore()
	if mstore == nil {
		membersUnavailable(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		members, err := mstore.GroupMembers(r.Context(), group)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		if members == nil {
			members = []meta.Member{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": group, "members": members})
	case http.MethodPut:
		var req memberBody
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
		if err := mstore.SetGroupMember(r.Context(), group, meta.Member{
			User: req.User, Rank: req.Rank, AddedBy: user, AddedAt: time.Now().Unix(),
		}); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"group": group, "user": req.User, "rank": req.Rank})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or PUT"}})
	}
}

// handleRepoMembers serves /api/v1/repos/<repo>/members: GET answers the
// effective membership (direct plus inherited, viaGroup marking the
// inheritance) alongside the direct-only table, PUT {user, rank} sets a
// DIRECT member (rank "" removes; the inheritance guard bounces grants not
// strictly above the inherited one).
func (s *Server) handleRepoMembers(w http.ResponseWriter, r *http.Request, repo string) {
	mstore := s.memberStore()
	if mstore == nil {
		membersUnavailable(w)
		return
	}
	if _, err := s.Meta.GetRepo(r.Context(), repo); err != nil {
		writeServiceErr(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		effective, err := mstore.RepoEffectiveMembers(r.Context(), repo)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		direct, err := mstore.RepoMembers(r.Context(), repo)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		if effective == nil {
			effective = []meta.EffectiveMember{}
		}
		if direct == nil {
			direct = []meta.Member{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "members": effective, "direct": direct})
	case http.MethodPut:
		var req memberBody
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
		if err := mstore.SetRepoMember(r.Context(), repo, meta.Member{
			User: req.User, Rank: req.Rank, AddedBy: user, AddedAt: time.Now().Unix(),
		}); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"repo": repo, "user": req.User, "rank": req.Rank})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or PUT"}})
	}
}
