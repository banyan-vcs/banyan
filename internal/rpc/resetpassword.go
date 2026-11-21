package rpc

// Administrator password reset and the must-change fence
// (docs/gateway/authorization.md §2a): POST /api/v1/auth/reset-password
// rewrites one table account with a fresh random password (answered to
// the admin exactly once) and raises must_change_password; the console
// then walks the user through choosing their own. The fence also reaches
// the persistent-credential mint faces — a reset-but-unchanged account
// must not mint a PAT or plant an SSH key (that would outlive the forced
// change); git itself and the CLI session stay open, they are the random
// password's own reach, not something longer-lived.
import (
	"net/http"
	"strings"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
)

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "POST"}})
		return
	}
	if !s.requireAdmin(w, r, "") {
		return
	}
	us, ok := s.Meta.(meta.UserStore)
	if !ok {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "the account table is not available (this deployment fronts another authenticator)"}})
		return
	}
	var req struct {
		User string `json:"user"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.User) == "" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "user required"}})
		return
	}
	u, err := us.UserByName(r.Context(), req.User)
	if err != nil {
		// Config-file accounts and unknown names are both "not in the
		// table" — the admin cannot reset what this server does not own.
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "no server-managed account named " + req.User}})
		return
	}
	plain := auth.RandomPassword()
	hash, err := auth.HashPassword(plain)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errEnvelope{Error: errBody{Code: "INTERNAL", Message: err.Error()}})
		return
	}
	u.Password = hash
	u.MustChangePassword = true // the admin's reset forces one self-chosen password
	if err := us.PutUser(r.Context(), u); err != nil {
		writeServiceErr(w, err)
		return
	}
	// The user's old password dies now, not at cache-lapse — the same
	// immediate-rotation semantics as self-service.
	if inv, ok := s.Auth.(auth.CredentialInvalidator); ok {
		inv.InvalidateCredentials()
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": u.Name, "password": plain})
}

// handleWhoami: GET /api/v1/auth/whoami — the authenticated identity plus
// the must-change fence bit (the web tier folds this into /api/session so
// the console can force the password page).
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET"}})
		return
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() {
		w.Header().Set("WWW-Authenticate", `Basic realm="banyan"`)
		writeJSON(w, http.StatusUnauthorized, errEnvelope{Error: errBody{Code: "UNAUTHORIZED", Message: "authentication required"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": id.User, "mustChange": s.mustChangeOf(r, id.User)})
}

// mustChangeOf answers whether the table account still owes a self-chosen
// password. Unknown names (config-file accounts) never owe one.
func (s *Server) mustChangeOf(r *http.Request, user string) bool {
	us, ok := s.Meta.(meta.UserStore)
	if !ok {
		return false
	}
	u, err := us.UserByName(r.Context(), user)
	if err != nil {
		return false
	}
	return u.MustChangePassword
}

// requireNoPasswordReset is the mint-face fence: an account whose
// password an administrator reset must choose its own before it may mint
// anything longer-lived than itself (PATs, SSH keys).
func (s *Server) requireNoPasswordReset(w http.ResponseWriter, r *http.Request) bool {
	if s.Auth == nil {
		return true
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() || !s.mustChangeOf(r, id.User) {
		return true
	}
	writeJSON(w, http.StatusForbidden, errEnvelope{Error: errBody{Code: "PASSWORD_CHANGE_REQUIRED", Message: "your password was reset by an administrator — choose a new one (Settings → Profile) before minting credentials"}})
	return false
}
