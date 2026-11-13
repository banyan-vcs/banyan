package refs

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/metrics"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
)

// The refs role's own telemetry plane (served at /metrics): reconciler and
// sweep progress — CAS contention is counted at the CAS implementations
// themselves (internal/storage), so every topology reports it.
var (
	mReconUnder = metrics.GetGauge("banyan_reconciler_under_replicated", "packs below replica target at the last pass")
	mReconFailed = metrics.GetGauge("banyan_reconciler_failed", "packs that could not converge at the last pass")
	mReconCopied = metrics.GetCounter("banyan_reconciler_copied_total", "packs re-replicated by the reconciler")
	mReconTrimmed = metrics.GetCounter("banyan_reconciler_trimmed_total", "over-replicated packs trimmed")
	mSweepKeys  = metrics.GetCounter("banyan_sweep_keys_reclaimed_total", "storage keys reclaimed by fleet sweeps")
	mSweepPacks = metrics.GetCounter("banyan_sweep_packs_reclaimed_total", "packs reclaimed by fleet sweeps")
)

// observeRecon records one convergence pass's outcome.
func observeRecon(res ReconResult) {
	mReconUnder.With().Set(int64(res.UnderReplicated))
	mReconFailed.With().Set(int64(res.Failed))
	mReconCopied.With().Add(int64(res.Copied))
	mReconTrimmed.With().Add(int64(res.Trimmed))
}

// Options configures the refs service (docs/storage/ha.md §8.1 thresholds).
type Options struct {
	DefaultReplicas  int
	WriteAcks        string // advisory (gateway enforces); kept for Describe
	SuspectAfter     time.Duration
	DeadAfter        time.Duration
	ReconInterval    time.Duration
	ReconConcurrency int
	// AddressingMode selects the storage addressing mode (docs/storage/storage.md §6):
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

// Server is the refs service: meta RPC over the storage meta.MetaStore, the node
// health table, and the reconciler. One instance per cluster (the single
// writer); tests and the all-in-one mode run the same code over SQLite/mem.
type Server struct {
	Meta   meta.MetaStore
	Opt    Options
	Health *HealthTable
	Recon  *Reconciler
	// Policy is the cluster authorization source of truth (docs/gateway/authorization.md §4):
	// gateways sync their snapshot from /internal/authz/policy. Nil = the
	// cluster runs open (the pre-M5 behavior).
	Policy *auth.StaticPolicy
}

func NewServer(ms meta.MetaStore, opt Options) *Server {
	opt.fill()
	health := NewHealthTable(opt.SuspectAfter, opt.DeadAfter)
	recon := NewReconciler(ms, health, opt.DefaultReplicas, opt.ReconConcurrency)
	recon.Addressing = meta.NewAddressing(opt.AddressingMode, ms)
	return &Server{
		Meta:   ms,
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
	meta.HandleFunc("/internal/meta/begin-rename", s.hBeginRename)
	meta.HandleFunc("/internal/meta/abort-rename", s.hAbortRename)
	meta.HandleFunc("/internal/meta/delete-repo", s.hDeleteRepo)
	meta.HandleFunc("/internal/meta/set-force-push", s.hSetForcePush)
	meta.HandleFunc("/internal/meta/set-default-branch", s.hSetDefaultBranch)
	meta.HandleFunc("/internal/meta/hooks", s.hHooks)
	meta.HandleFunc("/internal/meta/hooks/delete", s.hDeleteHook)
	meta.HandleFunc("/internal/meta/repo-blob", s.hSetRepoBlob)
	meta.HandleFunc("/internal/meta/repo-blob/get", s.hGetRepoBlob)
	meta.HandleFunc("/internal/meta/pool/link", s.hLinkPool)
	meta.HandleFunc("/internal/meta/pool/unlink", s.hUnlinkPool)
	meta.HandleFunc("/internal/meta/pool/of", s.hPoolOf)
	meta.HandleFunc("/internal/meta/pool/list", s.hListPoolMembers)
	meta.HandleFunc("/internal/meta/pool/delete", s.hDeletePoolRecords)
	meta.HandleFunc("/internal/meta/timeline", s.hTimeline)
	meta.HandleFunc("/internal/meta/cas-head", s.hCASHead)
	meta.HandleFunc("/internal/meta/ensure-timeline", s.hEnsureTimeline)
	meta.HandleFunc("/internal/meta/timeline-base", s.hSetTimelineBase)
	meta.HandleFunc("/internal/meta/list-timelines", s.hListTimelines)
	meta.HandleFunc("/internal/meta/list-all-timelines", s.hListAllTimelines)
	meta.HandleFunc("/internal/meta/set-placement", s.hSetPlacement)
	meta.HandleFunc("/internal/meta/get-placement", s.hGetPlacement)
	mux.Handle("/internal/meta/", meta)
	mux.HandleFunc("/internal/heartbeat", s.hHeartbeat)
	mux.HandleFunc("/internal/health/nodes", s.hHealthNodes)
	mux.HandleFunc("/internal/health/maintenance", s.hMaintenance)
	mux.HandleFunc("/internal/reconcile", s.hReconcile)
	mux.HandleFunc("/internal/sweep", s.hSweep)
	mux.HandleFunc("/internal/authz/policy", s.hAuthzPolicy)
	// The refs process's own telemetry (Prometheus text format).
	mux.Handle("/metrics", metrics.Handler())
}

// hAuthzPolicy serves the cluster's policy snapshot for gateways
// (docs/gateway/authorization.md §4): the refs role is the source of truth, RemotePolicy caches
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
			res := s.Recon.RunOnce(ctx)
			observeRecon(res)
			if res.UnderReplicated > 0 || res.Trimmed > 0 || res.Failed > 0 {
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
	case errors.Is(err, meta.ErrNonFastForward):
		code, status = "NON_FAST_FORWARD", http.StatusConflict
	case errors.Is(err, meta.ErrRepoNotFound):
		code, status = "REPO_NOT_FOUND", http.StatusNotFound
	case errors.Is(err, meta.ErrTimelineUnknown):
		code, status = "TIMELINE_UNKNOWN", http.StatusNotFound
	case errors.Is(err, meta.ErrPackNotFound):
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

// --- meta RPC handlers (thin adapters over meta.MetaStore) ---

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
		packs = []meta.PackInfo{}
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
	found := map[string]meta.ObjectLocation{}
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

// hRenameRepo flips the registry only (docs/storage/storage.md §6): storage-layer moves for
// named addressing happen at the gateway BETWEEN hBeginRename and this call,
// so by the time the name changes the bytes already answer under it. The
// flip also lifts the fence in the same transaction.
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

// hBeginRename installs the rename fence (write barrier phase 1, docs/storage/storage.md
// §6a); hAbortRename lifts it after a failed or abandoned rename.
func (s *Server) hBeginRename(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		From string `json:"from"`
		To   string `json:"to"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.BeginRename(r.Context(), req.From, req.To); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hAbortRename(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		From string `json:"from"`
		To   string `json:"to"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.AbortRename(r.Context(), req.From, req.To); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hDeleteRepo(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.DeleteRepo(r.Context(), req.Repo); err != nil {
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

// hSetForcePush toggles the repo-level force-push switch (docs/gateway/git-compat.md §6a).
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

// hSetDefaultBranch changes the advertised HEAD symref target (gitaly
// CreateRepository default-branch semantics).
func (s *Server) hSetDefaultBranch(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.SetDefaultBranch(r.Context(), req.Repo, req.Branch); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// hHooks serves the receive-hook registry (docs/gateway/git-compat.md §7): a request with a
// hook upserts it; without one it lists (the gateway's per-push lookup).
func (s *Server) hHooks(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string           `json:"repo"`
		Hook *meta.HookInfo `json:"hook"`
	}](w, r)
	if !ok {
		return
	}
	if req.Hook == nil {
		hooks, err := s.Meta.ListHooks(r.Context(), req.Repo)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"hooks": hooks})
		return
	}
	if err := s.Meta.SetHook(r.Context(), req.Repo, *req.Hook); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hDeleteHook(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
		ID   string `json:"id"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.DeleteHook(r.Context(), req.Repo, req.ID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hSetRepoBlob(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
		Key  string `json:"key"`
		Data []byte `json:"data"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.SetRepoBlob(r.Context(), req.Repo, req.Key, req.Data); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hGetRepoBlob(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo string `json:"repo"`
		Key  string `json:"key"`
	}](w, r)
	if !ok {
		return
	}
	data, found, err := s.Meta.GetRepoBlob(r.Context(), req.Repo, req.Key)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"data": data})
}

func (s *Server) hLinkPool(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Member   string `json:"member"`
		Pool     string `json:"pool"`
		Upstream bool   `json:"upstream"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.LinkPool(r.Context(), req.Member, req.Pool, req.Upstream); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hUnlinkPool(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Member string `json:"member"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.UnlinkPool(r.Context(), req.Member); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) hPoolOf(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Member string `json:"member"`
	}](w, r)
	if !ok {
		return
	}
	pool, ok, err := s.Meta.PoolOf(r.Context(), req.Member)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"pool": pool, "linked": ok})
}

func (s *Server) hListPoolMembers(w http.ResponseWriter, r *http.Request) {
	members, err := s.Meta.ListPoolMembers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"members": members})
}

func (s *Server) hDeletePoolRecords(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Pool string `json:"pool"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.Meta.DeletePoolRecords(r.Context(), req.Pool); err != nil {
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
		repos = []meta.RepoInfo{}
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

// hSetTimelineBase advances an existing timeline's fork point (explicit-rebase
// bookkeeping, docs/design/data-model.md §12 — the sync path lands here in cluster mode).
func (s *Server) hSetTimelineBase(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Repo   string `json:"repo"`
		Anchor string `json:"anchor"`
		Name   string `json:"name"`
		Base   string `json:"base"`
	}](w, r)
	if !ok {
		return
	}
	tl := model.Timeline{Repo: req.Repo, Anchor: model.AnchorPath(req.Anchor), Name: req.Name}
	if err := s.Meta.SetTimelineBase(r.Context(), tl, model.OID(req.Base)); err != nil {
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
	observeRecon(res)
	writeJSON(w, res)
}

// hSweep drives one fleet-sweep pass (docs/storage/ha.md §8.7): POST with
// {"min_age_sec": n, "dry_run": bool} — orphan keys/packs past the grace
// are reclaimed; a dry run reports the same numbers without deleting.
func (s *Server) hSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	req, ok := decode[struct {
		MinAgeSec int  `json:"min_age_sec"`
		DryRun    bool `json:"dry_run"`
	}](w, r)
	if !ok {
		return
	}
	s.Health.Tick()
	res := s.Recon.SweepOnce(r.Context(), SweepOptions{
		MinAge: time.Duration(req.MinAgeSec) * time.Second,
		DryRun: req.DryRun,
	})
	if !req.DryRun {
		mSweepKeys.With().Add(int64(res.KeysDropped))
		mSweepPacks.With().Add(int64(res.PacksDropped))
	}
	writeJSON(w, res)
}
