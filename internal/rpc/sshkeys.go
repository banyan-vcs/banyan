// User-managed SSH keys on the JSON face (docs/gateway/authorization.md
// §7a): list, add and remove your own keys — the table the KeyPolicy
// consults at the SSH handshake. The key material arrives as ONE
// authorized_keys line ("algo base64 comment"); parsing and fingerprinting
// happen here (x/crypto), the store keeps what it is given.
package rpc

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// sshKeyStore answers the metadata-plane key table, or nil where the
// deployment has none (remote-meta gateways; the tokens precedent).
func (s *Server) sshKeyStore() meta.SSHKeyStore {
	ks, _ := s.Meta.(meta.SSHKeyStore)
	return ks
}

// handleSSHKeys: GET /api/v1/auth/ssh-keys (own keys, newest first), POST
// (add — body {key}: one authorized_keys line), DELETE ?fingerprint= (remove
// one's own). The fingerprint rides a QUERY parameter, never a path
// segment: it is base64 payload and may contain '/', which a path cannot
// carry (the tokens' hex hashes have no such trouble).
func (s *Server) handleSSHKeys(w http.ResponseWriter, r *http.Request) {
	ks := s.sshKeyStore()
	if ks == nil {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "ssh key layer not available (this deployment fronts another authenticator)"}})
		return
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() {
		writeJSON(w, http.StatusUnauthorized, errEnvelope{Error: errBody{Code: "UNAUTHORIZED", Message: "authentication required"}})
		return
	}
	switch r.Method {
	case http.MethodGet:
		keys, err := ks.SSHKeys(r.Context(), id.User)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errEnvelope{Error: errBody{Code: "INTERNAL", Message: err.Error()}})
			return
		}
		if keys == nil {
			keys = []meta.SSHKey{} // "no keys yet" is [], not null
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
	case http.MethodPost:
		var req struct {
			Key string `json:"key"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		// An SSH key is an UNSCOPED credential — it carries no token
		// ceiling — so minting one is a write-rank act (a read PAT must not
		// forge itself a full-rank key; the token-minting gate, same seam).
		if !s.requireWriteRank(w, r) {
			return
		}
		pub, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(req.Key)))
		if err != nil || len(bytes.TrimSpace(rest)) != 0 {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "not a valid public key — paste exactly one authorized_keys line (algo base64 comment)"}})
			return
		}
		key := meta.SSHKey{
			Fingerprint: ssh.FingerprintSHA256(pub),
			User:        id.User,
			Type:        pub.Type(),
			Blob:        base64.StdEncoding.EncodeToString(pub.Marshal()),
			Comment:     comment,
			AddedAt:     time.Now().Unix(),
		}
		if err := ks.AddSSHKey(r.Context(), key); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"fingerprint": key.Fingerprint, "type": key.Type, "comment": key.Comment})
	case http.MethodDelete:
		fp := r.URL.Query().Get("fingerprint")
		if fp == "" {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "fingerprint query parameter required"}})
			return
		}
		if err := ks.DeleteSSHKey(r.Context(), id.User, fp); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET list, POST add, DELETE ?fingerprint="}})
	}
}
