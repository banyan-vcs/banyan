// Package rpc 提供 banyan CLI 的服务端 RPC（docs/05-cli.md）：
// HTTPS + JSON，读响应带 ETag（投影 head OID）实现不可变缓存。
package rpc

import (
	"encoding/json"
	"net/http"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Server 挂载 /api/v1/* 端点。
type Server struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo
}

func NewServer(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Server {
	return &Server{Meta: meta, Objects: objects, Algo: algo}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/objects", s.handleObjects)
	mux.HandleFunc("/api/v1/timelines", s.handleTimelines)
	mux.HandleFunc("/api/v1/log", s.handleLog)
	// TODO(M2): /api/v1/diff  /api/v1/file-history
	// TODO(M3): /api/v1/blame
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- POST /api/v1/objects：逐对象批量定位+获取（懒回填，docs/03 §4）---

type objectsRequest struct {
	Repo string     `json:"repo"`
	OIDs []model.OID `json:"oids"`
}

type objectMeta struct {
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Pack  string `json:"pack"`
	Node  string `json:"node"`
}

type objectsResponse struct {
	Found    map[string]objectMeta `json:"found"`
	Missing  []string              `json:"missing"`
	// TODO(M1): bytes 交付（base64 或 pack 流端点）——与 gitwire 的
	// partial clone 补取（want=OID 的普通 fetch）共享对象定位代码。
}

func (s *Server) handleObjects(w http.ResponseWriter, r *http.Request) {
	var req objectsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	locs, err := s.Meta.LocateObjects(r.Context(), req.Repo, req.OIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := objectsResponse{Found: map[string]objectMeta{}, Missing: []string{}}
	known := map[string]bool{}
	for _, oid := range req.OIDs {
		known[string(oid)] = true
	}
	for oid, loc := range locs {
		if !known[string(oid)] {
			continue
		}
		t := "unknown"
		if dp, err := s.Objects.BlobOrTreeOrCommit(r.Context(), req.Repo, oid); err == nil && len(dp) >= 0 {
			// 类型判定在 M1 解码层精确化（blob/tree/commit）。
			t = guessType(req.Repo, oid)
		}
		resp.Found[string(oid)] = objectMeta{Type: t, Pack: string(loc.PackOID), Node: loc.NodeID}
	}
	for _, oid := range req.OIDs {
		if _, ok := resp.Found[string(oid)]; !ok {
			resp.Missing = append(resp.Missing, string(oid))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func guessType(repo string, oid model.OID) string {
	_ = repo
	_ = oid
	return "unknown" // M1：从 ObjectService 解码缓存取精确类型
}

// --- GET /api/v1/timelines：锚点/timeline 枚举 ---

type timelinesResponse struct {
	Repo      string            `json:"repo"`
	Timelines []timelineSummary `json:"timelines"`
}

type timelineSummary struct {
	Anchor string `json:"anchor"`
	Name   string `json:"name"`
	Head   string `json:"head"`
	Ref    string `json:"ref"`
}

func (s *Server) handleTimelines(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo required"})
		return
	}
	// TODO(M1): MetaStore 需要 ListAllTimelines(repo)（跨锚点枚举）。
	writeJSON(w, http.StatusOK, timelinesResponse{Repo: repo, Timelines: []timelineSummary{}})
}
