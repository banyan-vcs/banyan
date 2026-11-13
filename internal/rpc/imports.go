// imports.go — POST /api/v1/imports: the server-side repository import
// (docs/web/web-layer.md §4). One synchronous call: create the target repo, fetch every
// ref of a smart-HTTP remote through internal/importer (the same ingest a
// push rides), adopt the remote's HEAD as the default branch. The source
// credential rides the request body ONLY — it is used for the fetch and
// never recorded, logged or echoed; the identity that authorized the
// import on THIS face is the authenticated caller (write rank).
//
// The call is synchronous on purpose: job state (running/history) belongs
// to the web tier, which has a database — this face stays stateless.
package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/importer"
)

type importRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// SourceAuthorization is the verbatim Authorization header for the
	// source remote (for example "Basic ..." built from an OAuth token or
	// PAT). Empty imports a public remote.
	SourceAuthorization string `json:"source_authorization"`
}

type importResponse struct {
	Repo          string `json:"repo"`
	Refs          int    `json:"refs"`
	DefaultBranch string `json:"defaultBranch"`
	URL           string `json:"url"`
}

func (s *Server) handleImports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "POST"}})
		return
	}
	var req importRequest
	if !decodeBody(w, r, &req) {
		return
	}
	raw := strings.TrimSpace(req.URL)
	if !(strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")) {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{
			Code:    "INVALID_ARG",
			Message: "url must be http(s):// — ssh remotes are not served (docs/integration/gitaly-compat.md)"}})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = importNameOf(raw)
	}
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{
			Code:    "INVALID_ARG",
			Message: "target name required (single path segment) — pass \"name\" or an URL whose last segment names it"}})
		return
	}
	if !s.requireWriteRank(w, r) {
		return
	}
	if _, err := s.Meta.GetRepo(r.Context(), name); err == nil {
		writeJSON(w, http.StatusConflict, errEnvelope{Error: errBody{Code: "CONFLICT_REF_CAS", Message: "repository already exists — imports land on fresh names only"}})
		return
	}
	if err := s.Meta.CreateRepo(r.Context(), name, s.Algo); err != nil {
		writeServiceErr(w, err)
		return
	}

	// The fetch outlives the request only in wall-clock terms — it runs on
	// a bounded background context so a slow remote cannot be cancelled by
	// the caller disconnecting, and cannot hang the server forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := importer.Fetch(ctx, s.Objects, s.Svc, name, raw, req.SourceAuthorization, true)
	if err != nil {
		_ = s.Objects.DeleteRepo(context.Background(), name)
		code, status := importErrStatus(err)
		writeJSON(w, status, errEnvelope{Error: errBody{Code: code, Message: "import failed: " + err.Error()}})
		return
	}
	if res.DefaultBranch != "" && res.DefaultBranch != "main" {
		_ = s.Meta.SetDefaultBranch(ctx, name, res.DefaultBranch)
	}
	writeJSON(w, http.StatusOK, importResponse{Repo: name, Refs: res.Refs, DefaultBranch: res.DefaultBranch, URL: raw})
}

// importErrStatus maps engine failures onto the JSON face's codes: a bad
// URL never reached the wire (invalid argument), the registered ssh gap
// keeps its own code, everything else is an upstream failure.
func importErrStatus(err error) (string, int) {
	switch {
	case errors.Is(err, importer.ErrInvalidURL):
		return "INVALID_ARG", http.StatusBadRequest
	case errors.Is(err, importer.ErrSSHRemote):
		return "INVALID_ARG", http.StatusBadRequest
	default:
		return "UPSTREAM_UNAVAILABLE", http.StatusBadGateway
	}
}

// importNameOf derives a single-segment target name from the remote URL's
// last path segment ("owner/repo.git" → "repo").
func importNameOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	seg := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
	if i := strings.LastIndexByte(seg, '/'); i >= 0 {
		seg = seg[i+1:]
	}
	return seg
}
