package rpc

import (
	"net/http"
	"strconv"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// handleLog: GET /api/v1/log?repo=<r>&rev=<branch>&path=/a/b&limit=20[&from=<oid>]
// — thin adapter over service.Log (docs/05 §6: one implementation, every
// frontend). rev only applies when from is absent (start at a branch tip).
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "repo required"}})
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	rev := r.URL.Query().Get("rev")
	from := model.OID(r.URL.Query().Get("from"))
	path := r.URL.Query().Get("path")

	entries, usedFrom, err := s.Svc.Log(r.Context(), repo, rev, from, path, limit)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	if entries == nil {
		entries = []service.LogEntry{}
	}
	le := make([]logEntry, len(entries))
	for i, e := range entries {
		le[i] = logEntry(e)
	}
	writeJSON(w, http.StatusOK, logResponse{
		Repo: repo, Rev: rev, Path: path, From: string(usedFrom),
		Count: len(le), Entries: le,
	})
}

// logEntry is one log record (hashes exactly match the git view).
type logEntry struct {
	OID        string   `json:"oid"`
	Subject    string   `json:"subject"`
	Author     string   `json:"author"`
	When       int64    `json:"when"`
	Parents    int      `json:"parents"`
	ParentOIDs []string `json:"parentOids,omitempty"`
}

type logResponse struct {
	Repo    string     `json:"repo"`
	Rev     string     `json:"rev,omitempty"`
	Path    string     `json:"path,omitempty"`
	From    string     `json:"from"`
	Count   int        `json:"count"`
	Entries []logEntry `json:"entries"`
}
