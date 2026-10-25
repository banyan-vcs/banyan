package refs

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Options configures the refs service (docs/10 §8.1 thresholds).
type Options struct {
	DefaultReplicas  int
	WriteAcks        string // advisory (gateway enforces); kept for Describe
	SuspectAfter     time.Duration
	DeadAfter        time.Duration
	ReconInterval    time.Duration
	ReconConcurrency int
	// AddressingMode selects the storage addressing mode (docs/03 §6):
	// "named" (default) or "hashed" — the reconciler must copy/trim packs
	// under the same key the gateways write them.
	AddressingMode string
}

func (o *Options) fill() {
	if o.DefaultReplicas <= 0 {
		o.DefaultReplicas = 3
	}
	if o.SuspectAfter <= 0 {
		o.SuspectAfter = 30 * time.Second
	}
	if o.DeadAfter <= o.SuspectAfter {
		o.DeadAfter = o.SuspectAfter + 10*time.Minute
	}
	if o.ReconInterval <= 0 {
		o.ReconInterval = 15 * time.Second
	}
	if o.ReconConcurrency <= 0 {
		o.ReconConcurrency = 4
	}
}

// Server is the refs service: meta RPC over the storage MetaStore, the node
// health table, and the reconciler. One instance per cluster (the single
// writer); tests and the all-in-one mode run the same code over SQLite/mem.
type Server struct {
	Meta   storage.MetaStore
	Opt    Options
	Health *HealthTable
	Recon  *Reconciler
	// Policy is the cluster authorization source of truth (docs/07 §4):
	// gateways sync their snapshot from /internal/authz/policy. Nil = the
	// cluster runs open (the pre-M5 behavior).
	Policy *auth.StaticPolicy
}

func NewServer(meta storage.MetaStore, opt Options) *Server {
	opt.fill()
	health := NewHealthTable(opt.SuspectAfter, opt.DeadAfter)
	recon := NewReconciler(meta, health, opt.DefaultReplicas, opt.ReconConcurrency)
	recon.Addressing = storage.NewAddressing(opt.AddressingMode, meta)
	return &Server{
		Meta:   meta,
		Opt:    opt,
		Health: health,
		Recon:  recon,
	}
}

// Register mounts the internal RPC surface (not exposed by the gateway).
func (s *Server) Register(mux *http.ServeMux) {
	meta := http.NewServeMux()
	meta.HandleFunc("/internal/meta/put-pack", s.hPutPack)
	meta.HandleFunc("/internal/meta/get-pack", s.hGetPack)
	meta.HandleFunc("/internal/meta/list-packs", s.hListPacks)
	meta.HandleFunc("/internal/meta/pack-objects", s.hPackObjects)
	meta.HandleFunc("/internal/meta/drop-pack", s.hDropPack)
	meta.HandleFunc("/internal/meta/set-pack-nodes", s.hSetPackNodes)
	meta.HandleFunc("/internal/meta/locate", s.hLocate)
	meta.HandleFunc("/internal/meta/create-repo", s.hCreateRepo)
	meta.HandleFunc("/internal/meta/get-repo", s.hGetRepo)
	meta.HandleFunc("/internal/meta/list-repos", s.hListRepos)
	meta.HandleFunc("/internal/meta/rename-repo", s.hRenameRepo)
	meta.HandleFunc("/internal/meta/set-force-push", s.hSetForcePush)
	meta.HandleFunc("/internal/meta/timeline", s.hTimeline)
	meta.HandleFunc("/internal/meta/cas-head", s.hCASHead)
	meta.HandleFunc("/internal/meta/ensure-timeline", s.hEnsureTimeline)
	meta.HandleFunc("/internal/meta/list-timelines", s.hListTimelines)
	meta.HandleFunc("/internal/meta/list-all-timelines", s.hListAllTimelines)
	meta.HandleFunc("/internal/meta/set-placement", s.hSetPlacement)
	meta.HandleFunc("/internal/meta/get-placement", s.hGetPlacement)
	mux.Handle("/internal/meta/", meta)
	mux.HandleFunc("/internal/heartbeat", s.hHeartbeat)
	mux.HandleFunc("/internal/health/nodes", s.hHealthNodes)
	mux.HandleFunc("/internal/health/maintenance", s.hMaintenance)
	mux.HandleFunc("/internal/reconcile", s.hReconcile)
	mux.HandleFunc("/internal/authz/policy", s.hAuthzPolicy)
}

// hAuthzPolicy serves the cluster's policy snapshot for gateways
// (docs/07 §4): the refs role is the source of truth, RemotePolicy caches
// this briefly. Cluster-internal surface, like the meta RPC beside it.
func (s *Server) hAuthzPolicy(w http.ResponseWriter, _ *http.Request) {
	if s.Policy == nil {
		http.Error(w, "authorization disabled on this cluster", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Policy)
}

// RunBackground drives the health ticker and the reconciler loop until ctx
// is cancelled.
func (s *Server) RunBackground(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Health.Tick()
			}
		}
	}()
	go func() {
		t := time.NewTicker(s.Opt.ReconInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if res := s.Recon.RunOnce(ctx); res.UnderReplicated > 0 || res.Trimmed > 0 || res.Failed > 0 {
					log.Printf("reconciler: under=%d copied=%d trimmed=%d failed=%d",
						res.UnderReplicated, res.Copied, res.Trimmed, res.Failed)
				}
			}
		}
	}()
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code, status := "INTERNAL", http.StatusInternalServerError
	switch {
	case errors.Is(err, storage.ErrNonFastForward):
		code, status = "NON_FAST_FORWARD", http.StatusConflict
	case errors.Is(err, storage.ErrRepoNotFound):
		code, status = "REPO_NOT_FOUND", http.StatusNotFound
	case errors.Is(err, storage.ErrTimelineUnknown):
		code, status = "TIMELINE_UNKNOWN", http.StatusNotFound
	case errors.Is(err, storage.ErrPackNotFound):
		code, status = "PACK_NOT_FOUND", http.StatusNotFound
	case errors.Is(err, model.ErrObjectNotFound):
		code, status = "OBJECT_NOT_FOUND", http.StatusNotFound
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": err.Error()}})
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, err)
		return v, false
	}
	return v, true
}

// --- meta RPC handlers (thin adapters over MetaStore) ---

func (s *Server) hPutPack(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo    string   `json:"repo"`
		PackOID string   `json:"pack_oid"`
		Objects []string `json:"objects"`
		Nodes   []string `json:"nodes"`
	}](w, r)
	if !ok {
		return
	}
	objs := make([]model.OID, len(req.Objects))
	for i, o := range req.Objects {
		objs[i] = model.OID(o)
	}
	if err := s.Meta.PutPack(r.Context(), req.Repo, model.OID(req.PackOID), objs, req.Nodes); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hGetPack(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo    string `json:"repo"`
		PackOID string `json:"pack_oid"`
	}](w, r)
	if !ok {
		return
	}
	info, err := s.Meta.GetPack(r.Context(), req.Repo, model.OID(req.PackOID))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"pack": info})
}

func (s *Server) hListPacks(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
	}](w, r)
	if !ok {
		return
	}
	packs, err := s.Meta.ListPacks(r.Context(), req.Repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	if packs == nil {
		packs = []storage.PackInfo{}
	}
	writeJSON(w, map[string]any{"packs": packs})
}

func (s *Server) hPackObjects(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo    string `json:"repo"`
		PackOID string `json:"pack_oid"`
	}](w, r)
	if !ok {
		return
	}
	objs, err := s.Meta.PackObjects(r.Context(), req.Repo, model.OID(req.PackOID))
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = string(o)
	}
	writeJSON(w, map[string]any{"objects": out})
}

func (s *Server) hDropPack(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo    string `json:"repo"`
		PackOID string `json:"pack_oid"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.DropPack(r.Context(), req.Repo, model.OID(req.PackOID)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hSetPackNodes(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo    string   `json:"repo"`
		PackOID string   `json:"pack_oid"`
		Nodes   []string `json:"nodes"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.SetPackNodes(r.Context(), req.Repo, model.OID(req.PackOID), req.Nodes); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hLocate(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string   `json:"repo"`
		OIDs []string `json:"oids"`
	}](w, r)
	if !ok {
		return
	}
	oids := make([]model.OID, len(req.OIDs))
	for i, o := range req.OIDs {
		oids[i] = model.OID(o)
	}
	locs, err := s.Meta.LocateObjects(r.Context(), req.Repo, oids)
	if err != nil {
		writeErr(w, err)
		return
	}
	found := map[string]storage.ObjectLocation{}
	for oid, loc := range locs {
		found[string(oid)] = loc
	}
	writeJSON(w, map[string]any{"found": found})
}

func (s *Server) hCreateRepo(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
		Algo int    `json:"algo"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.CreateRepo(r.Context(), req.Repo, model.HashAlgo(req.Algo)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// hRenameRepo flips the registry only (docs/03 §6): storage-layer moves for
// named addressing happen at the gateway BEFORE this call, so by the time
// the name changes the bytes already answer under it.
func (s *Server) hRenameRepo(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		From string `json:"from"`
		To   string `json:"to"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.RenameRepo(r.Context(), req.From, req.To); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hGetRepo(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
	}](w, r)
	if !ok {
		return
	}
	info, err := s.Meta.GetRepo(r.Context(), req.Repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"repo": info})
}

// hSetForcePush toggles the repo-level force-push switch (docs/04 §6a).
func (s *Server) hSetForcePush(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo    string `json:"repo"`
		Enabled bool   `json:"enabled"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.SetForcePush(r.Context(), req.Repo, req.Enabled); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.Meta.ListRepos(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if repos == nil {
		repos = []storage.RepoInfo{}
	}
	writeJSON(w, map[string]any{"repos": repos})
}

func (s *Server) hTimeline(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[model.Timeline](w, r)
	if !ok {
		return
	}
	st, err := s.Meta.Timeline(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) hCASHead(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo     string `json:"repo"`
		Anchor   string `json:"anchor"`
		Name     string `json:"name"`
		Expected string `json:"expected"`
		Next     string `json:"next"`
	}](w, r)
	if !ok {
		return
	}
	tl := model.Timeline{Repo: req.Repo, Anchor: model.AnchorPath(req.Anchor), Name: req.Name}
	if err := s.Meta.CASHead(r.Context(), tl,
		model.OID(req.Expected).NormalizeZero(), model.OID(req.Next).NormalizeZero()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hEnsureTimeline(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo   string `json:"repo"`
		Anchor string `json:"anchor"`
		Name   string `json:"name"`
		Head   string `json:"head"`
		Base   string `json:"base"`
	}](w, r)
	if !ok {
		return
	}
	tl := model.Timeline{Repo: req.Repo, Anchor: model.AnchorPath(req.Anchor), Name: req.Name}
	if err := s.Meta.EnsureTimeline(r.Context(), tl, model.OID(req.Head), model.OID(req.Base)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hListTimelines(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo   string `json:"repo"`
		Anchor string `json:"anchor"`
	}](w, r)
	if !ok {
		return
	}
	tls, err := s.Meta.ListTimelines(r.Context(), req.Repo, model.AnchorPath(req.Anchor))
	if err != nil {
		writeErr(w, err)
		return
	}
	if tls == nil {
		tls = []model.Timeline{}
	}
	writeJSON(w, map[string]any{"timelines": tls})
}

func (s *Server) hListAllTimelines(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
	}](w, r)
	if !ok {
		return
	}
	tls, err := s.Meta.ListAllTimelines(r.Context(), req.Repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tls == nil {
		tls = []model.Timeline{}
	}
	writeJSON(w, map[string]any{"timelines": tls})
}

func (s *Server) hSetPlacement(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo     string `json:"repo"`
		Replicas int    `json:"replicas"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.SetPlacement(r.Context(), req.Repo, req.Replicas); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hGetPlacement(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
	}](w, r)
	if !ok {
		return
	}
	replicas, err := s.Meta.GetPlacement(r.Context(), req.Repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]int{"replicas": replicas})
}

// --- health / cluster surface ---

func (s *Server) hHeartbeat(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		NodeID string `json:"node_id"`
		Addr   string `json:"addr"`
	}](w, r)
	if !ok {
		return
	}
	s.Health.Heartbeat(req.NodeID, req.Addr)
	writeJSON(w, map[string]string{"role": "refs"})
}

func (s *Server) hHealthNodes(w http.ResponseWriter, r *http.Request) {
	nodes := s.Health.Snapshot()
	if nodes == nil {
		nodes = []NodeStatus{}
	}
	writeJSON(w, map[string]any{"nodes": nodes})
}

func (s *Server) hMaintenance(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Node string `json:"node"`
		On   bool   `json:"on"`
	}](w, r)
	if !ok {
		return
	}
	if !s.Health.SetMaintenance(req.Node, req.On) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "NODE_UNKNOWN", "message": "node " + req.Node}})
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hReconcile(w http.ResponseWriter, r *http.Request) {
	s.Health.Tick()
	res := s.Recon.RunOnce(r.Context())
	writeJSON(w, res)
}
