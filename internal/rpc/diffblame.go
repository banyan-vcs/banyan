// Path diff and blame on the JSON surface (docs/05 §3, docs/14 M2/M3):
// /api/v1/diff and /api/v1/blame delegate to the same engines the gitaly
// frontend serves (internal/gitaly.Engines) — the text answers are
// byte-pinned against git itself (T81), so API consumers see exactly what
// `git diff` / `git blame --line-porcelain` produce.
package rpc

import (
	"net/http"

	"github.com/banyan-vcs/banyan/internal/gitaly"
)

// engines lazily builds the listener-less engine facade (one per Server —
// test suites host many).
func (s *Server) engines() *gitaly.Server {
	s.engMu.Lock()
	defer s.engMu.Unlock()
	if s.eng == nil {
		s.eng = gitaly.Engines(s.Meta, s.Objects, s.Algo, s.Svc)
	}
	return s.eng
}

// handleDiff: GET /api/v1/diff?repo=&from=&to=&path=
// (path optional: exact file, or directory prefix with a trailing slash).
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, from, to := q.Get("repo"), q.Get("from"), q.Get("to")
	if repo == "" || from == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "repo, from and to required"}})
		return
	}
	text, err := s.engines().DiffText(r.Context(), repo, from, to, q.Get("path"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

// handleBlame: GET /api/v1/blame?repo=&rev=&path= — git blame
// --line-porcelain text at one revision.
func (s *Server) handleBlame(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, rev, path := q.Get("repo"), q.Get("rev"), q.Get("path")
	if repo == "" || rev == "" || path == "" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "repo, rev and path required"}})
		return
	}
	text, err := s.engines().BlameText(r.Context(), repo, rev, path)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}
