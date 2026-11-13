package rpc

import (
	"net/http"

	"github.com/banyan-vcs/banyan/internal/service"
)

// handleTree: GET /api/v1/tree?repo=<r>&rev=<branch>&path=/a/b[&sub=] —
// one directory level of the woven view as structured entries (the web
// code browser's lazy tree walk). rev "" = default branch, path "" = root.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo := q.Get("repo")
	if repo == "" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "repo required"}})
		return
	}
	entries, err := s.Svc.ListTreeEntries(r.Context(), service.ResolveRequest{
		Repo: repo,
		Sub:  q.Get("sub"),
		Rev:  q.Get("rev"),
		Path: q.Get("path"),
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	if entries == nil {
		entries = []service.TreeEntryItem{}
	}
	writeJSON(w, http.StatusOK, treeResponse{
		Repo: repo, Rev: q.Get("rev"), Path: q.Get("path"),
		Count: len(entries), Entries: entries,
	})
}

type treeResponse struct {
	Repo    string                  `json:"repo"`
	Rev     string                  `json:"rev,omitempty"`
	Path    string                  `json:"path,omitempty"`
	Count   int                     `json:"count"`
	Entries []service.TreeEntryItem `json:"entries"`
}
