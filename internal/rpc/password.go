// Password self-service (docs/gateway/authorization.md §2a):
// POST /api/v1/auth/password {current, next} — the caller's own account in
// the metadata-plane table. Config-file accounts answer 400 with the
// reason (their passwords are not managed here); the current credential is
// re-verified explicitly, never assumed from the session.
package rpc

import (
	"net/http"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
)

// minPasswordLen is the floor for a new password. bcrypt does the heavy
// lifting; the floor only stops the trivial.
const minPasswordLen = 8

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "POST"}})
		return
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() {
		writeJSON(w, http.StatusUnauthorized, errEnvelope{Error: errBody{Code: "UNAUTHORIZED", Message: "authentication required"}})
		return
	}
	us, ok := s.Meta.(meta.UserStore)
	if !ok {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "password self-service not available (this deployment fronts another authenticator)"}})
		return
	}
	var req struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	u, err := us.UserByName(r.Context(), id.User)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "this account's password is not managed here (it lives in the deployment's config file)"}})
		return
	}
	if !auth.CheckPassword(u.Password, req.Current) {
		writeJSON(w, http.StatusForbidden, errEnvelope{Error: errBody{Code: "INVALID_CREDENTIALS", Message: "wrong current password"}})
		return
	}
	if len(req.Next) < minPasswordLen {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "the new password wants at least 8 characters"}})
		return
	}
	hash, err := auth.HashPassword(req.Next)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errEnvelope{Error: errBody{Code: "INTERNAL", Message: err.Error()}})
		return
	}
	u.Password = hash
	u.MustChangePassword = false // a self-chosen password clears the admin-reset fence
	if err := us.PutUser(r.Context(), u); err != nil {
		writeServiceErr(w, err)
		return
	}
	// The old value dies now, not at cache-lapse: the wired policy drops
	// its verified-credential cache when it carries one (the decorators
	// forward inward; policies without caches are no-ops).
	if inv, ok := s.Auth.(auth.CredentialInvalidator); ok {
		inv.InvalidateCredentials()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
