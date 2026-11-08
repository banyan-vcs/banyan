// RPC-layer authorization (docs/07 §5, docs/14 M5): the JSON surface keeps
// its authentication gate on every /api/v1 route, and management/write
// operations additionally consult the policy — repo/cluster management
// through the optional auth.AdminChecker extension, content writes (commit,
// ref CAS) through the same CanPush the git wire enforces. Policies without
// AdminChecker (the remote snapshot, gateway deployments) keep the plain
// authenticated behavior; the token management endpoints live here too.
package rpc

import (
	"net/http"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
)

// requireAdmin answers whether the caller may manage repo ("" = global:
// cluster and metadata-plane operations). Without the AdminChecker
// extension the authenticated-only behavior stands (docs/07 §5).
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request, repo string) bool {
	if s.Auth == nil {
		return true
	}
	ac, ok := s.Auth.(auth.AdminChecker)
	if !ok {
		return true
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() || !ac.CanAdmin(id, repo) {
		writeJSON(w, http.StatusForbidden, errEnvelope{Error: errBody{Code: "FORBIDDEN", Message: "management requires admin"}})
		return false
	}
	return true
}

// requirePush gates content writes with the same rule the git wire applies
// (scope ceiling included, when the policy is the token decorator).
func (s *Server) requirePush(w http.ResponseWriter, r *http.Request, repo, sub, branch string) bool {
	if s.Auth == nil {
		return true
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() || !s.Auth.CanPush(id, repo, sub, branch) {
		writeJSON(w, http.StatusForbidden, errEnvelope{Error: errBody{Code: "FORBIDDEN", Message: "push permission required"}})
		return false
	}
	return true
}

// requireWriteRank gates repository creation: a metadata-plane write, so
// scoped credentials need write rank. Unscoped identities (password
// logins, the console's signed service identity) keep the authenticated
// gate — the console topology authorizes product-level creation on its own
// side, and the not-yet-existing repo is unknown to the gateway policy by
// construction, so a per-repo CanPush would fail-closed the whole flow.
func (s *Server) requireWriteRank(w http.ResponseWriter, r *http.Request) bool {
	if s.Auth == nil {
		return true
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() || (id.Scope != "" && meta.ScopeRank(id.Scope) < meta.ScopeRank(meta.ScopeWrite)) {
		writeJSON(w, http.StatusForbidden, errEnvelope{Error: errBody{Code: "FORBIDDEN", Message: "write-rank credential required"}})
		return false
	}
	return true
}

// tokenRegistry answers the metadata-plane token registry, or nil where the
// deployment has none (gateway behind an upstream console: the console's
// own PATs apply, docs/17 §4).
func (s *Server) tokenRegistry() meta.TokenRegistry {
	tr, _ := s.Meta.(meta.TokenRegistry)
	return tr
}

// handleTokens: POST /api/v1/auth/tokens (mint — the plaintext is answered
// exactly once), GET /api/v1/auth/tokens[?all=1] (own tokens, or every
// user's with admin), DELETE /api/v1/auth/tokens/<hash> (own or admin).
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	tr := s.tokenRegistry()
	if tr == nil {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "token layer not available (this deployment fronts another authenticator)"}})
		return
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() {
		writeJSON(w, http.StatusUnauthorized, errEnvelope{Error: errBody{Code: "UNAUTHORIZED", Message: "authentication required"}})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/auth/tokens")
	rest = strings.Trim(rest, "/")
	switch {
	case r.Method == http.MethodPost && rest == "":
		var req struct {
			Name        string `json:"name"`
			Scope       string `json:"scope"`
			ExpiresDays int    `json:"expires_days"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Scope == "" {
			req.Scope = meta.ScopeWrite
		}
		info := meta.TokenInfo{User: id.User, Name: req.Name, Scope: req.Scope, Created: time.Now().Unix()}
		if req.ExpiresDays > 0 {
			info.Expires = time.Now().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour).Unix()
		}
		plaintext, err := meta.NewTokenPlaintext()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errEnvelope{Error: errBody{Code: "INTERNAL", Message: err.Error()}})
			return
		}
		info.Hash = meta.HashToken(plaintext)
		if err := info.Valid(); err != nil {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: err.Error()}})
			return
		}
		if err := tr.PutToken(r.Context(), info); err != nil {
			writeJSON(w, http.StatusInternalServerError, errEnvelope{Error: errBody{Code: "INTERNAL", Message: err.Error()}})
			return
		}
		// The plaintext exists only in this response (docs/07 §6).
		writeJSON(w, http.StatusOK, map[string]any{"token": plaintext, "hash": info.Hash, "scope": info.Scope, "expires": info.Expires})
	case r.Method == http.MethodGet && rest == "":
		user := id.User
		if r.URL.Query().Get("all") == "1" {
			if !s.requireAdmin(w, r, "") {
				return
			}
			user = ""
		}
		toks, err := tr.ListTokens(r.Context(), user)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errEnvelope{Error: errBody{Code: "INTERNAL", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": toks})
	case r.Method == http.MethodDelete && rest != "":
		hash := rest
		tok, err := tr.TokenByHash(r.Context(), hash)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		if tok.User != id.User && !s.requireAdmin(w, r, "") {
			return
		}
		if err := tr.RevokeToken(r.Context(), hash); err != nil {
			writeJSON(w, http.StatusInternalServerError, errEnvelope{Error: errBody{Code: "INTERNAL", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "POST list, GET list, DELETE /<hash>"}})
	}
}
