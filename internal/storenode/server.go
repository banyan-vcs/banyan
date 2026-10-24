// Package storenode implements the storage role (docs/11 §3): a dumb pack
// warehouse on local disk — PUT/GET/DELETE by (repo, packOID), content
// addressed, no metadata of its own. It heartbeats its address to the refs
// service; placement, holder sets and recovery all live elsewhere.
package storenode

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Server serves the storage role for one node.
type Server struct {
	NodeID    string
	Root      string // shard root on local disk
	Refs      string // refs service address (heartbeat target)
	Advertise string // address other roles use to reach this node
	local     *storage.LocalShardStore
	http      *http.Client
}

func New(nodeID, root, refsAddr, advertise string) *Server {
	return &Server{
		NodeID:    nodeID,
		Root:      root,
		Refs:      refsAddr,
		Advertise: advertise,
		local:     storage.NewLocalShardStore(root),
		http:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Register mounts the internal pack endpoints.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/internal/pack", s.hPack)
	mux.HandleFunc("/internal/object", s.hObject)
	mux.HandleFunc("/internal/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"storage"}`))
	})
}

// hObject serves single-object extraction (docs/12 P1-1): the pack's idx
// locates the object's offset; only that entry (plus its delta base chain)
// inflates. The response body is the raw object; X-Banyan-Type carries the
// git object type. Missing pack/idx/object answer 404 — the gateway falls
// back to the whole-pack path.
func (s *Server) hObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	repo, pack, oid := q.Get("repo"), model.OID(q.Get("pack")), model.OID(q.Get("oid"))
	if repo == "" || pack.IsZero() || oid.IsZero() {
		http.Error(w, "repo, pack and oid required", http.StatusBadRequest)
		return
	}
	t, data, err := s.local.GetObject(repo, pack, oid, storage.AlgoFromQuery(q.Get("algo")))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Banyan-Type", string(t))
	_, _ = w.Write(data)
}

func (s *Server) hPack(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	oid := model.OID(r.URL.Query().Get("oid"))
	if repo == "" || oid.IsZero() {
		http.Error(w, "repo and oid required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !storage.VerifyPackChecksum(oid, raw) {
			http.Error(w, "pack checksum mismatch", http.StatusBadRequest)
			return
		}
		idx := decodeIdx(r.Header.Get("X-Banyan-Idx"))
		if err := s.local.PutPack(repo, oid, raw, idx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		raw, err := s.local.GetPackBytes(repo, oid)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(raw)
	case http.MethodDelete:
		s.local.DeletePack(repo, oid)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "PUT, GET or DELETE", http.StatusMethodNotAllowed)
	}
}

func decodeIdx(v string) []byte {
	if v == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil // idx is an optimization artifact, never a correctness one
	}
	return raw
}

// HeartbeatLoop registers this node with the refs service until ctx is done.
func (s *Server) HeartbeatLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	beat := func() {
		body := []byte(`{"node_id":"` + s.NodeID + `","addr":"` + s.Advertise + `"}`)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Refs+"/internal/heartbeat", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.http.Do(req)
		if err != nil {
			log.Printf("storenode %s: heartbeat: %v", s.NodeID, err)
			return
		}
		resp.Body.Close()
	}
	beat() // register immediately
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat()
		}
	}
}
