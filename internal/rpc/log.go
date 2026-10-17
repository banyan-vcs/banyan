package rpc

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// handleLog 服务端 log 查询（统一 DAG 的路径过滤视图，docs/01 §10）：
//
//	GET /api/v1/log?repo=<r>&path=/a/b&limit=20[&from=<oid>]
//
//	path 省略 → 全历史；path 给定 → 仅展示"修改到该子树的提交"
//	（子树 OID 相对任一父发生变化，或为根提交）——hash 与父视图
//	完全一致（同一批对象），过滤只是查询语义（Scion：问题查询是 RPC）。
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
		state, err := s.Meta.Timeline(r.Context(), model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: branch})
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "entries": []logEntry{}})
			return
		}
		from = state.Head
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
	// xview 把任一提交的树归一到 X 视角：全仓树取 path 处子树；
	// 树中不含 path 首段 → 该树本身就是 X 视角的根树（子树侧提交/
	// 视图 tip），整树即内容（docs/01 §10 混合作用域）。
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

	// 后序 DFS：全父遍历，拓扑序（旧→新）；过滤后取最新 limit 条。
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
				continue // 路径在此提交不存在
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

// logEntry 是一条 log 记录（hash 与 git 视图完全一致）。
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

// hasTopEntry 报告树的顶层是否含名为 name 的条目（缺树/出错按缺省处理）。
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
