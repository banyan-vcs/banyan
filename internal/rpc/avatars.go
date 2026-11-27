// avatars.go — the account-avatar face: PUT /api/v1/me/avatar (self-service,
// the profile page's upload) and GET /api/v1/users/<name>/avatar (the
// console's per-name lookup — one small route, cached client-side, instead
// of fattening every listing with a column the listings never filter on).
// The string is a data:image/ or https:// URL capped at meta.MaxAvatarURL
// (the store validates); empty = the name-initial default the console
// renders, so clearing is PUT "".
package rpc

import (
	"net/http"
	"strings"

	"github.com/banyan-vcs/banyan/internal/meta"
)

func (s *Server) handleMyAvatar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "PUT"}})
		return
	}
	user, ok := s.actingUser(w, r)
	if !ok {
		return
	}
	var req struct {
		AvatarURL string `json:"avatarUrl"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	// Shape-check before the store round-trip so a bogus scheme answers
	// INVALID_ARG, not the users seam's generic refusal.
	if req.AvatarURL != "" && !strings.HasPrefix(req.AvatarURL, "data:image/") && !strings.HasPrefix(req.AvatarURL, "https://") {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "avatar must be a data:image/ or https:// URL (or empty to clear)"}})
		return
	}
	if len(req.AvatarURL) > meta.MaxAvatarURL {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "avatar too large — resize it smaller (a 64px round-trip is plenty)"}})
		return
	}
	us, ok := s.Meta.(meta.UserStore)
	if !ok {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NO_STORE", Message: "accounts not available on this deployment's metadata plane"}})
		return
	}
	u, err := us.UserByName(r.Context(), user)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	u.AvatarURL = req.AvatarURL
	if err := us.PutUser(r.Context(), u); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": user, "avatarUrl": req.AvatarURL})
}

// handleUserAvatar serves GET /api/v1/users/<name>/avatar behind the read
// posture the other listings keep (authenticated on an authed plane).
func (s *Server) handleUserAvatar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET"}})
		return
	}
	if !s.actingRead(w, r) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
	name = strings.TrimSuffix(name, "/avatar")
	if name == "" || strings.Contains(name, "/") {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "want /api/v1/users/<name>/avatar"}})
		return
	}
	us, ok := s.Meta.(meta.UserStore)
	if !ok {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NO_STORE", Message: "accounts not available on this deployment's metadata plane"}})
		return
	}
	u, err := us.UserByName(r.Context(), name)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": name, "avatarUrl": u.AvatarURL})
}
