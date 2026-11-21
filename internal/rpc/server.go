// Package rpc provides the server-side RPC for the banyan CLI (docs/client/cli.md,
// docs/gateway/service-api.md): HTTPS + JSON over the protocol-agnostic service layer; read
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
	"sync"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/cliproto"
	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/view"
)

// Server mounts the /api/v1/* endpoints (the httpd frontend of docs/gateway/service-api.md §1).
type Server struct {
	Meta    meta.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo
	Views   *view.Service // derived-view engine (docs/design/data-model.md sec.13)
	Svc     *service.Service
	// Auth, when set, requires an authenticated identity on every /api/v1
	// request (docs/gateway/authorization.md §5): the JSON surface is authentication-gated; the
	// fine-grained repo/dir/branch rules live on the git wire — the web
	// service enforces page-level visibility through it.
	Auth auth.Service

	// SSHAddr/WebURL feed the CLI capability broadcast (/api/v1/proto):
	// where a client can SSH-authenticate (the `banyan-auth` exec mints
	// session credentials, docs/gateway/authorization.md §6a) and where the
	// web console lives (its /cli-auth page approves CLI handshakes). Either
	// may be empty — the CLI then knows that login path is unavailable.
	SSHAddr string
	WebURL  string

	// The diff/blame engine facade (lazy; diffblame.go).
	engMu sync.Mutex
	eng   *gitaly.Server
}

func NewServer(meta meta.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Server {
	svc := service.New(meta, objects, algo)
	return &Server{Meta: meta, Objects: objects, Algo: algo, Views: svc.Views, Svc: svc}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/objects", s.authn(s.handleObjects))
	mux.HandleFunc("/api/v1/objects/content", s.authn(s.handleObjectsContent))
	mux.HandleFunc("/api/v1/objects/batch", s.authn(s.handleObjectsBatch))  // blob backfill downlink (docs/operations/performance.md §9 D)
	mux.HandleFunc("/api/v1/proto", s.handleProto)                          // CLI capability broadcast — anonymous by design (login itself needs it)
	mux.HandleFunc("/api/v1/auth/cli-session", s.authn(s.handleCLISession)) // short-lived session mint (docs/gateway/authorization.md §6a)
	mux.HandleFunc("/api/v1/timelines", s.authn(s.handleTimelines))
	mux.HandleFunc("/api/v1/subtrees", s.authn(s.handleSubtrees)) // subtree listing (docs/gateway/authorization.md §8)
	mux.HandleFunc("/api/v1/log", s.authn(s.handleLog))
	mux.HandleFunc("/api/v1/tree", s.authn(s.handleTree)) // structured tree listing (web code browser)
	mux.HandleFunc("/api/v1/repack", s.authn(s.handleRepack))
	mux.HandleFunc("/api/v1/fetch-trees", s.authn(s.handleFetchTrees))
	mux.HandleFunc("/api/v1/commit", s.authn(s.handleCommit))
	mux.HandleFunc("/api/v1/sync", s.authn(s.handleSync)) // subtree→root alignment (docs/design/data-model.md §12)
	mux.HandleFunc("/api/v1/refs", s.authn(s.handleRefs))
	mux.HandleFunc("/api/v1/repos", s.authn(s.handleRepos))
	mux.HandleFunc("/api/v1/merge-requests", s.authn(s.handleMergeRequestsAll)) // workspace-wide MR list
	mux.HandleFunc("/api/v1/groups", s.authn(s.handleGroups))   // repo groups (docs/design/data-model.md §15)
	mux.HandleFunc("/api/v1/groups/", s.authn(s.handleGroupSub)) // /api/v1/groups/<name>
	mux.HandleFunc("/api/v1/imports", s.authn(s.handleImports)) // server-side repo import (docs/web/web-layer.md §4)
	mux.HandleFunc("/api/v1/repos/", s.authn(s.handleRepoSub))  // /api/v1/repos/<repo>/placement
	mux.HandleFunc("/api/v1/cluster/nodes", s.authn(s.handleClusterNodes))
	mux.HandleFunc("/api/v1/meta/backup", s.authn(s.handleMetaBackup))
	mux.HandleFunc("/api/v1/resolve", s.authn(s.handleResolve))
	mux.HandleFunc("/api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("/api/v1/auth/tokens", s.authn(s.handleTokens))   // PAT mint/list (docs/gateway/authorization.md §6)
	mux.HandleFunc("/api/v1/auth/tokens/", s.authn(s.handleTokens))  // DELETE /<hash> revoke
	mux.HandleFunc("/api/v1/auth/ssh-keys", s.authn(s.handleSSHKeys)) // user ssh keys (docs/gateway/authorization.md §7a)
	mux.HandleFunc("/api/v1/auth/password", s.authn(s.handlePassword)) // password self-service (docs/gateway/authorization.md §2a)
	mux.HandleFunc("/api/v1/diff", s.authn(s.handleDiff))           // path diff (docs/project/roadmap.md M2)
	mux.HandleFunc("/api/v1/blame", s.authn(s.handleBlame))         // recursive blame (docs/project/roadmap.md M3)
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

// handleLogin verifies credentials for the web service (docs/gateway/authorization.md §5): the
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

// --- error envelope (docs/gateway/service-api.md §3.2): code → HTTP status here, CLI exit code client-side ---

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
	case "CONFLICT_BASE_MOVED", "CONFLICT_REF_CAS", "CONFLICT_SYNC_CONFLICT", "CONFLICT_MR", "CONFLICT_GROUP":
		return http.StatusConflict
	case "SYNC_UNSUPPORTED":
		return http.StatusBadRequest
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

// --- POST /api/v1/objects: batch per-object locate (lazy backfill, docs/gateway/git-compat.md §4) ---

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
	// Binary frames (Content-Type negotiation, docs/operations/performance.md
	// §9 A2): per-record zlib, no base64 — the CLI's default uplink. The JSON
	// body stays for older clients and non-CLI consumers.
	if strings.HasPrefix(r.Header.Get("Content-Type"), cliproto.ContentTypeObjects) {
		repo, objs, err := cliproto.ParseObjectsFrame(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_BODY", Message: err.Error()}})
			return
		}
		if err := s.Svc.PutObjects(r.Context(), repo, putObjectsFromFrame(objs)); err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
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

func putObjectsFromFrame(objs []cliproto.ObjectRecord) []service.PutObject {
	out := make([]service.PutObject, 0, len(objs))
	for _, o := range objs {
		out = append(out, service.PutObject{OID: model.OID(o.OID), Type: o.Type, Data: o.Data})
	}
	return out
}

// handleProto: GET /api/v1/proto — the CLI protocol capability broadcast
// (docs/client/cli.md §4.4, the multi-version contract). A banyand that
// predates the endpoint answers 404 = proto 1 (JSON + v1 frames only);
// this server advertises proto 2 and its feature set so clients cache the
// negotiation and never pay version trial round trips. Features are
// additive strings; clients must treat unknown ones as ignorable.
// Anonymous BY DESIGN: login itself needs the broadcast (which SSH address,
// which web console) before any credential exists.
func (s *Server) handleProto(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"proto":    2,
		"server":   "banyand",
		"features": []string{"fetch-trees-bin2", "objects-frame", "objects-batch"},
	}
	if s.SSHAddr != "" {
		out["ssh"] = s.SSHAddr
	}
	if s.WebURL != "" {
		out["web"] = s.WebURL
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCLISession: POST /api/v1/auth/cli-session — mints a short-lived
// session credential for the CALLER's identity (docs/gateway/authorization.md
// §6a). The web console's /cli-auth approval calls this with the logged-in
// user's injected credentials; the SSH path mints server-side instead
// (gitwire's banyan-auth exec). Scope is write — sessions never administer,
// and minting one is a write-rank act: a read-scoped credential must not be
// able to upgrade itself here (credentials only ever narrow).
func (s *Server) handleCLISession(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeJSON(w, http.StatusNotImplemented, errEnvelope{Error: errBody{Code: "NO_AUTH", Message: "no auth service configured"}})
		return
	}
	id := s.Auth.Identify(r)
	if id.Anonymous() {
		w.Header().Set("WWW-Authenticate", `Basic realm="banyan"`)
		writeJSON(w, http.StatusUnauthorized, errEnvelope{Error: errBody{Code: "UNAUTHENTICATED", Message: "login first"}})
		return
	}
	if !s.requireWriteRank(w, r) {
		return
	}
	reg, ok := s.Meta.(meta.TokenRegistry)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, errEnvelope{Error: errBody{Code: "NO_TOKEN_REGISTRY", Message: "this deployment owns no token registry"}})
		return
	}
	var req struct {
		Label string `json:"label"` // optional device name — revocable per machine
	}
	if r.Body != nil { // decodeBody would 400 an empty body; unlabeled stays legal
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	}
	plain, info, err := auth.MintSession(r.Context(), reg, id.User, auth.SessionTTL, time.Now(), req.Label)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": plain, "user": info.User, "expires": info.Expires})
}

// handleObjectsBatch: POST /api/v1/objects/batch {repo, oids} — the blob
// backfill downlink (docs/operations/performance.md §9 D): the thin
// client's pending blobs (>inlineLimit) stop being one serial GET each —
// a chunk rides ONE connection as a zlib-framed stream. Missing oids are
// simply absent from the frame; the caller's per-oid fallback reports
// them with context. The client bounds each chunk, so the gathered slice
// stays inside one request's memory window.
func (s *Server) handleObjectsBatch(w http.ResponseWriter, r *http.Request) {
	var req objectsRequest
	if !decodeBody(w, r, &req) || req.Repo == "" || len(req.OIDs) == 0 {
		return
	}
	var records []cliproto.ObjectRecord
	for _, oid := range req.OIDs {
		t, data, err := s.Svc.GetObject(r.Context(), req.Repo, oid)
		if err != nil {
			continue // absent from the frame: the client falls back per-oid
		}
		records = append(records, cliproto.ObjectRecord{OID: string(oid), Type: string(t), Data: data})
	}
	w.Header().Set("Content-Type", cliproto.ContentTypeObjects)
	w.WriteHeader(http.StatusOK)
	if err := cliproto.WriteObjectsFrame(w, req.Repo, cliproto.AlgoName(s.Algo), records); err != nil {
		log.Printf("objects/batch %s: frame encode: %v", req.Repo, err)
	}
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

// --- GET /api/v1/subtrees: the repo's directory timelines as a management
// listing (docs/gateway/authorization.md §8, docs/integration/gitlab-integration.md) — /.woven internals and aux refs filtered,
// branch heads decorated with their last commit, projected marks the
// refs/banyan/tree/<dir> projection set. ---

func (s *Server) handleSubtrees(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "repo required"}})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET"}})
		return
	}
	subs, err := s.Svc.Subtrees(r.Context(), repo)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	branch, err := s.Svc.DefaultBranch(r.Context(), repo)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "defaultBranch": branch, "subtrees": subs})
}

// --- POST /api/v1/repack: merge a repo's pack fragments (docs/storage/storage.md §5, docs/storage/indexing-packing.md §3.2) ---

type repackRequest struct {
	Repo      string `json:"repo"`
	Threshold int    `json:"threshold"` // 0 = server default
}

func (s *Server) handleRepack(w http.ResponseWriter, r *http.Request) {
	var req repackRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !s.requireAdmin(w, r, req.Repo) {
		return
	}
	res, err := s.Svc.Repack(r.Context(), req.Repo, req.Threshold)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- POST /api/v1/fetch-trees: thin-client materialization (docs/client/cli.md §4.1) ---

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
	// transfer drops JSON's base64 +33% (docs/operations/performance.md P1-2). JSON stays default.
	// "binz" is the v2 framing: per-record zlib + delta records (§9 A1+C) —
	// the response Content-Type tells the client which one it got, so an
	// older server silently degrades to v1/JSON and a newer one upgrades.
	switch req.Format {
	case "binz":
		w.Header().Set("Content-Type", cliproto.ContentTypeV2)
		w.WriteHeader(http.StatusOK)
		if err := cliproto.WriteV2(w, cliproFetch(res), s.Algo); err != nil {
			log.Printf("fetch-trees %s: binary v2 encode: %v", req.Repo, err)
		}
	case "bin":
		w.Header().Set("Content-Type", cliproto.ContentTypeV1)
		w.WriteHeader(http.StatusOK)
		if err := cliproto.WriteV1(w, cliproFetch(res), s.Algo); err != nil {
			log.Printf("fetch-trees %s: binary encode: %v", req.Repo, err)
		}
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// cliproFetch converts the service-layer result into the shared CLI
// protocol shape (string OIDs; internal/cliproto owns the framing both
// ends speak).
func cliproFetch(res *service.FetchResult) *cliproto.FetchResult {
	out := &cliproto.FetchResult{
		Branch: res.Branch, Head: string(res.Head), Tree: string(res.Tree), Full: res.Full,
		Trees:   make(map[string][]byte, len(res.Trees)),
		Blobs:   make(map[string][]byte, len(res.Blobs)),
		Pending: res.Pending,
	}
	for k, v := range res.Trees {
		out.Trees[k] = v
	}
	for k, v := range res.Blobs {
		out.Blobs[k] = v
	}
	if res.Deltas != nil {
		out.Deltas = make(map[string]cliproto.Delta, len(res.Deltas))
		for k, d := range res.Deltas {
			out.Deltas[k] = cliproto.Delta{Base: string(d.Base), Size: d.Size, Delta: d.Delta}
		}
	}
	return out
}

// --- POST /api/v1/commit: server-side changeset assembly (docs/client/cli.md §4.2) ---

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	var req service.CommitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// The same push rule the git wire enforces: "" resolves to the repo's
	// default branch so protected-branch policy sees the real name.
	branch := req.Branch
	if branch == "" {
		if info, err := s.Meta.GetRepo(r.Context(), req.Repo); err == nil && info.DefaultBranch != "" {
			branch = info.DefaultBranch
		}
	}
	if !s.requirePush(w, r, req.Repo, req.Sub, branch) {
		return
	}
	if s.mrOnly(r.Context(), req.Repo, branch) {
		s.rejectMROnly(w, branch)
		return
	}
	res, err := s.Svc.CreateCommit(r.Context(), req)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- POST /api/v1/sync: explicit subtree→root alignment (docs/design/data-model.md §12) ---

// handleSync replays a subtree timeline's local-only commits onto the root's
// current projection (docs/client/cli.md §3.9). The same push rule the commit endpoint
// enforces: sync rewrites the timeline head.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req service.SyncRequest
	if !decodeBody(w, r, &req) {
		return
	}
	branch := req.Branch
	if branch == "" {
		if info, err := s.Meta.GetRepo(r.Context(), req.Repo); err == nil && info.DefaultBranch != "" {
			branch = info.DefaultBranch
		}
	}
	if !s.requirePush(w, r, req.Repo, req.Sub, branch) {
		return
	}
	res, err := s.Svc.SyncSubtree(r.Context(), req)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- GET/POST /api/v1/refs: ref listing and CAS (docs/gateway/service-api.md §3.3) ---

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
		branch := strings.TrimPrefix(req.Name, "refs/heads/")
		if !s.requirePush(w, r, req.Repo, "", branch) {
			return
		}
		if branch != req.Name && s.mrOnly(r.Context(), req.Repo, branch) {
			s.rejectMROnly(w, branch)
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
		// Repository creation for the management surface (docs/gateway/authorization.md sec.5, the
		// web tier): the same registry the git-surface auto-create uses.
		// Parent (docs/design/data-model.md §14) optionally nests the new repo under an
		// existing one — organizational metadata only, names stay flat.
		var req struct {
			Name        string `json:"name"`
			Parent      string `json:"parent"`
			Group       string `json:"group"`
			Description string `json:"description"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Name == "" || strings.Contains(req.Name, "/") {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "name required, single path segment"}})
			return
		}
		if req.Parent == req.Name {
			writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "a repo cannot be its own parent"}})
			return
		}
		// A metadata-plane write: scoped credentials need write rank
		// (docs/gateway/authorization.md §8b) — unscoped identities keep the authenticated gate;
		// the not-yet-existing repo is unknown to per-repo policy by
		// construction, and the console authorizes creation on its side.
		if !s.requireWriteRank(w, r) {
			return
		}
		if err := s.Meta.CreateRepo(r.Context(), req.Name, s.Algo); err != nil {
			writeServiceErr(w, err)
			return
		}
		// Link after create; a refused link (missing parent, over-deep chain)
		// rolls the half-created repo back — creation stays all-or-nothing
		// (the imports precedent).
		if req.Parent != "" {
			if err := s.Meta.SetRepoParent(r.Context(), req.Name, req.Parent); err != nil {
				_ = s.Meta.DeleteRepo(r.Context(), req.Name)
				writeServiceErr(w, err)
				return
			}
		}
		// The group label follows the same all-or-nothing rule: an unknown
		// group rolls the create back too.
		if req.Group != "" {
			if gs := s.groupStore(); gs == nil {
				_ = s.Meta.DeleteRepo(r.Context(), req.Name)
				writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "groups not available on this deployment's metadata plane"}})
				return
			} else if err := gs.SetRepoGroup(r.Context(), req.Name, req.Group); err != nil {
				_ = s.Meta.DeleteRepo(r.Context(), req.Name)
				writeServiceErr(w, err)
				return
			}
		}
		// The display one-liner rides along (display metadata, no rollback
		// semantics to violate — a failed write still fails the create).
		if req.Description != "" {
			if err := s.Meta.SetRepoDescription(r.Context(), req.Name, req.Description); err != nil {
				_ = s.Meta.DeleteRepo(r.Context(), req.Name)
				writeServiceErr(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": req.Name, "parent": req.Parent})
	default:
		repos, err := s.Svc.ListRepos(r.Context())
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
	}
}

// --- GET/PUT /api/v1/repos/<repo>/placement: per-repo replica target (docs/storage/ha.md §6) ---

func (s *Server) handleRepoSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) >= 2 && parts[1] == "merge-requests" {
		s.handleMergeRequests(w, r, parts)
		return
	}
	if len(parts) >= 2 && parts[1] == "branch-rules" {
		s.handleBranchRules(w, r, parts)
		return
	}
	if len(parts) >= 2 && parts[1] == "members" {
		s.handleRepoMembers(w, r, parts[0])
		return
	}
	if len(parts) != 2 || (parts[1] != "placement" && parts[1] != "rename" && parts[1] != "force-push" && parts[1] != "size" && parts[1] != "hooks" && parts[1] != "subtree-access" && parts[1] != "parent" && parts[1] != "group" && parts[1] != "description") {
		writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "want /api/v1/repos/<repo>/(placement|rename|force-push|size|hooks|subtree-access|parent|group|description|members)"}})
		return
	}
	// Repo management is admin territory (docs/gateway/authorization.md §5); size stays a plain
	// repo stat for any authenticated member, and the group label and the
	// description are organizational/display metadata — write rank, the
	// creation gate's rank (the parent link stays admin because re-homing
	// reshapes the hierarchy the policy engine walks; a label never does).
	if parts[1] != "size" && parts[1] != "group" && parts[1] != "description" && !s.requireAdmin(w, r, parts[0]) {
		return
	}
	if parts[1] == "group" {
		s.handleRepoGroup(w, r, parts[0])
		return
	}
	if parts[1] == "description" {
		// GET/PUT the display one-liner: GET any authenticated member, PUT
		// write rank (the group-label precedent).
		switch r.Method {
		case http.MethodGet:
			info, err := s.Meta.GetRepo(r.Context(), parts[0])
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"repo": parts[0], "description": info.Description})
		case http.MethodPut:
			var req struct {
				Description string `json:"description"`
			}
			if !decodeBody(w, r, &req) {
				return
			}
			if !s.requireWriteRank(w, r) {
				return
			}
			if err := s.Meta.SetRepoDescription(r.Context(), parts[0], req.Description); err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"repo": parts[0], "description": req.Description})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or PUT"}})
		}
		return
	}
	if parts[1] == "parent" {
		// The hierarchy link (docs/design/data-model.md §14). Re-homing an existing repo is
		// repo management — the outer requireAdmin gate covers it; cycle
		// and depth validation live in the store.
		switch r.Method {
		case http.MethodGet:
			info, err := s.Meta.GetRepo(r.Context(), parts[0])
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"repo": parts[0], "parent": info.Parent})
		case http.MethodPut:
			var req struct {
				Parent string `json:"parent"`
			}
			if !decodeBody(w, r, &req) {
				return
			}
			if err := s.Meta.SetRepoParent(r.Context(), parts[0], req.Parent); err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"repo": parts[0], "parent": req.Parent})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET or PUT"}})
		}
		return
	}
	if parts[1] == "hooks" {
		// Receive-hook registry (docs/gateway/git-compat.md §7): registrations live in the
		// metadata plane so every gateway evaluates the same hooks. Same
		// authentication gate as repo administration.
		repo := parts[0]
		switch r.Method {
		case http.MethodGet:
			hooks, err := s.Meta.ListHooks(r.Context(), repo)
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "hooks": hooks})
		case http.MethodPut:
			var hook meta.HookInfo
			if !decodeBody(w, r, &hook) {
				return
			}
			if err := s.Meta.SetHook(r.Context(), repo, hook); err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		case http.MethodDelete:
			id := r.URL.Query().Get("id")
			if id == "" {
				writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "id query parameter required"}})
				return
			}
			if err := s.Meta.DeleteHook(r.Context(), repo, id); err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET, PUT or DELETE"}})
		}
		return
	}
	if parts[1] == "subtree-access" {
		// Runtime directory access rules (docs/gateway/authorization.md §8): fetch AND push of a
		// subtree anchor (and everything deeper) restricted to an allowlist.
		// Same admin gate as the other management rows; deployments whose
		// metadata plane carries no rule store answer 404 (the token-layer
		// precedent).
		rs, ok := s.Meta.(meta.SubtreeRuleStore)
		if !ok {
			writeJSON(w, http.StatusNotFound, errEnvelope{Error: errBody{Code: "NOT_FOUND", Message: "subtree rules not available on this deployment's metadata plane"}})
			return
		}
		repo := parts[0]
		switch r.Method {
		case http.MethodGet:
			rules, err := rs.SubtreeRules(r.Context(), repo)
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			if rules == nil {
				rules = []meta.SubtreeRule{} // JSON null would crash the console (the token-layer precedent)
			}
			writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "rules": rules})
		case http.MethodPut:
			var rule meta.SubtreeRule
			if !decodeBody(w, r, &rule) {
				return
			}
			if err := rule.Valid(); err != nil {
				writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: err.Error()}})
				return
			}
			if err := rs.SetSubtreeRule(r.Context(), repo, rule); err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		case http.MethodDelete:
			path := r.URL.Query().Get("path")
			if !meta.ValidSubtreePath(path) {
				writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "path query parameter required (anchor form: / or /a/b)"}})
				return
			}
			if err := rs.DeleteSubtreeRule(r.Context(), repo, path); err != nil {
				writeServiceErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET, PUT or DELETE"}})
		}
		return
	}
	if parts[1] == "size" {
		// Repo-scale statistics (docs/operations/performance.md §6): total pack bytes + pack count —
		// the number the full-clone budget enforces.
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET"}})
			return
		}
		bytes, packs, err := s.Svc.RepoSize(r.Context(), parts[0])
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repo": parts[0], "bytes": bytes, "packs": packs})
		return
	}
	repo := parts[0]
	if parts[1] == "rename" {
		// Repository rename (docs/storage/storage.md §6): hashed addressing flips the
		// registry alone; named addressing moves every storage location
		// first. Same authentication gate as repo creation (docs/gateway/authorization.md §5 —
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
		// Repo-level force-push switch (docs/gateway/git-compat.md §6a): the subtree rollback
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
	if !s.requireAdmin(w, r, "") {
		return
	}
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
		nodes = []meta.NodeInfo{}
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

// handleMetaBackup: POST /api/v1/meta/backup {"path": "..."} — online
// snapshot of the SQLite metadata plane (VACUUM INTO: readers and the
// single writer keep running; docs/storage/ha.md §8.4). Cluster backends answer 409:
// their backups belong to the database ecosystem's own tooling.
func (s *Server) handleMetaBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r, "") {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "POST only"}})
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "path required"}})
		return
	}
	sql, ok := s.Meta.(*meta.SQLMetaStore)
	if !ok {
		writeJSON(w, http.StatusConflict, errEnvelope{Error: errBody{Code: "CONFLICT", Message: "metadata backend does not support in-process backup"}})
		return
	}
	if err := sql.Backup(req.Path); err != nil {
		writeJSON(w, http.StatusInternalServerError, errEnvelope{Error: errBody{Code: "INTERNAL", Message: err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "path": req.Path})
}
