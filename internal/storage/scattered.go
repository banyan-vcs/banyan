package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
)

// NodeClient is the gateway's / reconciler's HTTP client to one storage node
// (docs/10 §3): packs are raw bodies, content-addressed by query params.
type NodeClient struct {
	Addr string
	HTTP *http.Client
}

// Node request timeouts are layered (docs/10 §3): connection setup and
// response headers get fixed transport budgets — the only phases with no
// progress signal — while body streaming is governed by the caller's ctx
// alone. A whole-client Timeout would cap every body read: a multi-GB pack
// transfer would die at the 5-minute mark regardless of steady progress,
// and a stalled small read would wait the full budget for nothing.
var (
	nodeDialTimeout         = 10 * time.Second
	nodeResponseHeaderWait  = 60 * time.Second
	nodeTLSHandshakeTimeout = 10 * time.Second
)

func NewNodeClient(addr string) *NodeClient {
	return &NodeClient{Addr: addr, HTTP: &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: nodeDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   nodeTLSHandshakeTimeout,
			ResponseHeaderTimeout: nodeResponseHeaderWait,
			IdleConnTimeout:       90 * time.Second,
		},
	}}
}

func (n *NodeClient) packURL(repo string, packOID model.OID) string {
	return n.Addr + "/internal/pack?repo=" + url.QueryEscape(repo) + "&oid=" + url.QueryEscape(string(packOID))
}

// RenameRepo asks a node to move a repo directory between storage keys
// (named addressing, docs/03 §6). A node without the source answers OK —
// the move is idempotent per node.
func (n *NodeClient) RenameRepo(ctx context.Context, from, to string) error {
	body := []byte(`{"from":` + strconv.Quote(from) + `,"to":` + strconv.Quote(to) + `}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Addr+"/internal/pack/rename", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("storage %s rename %s→%s: status %d", n.Addr, from, to, resp.StatusCode)
	}
	return nil
}

// DeleteRepo asks a node to drop a repo's whole storage directory.
func (n *NodeClient) DeleteRepo(ctx context.Context, repo string) error {
	body := []byte(`{"repo":` + strconv.Quote(repo) + `}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Addr+"/internal/pack/delete-repo", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("storage %s delete %s: status %d", n.Addr, repo, resp.StatusCode)
	}
	return nil
}

func (n *NodeClient) PutPack(ctx context.Context, repo string, packOID model.OID, packBytes, idxBytes []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, n.packURL(repo, packOID), bytes.NewReader(packBytes))
	if err != nil {
		return err
	}
	if h := encodeIdxHeader(idxBytes); h != "" {
		req.Header.Set("X-Banyan-Idx", h)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("storage %s put %s: status %d", n.Addr, packOID, resp.StatusCode)
	}
	return nil
}

// PutPackStream streams the pack body from open() (one pass, no buffering)
// with ContentLength set so the node can size-check the landing file.
func (n *NodeClient) PutPackStream(ctx context.Context, repo string, packOID model.OID, size int64, open func() (io.ReadCloser, error), idxBytes []byte) error {
	src, err := open()
	if err != nil {
		return err
	}
	defer src.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, n.packURL(repo, packOID), src)
	if err != nil {
		return err
	}
	if size > 0 {
		req.ContentLength = size
	}
	if h := encodeIdxHeader(idxBytes); h != "" {
		req.Header.Set("X-Banyan-Idx", h)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("storage %s put %s: status %d", n.Addr, packOID, resp.StatusCode)
	}
	return nil
}

func (n *NodeClient) GetPack(ctx context.Context, repo string, packOID model.OID) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.packURL(repo, packOID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: pack %s on %s", model.ErrObjectNotFound, packOID, n.Addr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage %s get %s: status %d", n.Addr, packOID, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// OpenPack streams a pack GET: the node's response body is handed back
// unbuffered (disk → network → consumer), with Content-Length as size.
func (n *NodeClient) OpenPack(ctx context.Context, repo string, packOID model.OID) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.packURL(repo, packOID), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		rerr := fmt.Errorf("storage %s get %s: status %d", n.Addr, packOID, resp.StatusCode)
		if resp.StatusCode == http.StatusNotFound {
			rerr = fmt.Errorf("%w: pack %s on %s", model.ErrObjectNotFound, packOID, n.Addr)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, 0, rerr
	}
	return resp.Body, resp.ContentLength, nil
}

// HeadPack probes a pack's size (Content-Length) without transferring it —
// the whole-pack memoize guard's size check.
func (n *NodeClient) HeadPack(ctx context.Context, repo string, packOID model.OID) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, n.packURL(repo, packOID), nil)
	if err != nil {
		return -1, err
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1, fmt.Errorf("storage %s head %s: status %d", n.Addr, packOID, resp.StatusCode)
	}
	if resp.ContentLength < 0 {
		return -1, nil
	}
	return resp.ContentLength, nil
}

// VerifyPack streams the pack from the node and checks its trailer with a
// trailing hash window — full-content integrity at O(window) memory, the
// multi-GB replica-verify path (reconciler).
func (n *NodeClient) VerifyPack(ctx context.Context, repo string, packOID model.OID) error {
	rc, _, err := n.OpenPack(ctx, repo, packOID)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, sum, ok, err := SpoolVerifiedCopy(io.Discard, rc, algoOfPackOID(packOID))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("storage %s pack %s: checksum mismatch", n.Addr, packOID)
	}
	if sum != string(packOID) {
		return fmt.Errorf("storage %s pack %s: checksum/oid mismatch (got %s)", n.Addr, packOID, sum)
	}
	return nil
}

func (n *NodeClient) DeletePack(ctx context.Context, repo string, packOID model.OID) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, n.packURL(repo, packOID), nil)
	if err != nil {
		return err
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil // 404 tolerated: CAS re-ingest may have skipped the write
}

func (n *NodeClient) objectURL(repo string, packOID, oid model.OID, algo model.HashAlgo) string {
	return n.Addr + "/internal/object?repo=" + url.QueryEscape(repo) +
		"&pack=" + url.QueryEscape(string(packOID)) +
		"&oid=" + url.QueryEscape(string(oid)) +
		"&algo=" + algoQuery(algo)
}

func algoQuery(algo model.HashAlgo) string {
	if algo == model.SHA256 {
		return "sha256"
	}
	return "sha1"
}

// AlgoFromQuery parses the algo parameter of /internal/object (storenode side).
func AlgoFromQuery(v string) model.HashAlgo {
	if v == "sha256" {
		return model.SHA256
	}
	return model.SHA1
}

// GetObject extracts one object on the node via its local idx (docs/11
// P1-1): the response body is the object, X-Banyan-Type carries its type.
func (n *NodeClient) GetObject(ctx context.Context, repo string, packOID, oid model.OID, algo model.HashAlgo) (model.ObjectType, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.objectURL(repo, packOID, oid, algo), nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", nil, fmt.Errorf("storage %s object %s/%s: status %d", n.Addr, packOID, oid, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	return model.ObjectType(resp.Header.Get("X-Banyan-Type")), data, nil
}

// --- ScatteredStore: the gateway's shard backend (docs/10 §3/§5) ---

// ScatteredStore places each pack on R random healthy nodes, confirms W
// writes, and reads from any holder. Registration is the fact: PutPack
// returns exactly the confirmed holder set for the caller to register.
type ScatteredStore struct {
	Refs      *RefsClient
	DefaultR  int    // cluster default replica target
	WriteAcks string // "quorum" (default) | "all" | "one"
	// DiskCache persists whole packs on gateway disk (docs/11 P2-1): a
	// restart serves its first clones from disk instead of re-transferring
	// from storage nodes. Optional — nil disables.
	DiskCache *DiskPackCache

	healthyTTL time.Duration
	healthyMu  sync.Mutex
	healthy    []NodeInfo
	healthyAt  time.Time
	clientsMu  sync.Mutex
	clients    map[string]*NodeClient
}

func NewScatteredStore(refs *RefsClient, defaultR int, writeAcks string) *ScatteredStore {
	if defaultR <= 0 {
		defaultR = 3
	}
	if writeAcks == "" {
		writeAcks = "quorum"
	}
	return &ScatteredStore{
		Refs:       refs,
		DefaultR:   defaultR,
		WriteAcks:  writeAcks,
		healthyTTL: 2 * time.Second,
		clients:    map[string]*NodeClient{},
	}
}

func (s *ScatteredStore) client(addr string) *NodeClient {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	if c, ok := s.clients[addr]; ok {
		return c
	}
	c := NewNodeClient("http://" + addr)
	s.clients[addr] = c
	return c
}

// healthyNodes caches the refs health view briefly: a stale view at worst
// costs a failed PUT (that node just doesn't confirm).
func (s *ScatteredStore) healthyNodes(ctx context.Context) []NodeInfo {
	s.healthyMu.Lock()
	if time.Since(s.healthyAt) < s.healthyTTL && s.healthy != nil {
		out := s.healthy
		s.healthyMu.Unlock()
		return out
	}
	s.healthyMu.Unlock()
	nodes, err := s.Refs.HealthyNodes(ctx)
	if err != nil || len(nodes) == 0 {
		s.healthyMu.Lock()
		out := s.healthy // fall back to the last known view on RPC failure
		s.healthyMu.Unlock()
		return out
	}
	s.healthyMu.Lock()
	s.healthy, s.healthyAt = nodes, time.Now()
	out := nodes
	s.healthyMu.Unlock()
	return out
}

// pickRandom draws n distinct entries at random (placement, docs/10 §3).
func pickRandom[T any](items []T, n int) []T {
	if n >= len(items) {
		return append([]T(nil), items...)
	}
	pool := append([]T(nil), items...)
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	return pool[:n]
}

func acksFor(replicas int, mode string) int {
	switch mode {
	case "one":
		return 1
	case "all":
		return replicas
	default: // quorum
		return replicas/2 + 1
	}
}

func (s *ScatteredStore) PutPack(ctx context.Context, repo string, packOID model.OID, packBytes, idxBytes []byte) ([]string, error) {
	return putPackStreamFromBytes(ctx, s, repo, packOID, packBytes, idxBytes)
}

// PrefersWholePack is true on the cluster wire: each GetObject is an RPC
// round trip, so an amplified closure walk takes ONE whole-pack fetch +
// decode + memoize over thousands of per-object round trips.
func (s *ScatteredStore) PrefersWholePack() bool { return true }

// PutPackStream fans the pack out to R healthy nodes in parallel, each
// writer drawing its own reader of the same bytes (the spooled push file) —
// the pack never sits whole in gateway memory.
func (s *ScatteredStore) PutPackStream(ctx context.Context, repo string, packOID model.OID, size int64, open func() (io.ReadCloser, error), idxBytes []byte) ([]string, error) {
	healthy := s.healthyNodes(ctx)
	if len(healthy) == 0 {
		return nil, fmt.Errorf("scattered: no healthy storage nodes")
	}
	replicas := s.DefaultR
	if r, err := s.Refs.GetPlacement(ctx, repo); err == nil && r > 0 {
		replicas = r
	}
	if replicas > len(healthy) {
		replicas = len(healthy) // degrade: fewer nodes than target
	}
	targets := pickRandom(healthy, replicas)

	type result struct {
		node NodeInfo
		ok   bool
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t NodeInfo) {
			defer wg.Done()
			results[i] = result{node: t, ok: s.client(t.Addr).PutPackStream(ctx, repo, packOID, size, open, idxBytes) == nil}
		}(i, t)
	}
	wg.Wait()

	var confirmed []string
	for _, r := range results {
		if r.ok {
			confirmed = append(confirmed, r.node.ID)
		}
	}
	if need := acksFor(replicas, s.WriteAcks); len(confirmed) < need {
		return nil, fmt.Errorf("scattered: pack %s confirmed on %d/%d nodes (need %d)",
			packOID, len(confirmed), replicas, need)
	}
	return confirmed, nil
}

// teeCacheReader streams a node body into the gateway disk cache while the
// consumer reads it (write-through, one pass): EOF promotes the cache entry,
// an early Close aborts it. Cache errors never fail the serving stream.
type teeCacheReader struct {
	rc  io.ReadCloser
	put *CachePut
}

func (t *teeCacheReader) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 && t.put != nil {
		if _, werr := t.put.Write(p[:n]); werr != nil {
			t.put.Abort()
			t.put = nil
		}
	}
	if err == io.EOF && t.put != nil {
		_ = t.put.Complete()
		t.put = nil
	}
	return n, err
}

func (t *teeCacheReader) Close() error {
	if t.put != nil {
		t.put.Abort()
		t.put = nil
	}
	return t.rc.Close()
}

// OpenPack streams one pack without buffering it (exact-pack verbatim
// serve, docs/03 §5): gateway disk cache first (disk → consumer), else the
// first reachable holder's response body, tee'd into the disk cache as the
// bytes flow past.
func (s *ScatteredStore) OpenPack(ctx context.Context, repo string, packOID model.OID, holders []string) (io.ReadCloser, int64, error) {
	if s.DiskCache != nil {
		if f, size, ok := s.DiskCache.Open(repo, packOID); ok {
			return f, size, nil
		}
	}
	byID := map[string]string{}
	for _, n := range s.healthyNodes(ctx) {
		byID[n.ID] = n.Addr
	}
	for _, id := range holders {
		addr, ok := byID[id]
		if !ok {
			continue
		}
		rc, size, err := s.client(addr).OpenPack(ctx, repo, packOID)
		if err != nil {
			continue
		}
		if s.DiskCache != nil {
			if put, perr := s.DiskCache.BeginPut(repo, packOID); perr == nil {
				return &teeCacheReader{rc: rc, put: put}, size, nil
			}
		}
		return rc, size, nil
	}
	return nil, 0, errNoHolder(packOID, holders)
}

// PackSize reports a pack's byte size cheaply: cache index first, then a
// HEAD to the first reachable holder. -1 = unknown (old nodes) — callers
// treat unknown as "no guard" and keep legacy behavior.
func (s *ScatteredStore) PackSize(ctx context.Context, repo string, packOID model.OID, holders []string) (int64, error) {
	if s.DiskCache != nil {
		if size, ok := s.DiskCache.SizeHit(repo, packOID); ok {
			return size, nil
		}
	}
	byID := map[string]string{}
	for _, n := range s.healthyNodes(ctx) {
		byID[n.ID] = n.Addr
	}
	for _, id := range holders {
		addr, ok := byID[id]
		if !ok {
			continue
		}
		if size, err := s.client(addr).HeadPack(ctx, repo, packOID); err == nil {
			return size, nil
		}
	}
	return -1, nil
}

func (s *ScatteredStore) GetPack(ctx context.Context, repo string, packOID model.OID, holders []string) ([]byte, error) {
	// Disk cache first (docs/11 P2-1): content-addressed by pack OID, so a
	// hit is the pack and no node round trip is needed.
	if s.DiskCache != nil {
		if raw, ok := s.DiskCache.Get(repo, packOID); ok {
			return raw, nil
		}
	}
	// Resolve holder IDs to addresses through the health view (dead holders
	// may still be listed; their GET failing just falls through to the next).
	byID := map[string]string{}
	for _, n := range s.healthyNodes(ctx) {
		byID[n.ID] = n.Addr
	}
	var lastErr error
	for _, id := range holders {
		addr, ok := byID[id]
		if !ok {
			continue
		}
		raw, err := s.client(addr).GetPack(ctx, repo, packOID)
		if err == nil {
			if s.DiskCache != nil {
				_ = s.DiskCache.Put(repo, packOID, raw) // best-effort
			}
			return raw, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errNoHolder(packOID, holders)
}

// CheapPack: a disk-cache hit IS the pack (content-addressed, docs/11
// P2-1) — but only when small: a cached big pack is NOT cheap to load
// (whole-pack decode + memoize would park it in gateway memory), so
// GetCapped routes big packs back to the single-object path.
func (s *ScatteredStore) CheapPack(_ context.Context, repo string, packOID model.OID, _ []string) ([]byte, bool) {
	if s.DiskCache == nil {
		return nil, false
	}
	return s.DiskCache.GetCapped(repo, packOID, packMemoMaxBytes)
}

// GetObject single-object extraction with holder fallback (docs/11 P1-1):
// the holder resolves the object via its local idx — the wire carries one
// object instead of the whole pack. Any failure surfaces as an error; the
// caller falls back to GetPack.
func (s *ScatteredStore) GetObject(ctx context.Context, repo string, packOID, oid model.OID, holders []string, algo model.HashAlgo) (model.ObjectType, []byte, error) {
	byID := map[string]string{}
	for _, n := range s.healthyNodes(ctx) {
		byID[n.ID] = n.Addr
	}
	var lastErr error
	for _, id := range holders {
		addr, ok := byID[id]
		if !ok {
			continue
		}
		t, data, err := s.client(addr).GetObject(ctx, repo, packOID, oid, algo)
		if err == nil {
			return t, data, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", nil, lastErr
	}
	return "", nil, errNoHolder(packOID, holders)
}

func (s *ScatteredStore) DeletePack(ctx context.Context, repo string, packOID model.OID, holders []string) {
	byID := map[string]string{}
	for _, n := range s.healthyNodes(ctx) {
		byID[n.ID] = n.Addr
	}
	var wg sync.WaitGroup
	for _, id := range holders {
		addr, ok := byID[id]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			_ = s.client(addr).DeletePack(ctx, repo, packOID)
		}(addr)
	}
	wg.Wait()
}

// RenameRepo fans a directory move out to every node in the current health
// view (named addressing, docs/03 §6). Idempotent per node: a holder without
// the source answers OK, so a re-run after a partial failure only reaches
// laggards. Any refusal aborts before the registry flip at the caller.
func (s *ScatteredStore) RenameRepo(ctx context.Context, from, to string) error {
	var firstErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range s.healthyNodes(ctx) {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			if err := s.client(addr).RenameRepo(ctx, from, to); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("node %s: %w", addr, err)
				}
				mu.Unlock()
			}
		}(n.Addr)
	}
	wg.Wait()
	return firstErr
}

// DeleteRepo fans a storage-key drop out to every node in the current health
// view. Same contract as RenameRepo: idempotent per node, any refusal aborts
// before the registry drop at the caller.
func (s *ScatteredStore) DeleteRepo(ctx context.Context, repo string) error {
	var firstErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range s.healthyNodes(ctx) {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			if err := s.client(addr).DeleteRepo(ctx, repo); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("node %s: %w", addr, err)
				}
				mu.Unlock()
			}
		}(n.Addr)
	}
	wg.Wait()
	return firstErr
}
