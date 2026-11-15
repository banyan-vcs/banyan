// cliauth.go — the CLI login approval flow (docs/gateway/authorization.md
// §6a, the no-SSH path): `banyan login` generates a one-time handshake id
// (128-bit random — the poll's only capability), the user opens /cli-auth#<id>
// in a browser, authenticates with whatever this console fronts (local
// accounts, GitHub/GitLab OAuth — the existing session machinery), approves,
// and the console relays a SHORT-LIVED gateway session token back through
// the poll. No permanent token is ever created or stored: the grant row is
// a 5-minute one-shot relay, deleted on claim.
package webapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// cliGrantTTL bounds the claim window: after approval the CLI has five
// minutes to poll the token home, then the row expires.
const cliGrantTTL = 5 * time.Minute

func (w *DB) putCLIGrant(id, token, user string, tokenExpires int64) error {
	return w.orm.Create(&webCLIGrantRow{
		HandshakeID: id, Token: token, UserName: user,
		TokenExpires: tokenExpires, ExpiresAt: unix(time.Now().Add(cliGrantTTL)),
	}).Error
}

// claimCLIGrant hands the token out exactly once and deletes the row.
func (w *DB) claimCLIGrant(id string) (token, user string, expires int64, ok bool) {
	var row webCLIGrantRow
	if err := w.orm.Where("handshake_id = ?", id).Take(&row).Error; err != nil || row.ExpiresAt < unix(time.Now()) {
		_ = w.orm.Where("handshake_id = ?", id).Delete(&webCLIGrantRow{}).Error
		return "", "", 0, false
	}
	_ = w.orm.Where("handshake_id = ?", id).Delete(&webCLIGrantRow{}).Error
	return row.Token, row.UserName, row.TokenExpires, true
}

func (w *DB) sweepCLIGrants() {
	_ = w.orm.Where("expires_at < ?", unix(time.Now())).Delete(&webCLIGrantRow{}).Error
}

// handleCLIAuthPage: GET /cli-auth — a self-contained approval page (no SPA
// build dependency). The handshake id rides the URL fragment (#id=...) so it
// never reaches any access log; anonymous visitors are pointed at the login
// first and return here.
func (s *Server) handleCLIAuthPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionOf(r); !ok {
		// Not logged in: same behavior as the SPA — land on the console,
		// which bounces to login and can find its way back here.
		http.Redirect(w, r, s.opts.BasePath+"/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8">
<title>banyan CLI 授权 / CLI authorization</title>
<style>body{font-family:system-ui,sans-serif;max-width:32em;margin:8vh auto;padding:0 1em;color:#222}
button{font:inherit;padding:.5em 1.2em;cursor:pointer}#out{margin-top:1em;white-space:pre-wrap}</style>
</head><body>
<h2>banyan CLI 请求登录凭证</h2>
<p>A banyan CLI on one of your machines asks for a short-lived session
(12h, write scope, revocable). Approving mints it for your account —
<b>`+htmlEscape(s.sessionUser(r))+`</b>.</p>
<p><button id="ok">Approve</button></p><p id="out"></p>
<script>
const id = (location.hash.match(/[#&]id=([A-Za-z0-9_-]{16,128})/)||[])[1];
const label = (location.hash.match(/[#&]l=([A-Za-z0-9._-]{1,32})/)||[])[1]||'';
const out = document.getElementById('out');
if (!id) { out.textContent = 'no handshake id in the URL — rerun "banyan login"'; }
else document.getElementById('ok').onclick = async () => {
  out.textContent = 'approving…';
  const resp = await fetch('`+s.opts.BasePath+`/api/cli/approve', {method:'POST',
    headers:{'Content-Type':'application/json'}, body: JSON.stringify({id, label})});
  out.textContent = resp.ok ? 'approved — the CLI will pick it up shortly (this page can be closed)'
                            : 'denied: ' + (await resp.text());
};
</script></body></html>`)
}

func (s *Server) sessionUser(r *http.Request) string {
	if sess, ok := s.sessionOf(r); ok {
		return sess.Username
	}
	return ""
}

func htmlEscape(v string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(v)
}

// handleCLIApprove: POST /api/cli/approve {id} — session-authenticated.
// Mints a short-lived session AT THE GATEWAY with the logged-in user's own
// injected credentials, then parks it on the handshake id for the CLI poll.
func (s *Server) handleCLIApprove(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
		return
	}
	var req struct {
		ID    string `json:"id"`
		Label string `json:"label"` // device name from the CLI's URL fragment — names the minted session
	}
	if !readJSONBody(w, r, &req) || len(req.ID) < 16 || len(req.ID) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad handshake id"})
		return
	}
	mintPayload, _ := json.Marshal(map[string]string{"label": req.Label})
	mintReq, err := http.NewRequest(http.MethodPost, s.opts.Gateway+"/api/v1/auth/cli-session", bytes.NewReader(mintPayload))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	mintReq.Header.Set("Content-Type", "application/json")
	mintReq.Header.Set("Authorization", sess.Basic())
	mint, err := s.client.Do(mintReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "gateway: " + err.Error()})
		return
	}
	defer mint.Body.Close()
	if mint.StatusCode != http.StatusOK {
		writeJSON(w, mint.StatusCode, map[string]string{"error": "gateway refused the session mint"})
		return
	}
	var granted struct {
		Token   string `json:"token"`
		User    string `json:"user"`
		Expires int64  `json:"expires"`
	}
	if err := json.NewDecoder(mint.Body).Decode(&granted); err != nil || granted.Token == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "gateway mint unreadable"})
		return
	}
	if err := s.opts.DB.putCLIGrant(req.ID, granted.Token, granted.User, granted.Expires); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": granted.User, "expires": granted.Expires})
}

// handleCLIPoll: POST /api/cli/poll {id} — the CLI's side. The handshake
// id is the grant's ONLY capability, so it travels in the request BODY:
// request lines (method+path+query) land in reverse-proxy and access logs,
// bodies do not — a logged id used to be raceable against the claim. GET
// with a query id remains for one CLI generation of compatibility. Unknown
// or unapproved ids answer "pending" (no oracle: the id is a 256-bit
// random capability); an approved id answers the token exactly once.
func (s *Server) handleCLIPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id") // legacy GET form
	if id == "" && r.Method == http.MethodPost {
		var req struct {
			ID string `json:"id"`
		}
		if !readJSONBody(w, r, &req) {
			return
		}
		id = req.ID
	}
	if len(id) < 16 || len(id) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad handshake id"})
		return
	}
	if token, user, expires, ok := s.opts.DB.claimCLIGrant(id); ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "granted", "token": token, "user": user, "expires": expires})
		return
	}
	s.opts.DB.sweepCLIGrants()
	writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
}
