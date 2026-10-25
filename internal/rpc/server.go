// Package rpc provides the server-side RPC for the banyan CLI (docs/06,
// docs/05): HTTPS + JSON over the protocol-agnostic service layer; read
// responses carry an ETag (projection head OID) for immutable caching.
// Handlers stay thin — parse, delegate to internal/service, serialize.
package rpc

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/view"
)

// Server mounts the /api/v1/* endpoints (the httpd frontend of docs/05 §1).
type Server struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo
	Views   *view.Service // derived-view engine (docs/02 sec.13)
	Svc     *service.Service
	// Auth, when set, requires an authenticated identity on every /api/v1
	// request (docs/07 §5): the JSON surface is authentication-gated; the
	// fine-grained repo/dir/branch rules live on the git wire — the web
	// service enforces page-level visibility through it.
	Auth auth.Service
}

func NewServer(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Server {
	svc := service.New(meta, objects, algo)
	return &Server{Meta: meta, Objects: objects, Algo: algo, Views: svc.Views, Svc: svc}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/objects", s.authn(s.handleObjects))
	mux.HandleFunc("/api/v1/objects/content", s.authn(s.handleObjectsContent))
	mux.HandleFunc("/api/v1/timelines", s.authn(s.handleTimelines))
	mux.HandleFunc("/api/v1/log", s.authn(s.handleLog))
	mux.HandleFunc("/api/v1/repack", s.authn(s.handleRepack))
	mux.HandleFunc("/api/v1/fetch-trees", s.authn(s.handleFetchTrees))
	mux.HandleFunc("/api/v1/commit", s.authn(s.handleCommit))
	mux.HandleFunc("/api/v1/refs", s.authn(s.handleRefs))
	mux.HandleFunc("/api/v1/repos", s.authn(s.handleRepos))
	mux.HandleFunc("/api/v1/repos/", s.authn(s.handleRepoSub)) // /api/v1/repos/<repo>/placement
	mux.HandleFunc("/api/v1/cluster/nodes", s.authn(s.handleClusterNodes))
	mux.HandleFunc("/api/v1/resolve", s.authn(s.handleResolve))
	mux.HandleFunc("/api/v1/auth/login", s.handleLogin)
	// TODO(M2): /api/v1/diff  /api/v1/file-history
	// TODO(M3): /api/v1/blame
}

// authn wraps a handler with the authentication requirement (no-op without
// an Auth service).
func (s *Server) authn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Auth != nil && s.Auth.Identify(r).Anonymous() {
			w.Header().Set("WWW-Authenticate", `Basic realm="banyan"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleLogin verifies credentials for the web service (docs/07 §5): the
// single-source check against the wired policy — the session layer lives in
// the web tier, the password table never leaves the server.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if s.Auth == nil {
		writeJSON(w, http.StatusNotImplemented, errEnvelope{Error: errBody{Code: "AUTH_DISABLED", Message: "authorization is off"}})
		return
	}
	check := r.Clone(r.Context())
	check.Header.Set("Authorization",
		"Basic "+base64.StdEncoding.EncodeToString([]byte(req.User+":"+req.Password)))
	if s.Auth.Identify(check).Anonymous() {
		writeJSON(w, http.StatusUnauthorized, errEnvelope{Error: errBody{Code: "INVALID_CREDENTIALS", Message: "wrong user or password"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": req.User})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- error envelope (docs/05 §3.2): code → HTTP status here, CLI exit code client-side ---

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

// --- POST /api/v1/objects: batch per-object locate (lazy backfill, docs/04 §4) ---

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

// --- POST /api/v1/repack: merge a repo's pack fragments (docs/03 §5, docs/09 §3.2) ---

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

// --- POST /api/v1/fetch-trees: thin-client materialization (docs/06 §4.1) ---

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
	// Binary framing on request ("format":"bin") — the thin CLI's dominant
	// transfer drops JSON's base64 +33% (docs/11 P1-2). JSON stays default.
	if req.Format == "bin" {
		w.Header().Set("Content-Type", "application/x-banyan-fetch-trees")
		w.WriteHeader(http.StatusOK)
		if err := writeFetchBinary(w, res, s.Algo); err != nil {
			log.Printf("fetch-trees %s: binary encode: %v", req.Repo, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- POST /api/v1/commit: server-side changeset assembly (docs/06 §4.2) ---

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

// --- GET/POST /api/v1/refs: ref listing and CAS (docs/05 §3.3) ---

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
	switch r.Method {
	case http.MethodPost:
		// Repository creation for the management surface (docs/07 sec.5, the
		// web tier): the same registry the git-surface auto-create uses.
		var req struct {
			Name string `json:"name"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Name == "" || strings.Contains(req.Name, "/") {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "name required, single path segment"}})
			return
		}
		if err := s.Meta.CreateRepo(r.Context(), req.Name, s.Algo); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": req.Name})
	default:
		repos, err := s.Svc.ListRepos(r.Context())
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
	}
}

// --- GET/PUT /api/v1/repos/<repo>/placement: per-repo replica target (docs/10 §6) ---

func (s *Server) handleRepoSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || (parts[1] != "placement" && parts[1] != "rename" && parts[1] != "force-push") {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "want /api/v1/repos/<repo>/(placement|rename|force-push)"}})
		return
	}
	repo := parts[0]
	if parts[1] == "rename" {
		// Repository rename (docs/03 §6): hashed addressing flips the
		// registry alone; named addressing moves every storage location
		// first. Same authentication gate as repo creation (docs/07 §5 —
		// fine-grained RPC authz is the listed follow-up).
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "POST"}})
			return
		}
		var req struct {
			To string `json:"to"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if req.To == "" || strings.Contains(req.To, "/") {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "to required, single path segment"}})
			return
		}
		if err := s.Objects.RenameRepo(r.Context(), repo, req.To); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"from": repo, "to": req.To})
		return
	}
	if parts[1] == "force-push" {
		// Repo-level force-push switch (docs/04 §6a): the subtree rollback
		// guard is default-on; enabling this switch is an explicit
		// administrative opt-in to history-rewriting subtree pushes. Same
		// authentication gate as repo creation.
		switch r.Method {
		case http.MethodGet:
			enabled, err := s.Svc.GetForcePush(r.Context(), repo)
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "enabled": enabled})
		case http.MethodPut:
			var req struct {
				Enabled bool `json:"enabled"`
			}
			if !decodeBody(w, r, &req) {
				return
			}
			if err := s.Svc.SetForcePush(r.Context(), repo, req.Enabled); err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or PUT"}})
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		replicas, err := s.Svc.GetPlacement(r.Context(), repo)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "replicas": replicas})
	case http.MethodPut:
		var req struct {
			Replicas int `json:"replicas"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if err := s.Svc.SetPlacement(r.Context(), repo, req.Replicas); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or PUT"}})
	}
}

// --- GET /api/v1/cluster/nodes: storage-node health view (cluster mode) ---

func (s *Server) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err, cluster := s.Svc.ClusterNodes(r.Context())
	if !cluster {
		writeJSON(w, http.StatusOK, map[string]any{"cluster": false, "nodes": []any{}})
		return
	}
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	if nodes == nil {
		nodes = []storage.NodeInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cluster": true, "nodes": nodes})
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
