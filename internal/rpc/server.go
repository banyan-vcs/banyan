// Package rpc provides the server-side RPC for the banyan CLI (docs/05,
// docs/10): HTTPS + JSON over the protocol-agnostic service layer; read
// responses carry an ETag (projection head OID) for immutable caching.
// Handlers stay thin — parse, delegate to internal/service, serialize.
package rpc

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/view"
)

// Server mounts the /api/v1/* endpoints (the httpd frontend of docs/10 §1).
type Server struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo
	Views   *view.Service // derived-view engine (docs/01 sec.13)
	Svc     *service.Service
}

func NewServer(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Server {
	svc := service.New(meta, objects, algo)
	return &Server{Meta: meta, Objects: objects, Algo: algo, Views: svc.Views, Svc: svc}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/objects", s.handleObjects)
	mux.HandleFunc("/api/v1/objects/content", s.handleObjectsContent)
	mux.HandleFunc("/api/v1/timelines", s.handleTimelines)
	mux.HandleFunc("/api/v1/log", s.handleLog)
	mux.HandleFunc("/api/v1/repack", s.handleRepack)
	mux.HandleFunc("/api/v1/fetch-trees", s.handleFetchTrees)
	mux.HandleFunc("/api/v1/commit", s.handleCommit)
	mux.HandleFunc("/api/v1/refs", s.handleRefs)
	mux.HandleFunc("/api/v1/repos", s.handleRepos)
	mux.HandleFunc("/api/v1/resolve", s.handleResolve)
	// TODO(M2): /api/v1/diff  /api/v1/file-history
	// TODO(M3): /api/v1/blame
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- error envelope (docs/10 §3.2): code → HTTP status here, CLI exit code client-side ---

type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type errEnvelope struct {
	Error errBody `json:"error"`
}

func statusFor(code string) int {
	switch code {
	case "CONFLICT_BASE_MOVED", "CONFLICT_REF_CAS":
		return http.StatusConflict
	case "NOT_FOUND":
		return http.StatusNotFound
	case "INVALID_ARG":
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeServiceErr(w http.ResponseWriter, err error) {
	code, data := service.Translate(err)
	writeJSON(w, statusFor(code), errEnvelope{Error: errBody{Code: code, Message: err.Error(), Data: data}})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: err.Error()}})
		return false
	}
	return true
}

// --- POST /api/v1/objects: batch per-object locate (lazy backfill, docs/03 §4) ---

type objectsRequest struct {
	Repo string      `json:"repo"`
	OIDs []model.OID `json:"oids"`
}

type objectMeta struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
	Pack string `json:"pack"`
	Node string `json:"node"`
}

type objectsResponse struct {
	Found   map[string]objectMeta `json:"found"`
	Missing []string              `json:"missing"`
}

func (s *Server) handleObjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.objectsGet(w, r)
	case http.MethodPost:
		s.objectsLocate(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or POST"}})
	}
}

// objectsGet: GET /api/v1/objects?repo=<r>&oid=<o> — single-object content
// fetch (cat / thin-client pending-blob backfill).
func (s *Server) objectsGet(w http.ResponseWriter, r *http.Request) {
	repo, oid := r.URL.Query().Get("repo"), model.OID(r.URL.Query().Get("oid"))
	if repo == "" || oid.IsZero() {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "repo and oid required"}})
		return
	}
	t, data, err := s.Svc.GetObject(r.Context(), repo, oid)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Banyan-Type", string(t))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) objectsLocate(w http.ResponseWriter, r *http.Request) {
	var req objectsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	locs, err := s.Meta.LocateObjects(r.Context(), req.Repo, req.OIDs)
	if err != nil {
		writeServiceErr(w, err)
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
		if ot, _, rerr := s.Objects.Raw(r.Context(), req.Repo, oid); rerr == nil {
			t = string(ot)
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

// --- POST /api/v1/objects/content: verified object upload (CLI pre-commit blobs) ---

type contentRequest struct {
	Repo    string              `json:"repo"`
	Objects []service.PutObject `json:"objects"`
}

func (s *Server) handleObjectsContent(w http.ResponseWriter, r *http.Request) {
	var req contentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.Svc.PutObjects(r.Context(), req.Repo, req.Objects); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- GET /api/v1/timelines: anchor/timeline enumeration ---

func (s *Server) handleTimelines(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "repo required"}})
		return
	}
	tls, err := s.Svc.Timelines(r.Context(), repo)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "timelines": tls})
}

// --- POST /api/v1/repack: merge a repo's pack fragments (docs/02 §5, docs/09 §3.2) ---

type repackRequest struct {
	Repo      string `json:"repo"`
	Threshold int    `json:"threshold"` // 0 = server default
}

func (s *Server) handleRepack(w http.ResponseWriter, r *http.Request) {
	var req repackRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.Svc.Repack(r.Context(), req.Repo, req.Threshold)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- POST /api/v1/fetch-trees: thin-client materialization (docs/05 §4.1) ---

func (s *Server) handleFetchTrees(w http.ResponseWriter, r *http.Request) {
	var req service.FetchRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.Svc.FetchTrees(r.Context(), req)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	if !res.Head.IsZero() {
		w.Header().Set("ETag", "\""+string(res.Head)+"\"")
	}
	writeJSON(w, http.StatusOK, res)
}

// --- POST /api/v1/commit: server-side changeset assembly (docs/05 §4.2) ---

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req service.CommitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.Svc.CreateCommit(r.Context(), req)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- GET/POST /api/v1/refs: ref listing and CAS (docs/10 §3.3) ---

func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		repo, sub := r.URL.Query().Get("repo"), r.URL.Query().Get("sub")
		if repo == "" {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "repo required"}})
			return
		}
		refs, err := s.Svc.ListRefs(r.Context(), repo, sub)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "refs": refs})
	case http.MethodPost:
		var req service.RefUpdate
		if !decodeBody(w, r, &req) {
			return
		}
		if err := s.Svc.UpdateRef(r.Context(), req); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or POST"}})
	}
}

// --- GET /api/v1/repos: repo registry ---

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.Svc.ListRepos(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

// --- POST /api/v1/resolve: rev:path → object pointer (CLI cat) ---

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var req service.ResolveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.Svc.Resolve(r.Context(), req)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
