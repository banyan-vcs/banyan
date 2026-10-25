package service

import (
	"context"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Log is the server-side history query (docs/02 §10): post-order DFS over
// the unified DAG — hashes match the git view exactly; path filtering is
// query semantics only (Scion: "problem queries are RPCs"). Moved here from
// the rpc handler (docs/05 §6) so every frontend shares one implementation.
type LogEntry struct {
	OID     string `json:"oid"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	When    int64  `json:"when"`
	Parents int    `json:"parents"`
}

// Log returns up to limit entries (newest first). from zero = current
// default-branch woven tip. path ("/a/b" anchor form, "" = full history)
// keeps only commits whose X-view subtree changed.
func (s *Service) Log(ctx context.Context, repo string, from model.OID, path string, limit int) (entries []LogEntry, usedFrom model.OID, err error) {
	if limit <= 0 {
		limit = 20
	}
	if from.IsZero() {
		branch, err := s.defaultBranchOf(ctx, repo)
		if err != nil {
			return nil, "", err
		}
		tip, terr := s.Views.RootTip(ctx, repo, branch)
		if terr != nil {
			return nil, "", terr
		}
		if tip.IsZero() {
			return []LogEntry{}, "", nil
		}
		from = tip
	}
	usedFrom = from

	filtered := false
	if path != "" {
		anchor, err := model.ParseAnchorPath(path)
		if err != nil {
			return nil, "", invalid("path: %v", err)
		}
		path, filtered = string(anchor), true
	}

	lookup := s.Objects.Lookup(repo)
	subMemo := map[model.OID]model.OID{}
	// xview normalizes any commit's tree to the X view: a whole-repo tree maps
	// to its subtree at path; a tree lacking path's first segment is itself
	// the X-view root tree (subtree-side commit / view tip) — docs/02 §10.
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

	// Post-order DFS: full parent traversal, topological order (old→new); the
	// newest limit entries are then taken from the end.
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
		return nil, "", err
	}

	entries = make([]LogEntry, 0, limit)
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
		c, cerr := lookup.Commit(oid)
		if cerr != nil {
			continue
		}
		entries = append(entries, LogEntry{
			OID:     string(oid),
			Subject: firstLine(c.Message),
			Author:  c.Author.Name + " <" + c.Author.Email + ">",
			When:    ci.When,
			Parents: len(ci.Parents),
		})
	}
	return entries, usedFrom, nil
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
