package rpc

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// handleLog is the server-side log query (path-filtered view over the unified DAG, docs/01 §10):
//
//	GET /api/v1/log?repo=<r>&path=/a/b&limit=20[&from=<oid>]
//
//	path omitted → full history; path given → show only commits that touched
//	the subtree (subtree OID changed vs any parent, or a root commit) — hashes
//	match the parent view exactly (same objects); filtering is query semantics only (Scion: problem queries are RPCs).
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo required"})
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	from := model.OID(r.URL.Query().Get("from"))

	info, err := s.Meta.GetRepo(r.Context(), repo)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	branch := info.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	if from.IsZero() {
		// The root view is derived on read (docs/01 §13): walk the woven tip.
		tip, terr := s.Views.RootTip(r.Context(), repo, branch)
		if terr != nil || tip.IsZero() {
			writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "entries": []logEntry{}})
			return
		}
		from = tip
	}

	path := ""
	filtered := false
	if p := r.URL.Query().Get("path"); p != "" {
		anchor, err := model.ParseAnchorPath(p)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		path, filtered = string(anchor), true
	}

	lookup := s.Objects.Lookup(repo)
	subMemo := map[model.OID]model.OID{}
	// xview normalizes any commit's tree to the X view: a whole-repo tree maps to
	// its subtree at path; a tree lacking path's first segment is itself the X-view
	// root tree (subtree-side commit / view tip) — the whole tree is the content (docs/01 §10 mixed scopes).
	xview := func(tree model.OID) model.OID {
		if v, ok := subMemo[tree]; ok {
			return v
		}
		v := model.OID("")
		if !filtered {
			v = tree
		} else if hasTopEntry(lookup, tree, firstSegment(path)) {
			v = model.SubtreeAt(lookup, tree, model.AnchorPath(path))
		} else {
			v = tree
		}
		subMemo[tree] = v
		return v
	}
	subOf := func(oid model.OID) model.OID {
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			return ""
		}
		return xview(ci.Tree)
	}

	// Post-order DFS: full parent traversal, topological order (old→new); after filtering take the newest limit entries.
	visited := map[model.OID]bool{}
	var order []model.OID
	var visit func(oid model.OID) error
	visit = func(oid model.OID) error {
		if visited[oid] {
			return nil
		}
		visited[oid] = true
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			return err
		}
		for _, p := range ci.Parents {
			if err := visit(p); err != nil {
				return err
			}
		}
		order = append(order, oid)
		return nil
	}
	if err := visit(from); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	entries := make([]logEntry, 0, limit)
	for i := len(order) - 1; i >= 0 && len(entries) < limit; i-- {
		oid := order[i]
		ci, err := s.Objects.CommitInfo(repo, oid)
		if err != nil {
			continue
		}
		if filtered {
			sub := subOf(oid)
			if sub.IsZero() {
				continue // path absent in this commit
			}
			touched := len(ci.Parents) == 0
			for _, p := range ci.Parents {
				if subOf(p) != sub {
					touched = true
					break
				}
			}
			if !touched {
				continue
			}
		}
		entries = append(entries, logEntry{
			OID:     string(oid),
			Subject: firstLine(decodeMessage(lookup, oid)),
			Author:  ciAuthor(lookup, oid),
			When:    ci.When,
			Parents: len(ci.Parents),
		})
	}
	writeJSON(w, http.StatusOK, logResponse{Repo: repo, Path: path, From: string(from), Count: len(entries), Entries: entries})
}

// logEntry is one log record (hashes exactly match the git view).
type logEntry struct {
	OID     string `json:"oid"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	When    int64  `json:"when"`
	Parents int    `json:"parents"`
}

type logResponse struct {
	Repo    string     `json:"repo"`
	Path    string     `json:"path,omitempty"`
	From    string     `json:"from"`
	Count   int        `json:"count"`
	Entries []logEntry `json:"entries"`
}

func decodeMessage(lookup model.ObjectLookup, oid model.OID) string {
	c, err := lookup.Commit(oid)
	if err != nil {
		return ""
	}
	return c.Message
}

func ciAuthor(lookup model.ObjectLookup, oid model.OID) string {
	c, err := lookup.Commit(oid)
	if err != nil {
		return ""
	}
	return c.Author.Name + " <" + c.Author.Email + ">"
}

func firstLine(s string) string {
	return strings.SplitN(strings.TrimRight(s, "\n"), "\n", 2)[0]
}

func firstSegment(path string) string {
	segs := model.AnchorPath(path).Segments()
	if len(segs) == 0 {
		return ""
	}
	return segs[0]
}

// hasTopEntry reports whether the tree's top level has an entry named name (missing tree/errors treated as absent).
func hasTopEntry(lookup model.ObjectLookup, tree model.OID, name string) bool {
	if name == "" {
		return false
	}
	t, err := lookup.Tree(tree)
	if err != nil {
		return false
	}
	for _, e := range t.Entries {
		if e.Name == name {
			return true
		}
	}
	return false
}
