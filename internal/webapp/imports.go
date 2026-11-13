// imports.go — the web tier's import orchestration: the job state machine
// lives here (this is the layer with a database), the fetch itself runs on
// the gateway's synchronous /api/v1/imports. POST /api/imports answers
// immediately with a job id; a goroutine drives the gateway call on a
// long-timeout client and closes the row. Source credentials are wrapped
// per provider kind and never stored, logged or echoed.
package webapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type importCall struct {
	Name                string `json:"name,omitempty"`
	URL                 string `json:"url"`
	SourceAuthorization string `json:"source_authorization,omitempty"`
}

// sourceAuthorization derives the Authorization header for the SOURCE
// remote. Session mode rides the login provider's token (a local password
// must never be sent to a third-party host — that mode requires a pasted
// token instead); token mode wraps the pasted PAT per the URL's host.
func (s *Server) sourceAuthorization(sess Session, rawURL, mode, token string) (string, error) {
	kind := ""
	if p, ok := s.providerByHost(hostOf(rawURL)); ok {
		kind = p.Kind
	} else if strings.HasSuffix(hostOf(rawURL), "github.com") {
		kind = "github"
	} else {
		kind = "gitlab"
	}
	switch mode {
	case "", "session":
		if sess.ProviderKind == "" {
			return "", fmt.Errorf("this login carries a local password, which is never sent to a source host — paste a token instead")
		}
		return sess.Basic(), nil
	case "token":
		token = strings.TrimSpace(token)
		if token == "" {
			return "", fmt.Errorf("token mode requires a token")
		}
		// banyan's own PATs carry the console convention (docs/17 §4): the
		// Basic username must be the token's owner — the importing session's
		// user; a token minted by anyone else simply fails to authenticate.
		if strings.HasPrefix(token, "banyan_") {
			return "Basic " + base64Of(sess.Username+":"+token), nil
		}
		if kind == "github" {
			return "Basic " + base64Of("x-access-token:"+token), nil
		}
		return "Basic " + base64Of("oauth2:"+token), nil
	default:
		return "", fmt.Errorf("auth mode %q: want session|token", mode)
	}
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// providerByHost matches a configured provider whose instance host equals
// the source host.
func (s *Server) providerByHost(host string) (Provider, bool) {
	for _, p := range s.providers {
		if hostOf(p.URL) == host || hostOf(p.ExternalURL) == host {
			return p, true
		}
	}
	return Provider{}, false
}

// importClient dials the gateway for the long haul (a monorepo import is
// one synchronous call; the SPA polls /api/imports meanwhile).
func (s *Server) importClient() *http.Client {
	if s.opts.Client != nil { // tests share their transport
		return s.opts.Client
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

func (s *Server) handleImportCreate(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
		return
	}
	if s.opts.Profile == ProfileGitLab {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "imports are a standalone-console capability"})
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
		Auth struct {
			Mode  string `json:"mode"`
			Token string `json:"token"`
		} `json:"auth"`
	}
	if !readJSONBody(w, r, &req) {
		return
	}
	raw := strings.TrimSpace(req.URL)
	if !(strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url must be http(s)://"})
		return
	}
	sourceAuth, err := s.sourceAuthorization(sess, raw, req.Auth.Mode, req.Auth.Token)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = cleanRepoName(raw)
	}
	job := ImportJob{
		ID:        randomHex(12),
		Repo:      name,
		SourceURL: raw,
		Initiator: sess.Username,
		Status:    "running",
		StartedAt: time.Now().Unix(),
	}
	if err := s.opts.DB.CreateImport(job); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	go s.driveImport(job, sess, importCall{Name: name, URL: raw, SourceAuthorization: sourceAuth})
	writeJSON(w, http.StatusAccepted, map[string]string{"id": job.ID, "repo": job.Repo})
}

// driveImport performs the synchronous gateway call and closes the job row.
func (s *Server) driveImport(job ImportJob, sess Session, call importCall) {
	payload, _ := json.Marshal(call)
	req, err := http.NewRequest(http.MethodPost, s.opts.Gateway+"/api/v1/imports", bytes.NewReader(payload))
	if err != nil {
		_ = s.opts.DB.FinishImport(job.ID, "failed", err.Error(), 0, "")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", sess.Basic())
	resp, err := s.importClient().Do(req)
	if err != nil {
		_ = s.opts.DB.FinishImport(job.ID, "failed", "gateway unreachable: "+err.Error(), 0, "")
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &env)
		msg := env.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("gateway answered %s", resp.Status)
		}
		_ = s.opts.DB.FinishImport(job.ID, "failed", msg, 0, "")
		return
	}
	var out struct {
		Repo          string `json:"repo"`
		Refs          int64  `json:"refs"`
		DefaultBranch string `json:"defaultBranch"`
	}
	_ = json.Unmarshal(body, &out)
	_ = s.opts.DB.FinishImport(job.ID, "succeeded", "", out.Refs, out.DefaultBranch)
}

func (s *Server) handleImportList(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
		return
	}
	jobs, err := s.opts.DB.ListImports(sess.Username)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imports": jobs})
}
