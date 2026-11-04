// ha_cluster_test.go — T23-T26: the cluster deployment (docs/10) end to end
// with the real git CLI as the client. The harness starts the actual role
// servers in-process: a refs service (SQLite meta, tight health thresholds),
// four storage nodes (storenode over LocalShardStore) and one gateway
// (gitwire + rpc over RemoteMeta + ScatteredStore). Health ticks and
// reconciler passes are driven directly for determinism.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/refs"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/storenode"
)

// haCluster is the in-process cluster harness: refs + N storage nodes + one
// gateway, exactly the role wiring of cmd/banyand.
type haCluster struct {
	t        *testing.T
	dbPath   string
	Meta     *storage.SQLiteMetaStore // direct handle for assertions
	RefsSrv  *refs.Server
	refsLn   net.Listener
	refsAddr string // host:port, reused across refs restarts (T26)
	refsHTTP *http.Server
	RefsURL  string
	Storage  map[string]*httptest.Server // nodeID → server (nil after kill)
	Roots    map[string]string           // nodeID → shard root (T40 disk assertions)
	beats    map[string]context.CancelFunc // nodeID → heartbeat loop cancel
	GW       *httptest.Server
	cancel   context.CancelFunc
	mode     string // storage addressing mode (docs/03 §6): "" = named

	// Per-node internal request counters (T36: gateway disk-cache effect).
	packGets atomic.Int32 // GET /internal/pack
	objGets  atomic.Int32 // GET /internal/object
}

var haNodeSeq atomic.Int32

// startHACluster boots refs + N storage nodes + one gateway (named
// addressing). startHAClusterMode covers hashed addressing (T40).
func startHACluster(t *testing.T, nodes int) *haCluster {
	return startHAClusterMode(t, nodes, "")
}

func startHAClusterMode(t *testing.T, nodes int, mode string) *haCluster {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &haCluster{
		t:       t,
		Storage: map[string]*httptest.Server{},
		Roots:   map[string]string{},
		beats:   map[string]context.CancelFunc{},
		cancel:  cancel,
		mode:    mode,
	}

	// refs service on a dedicated listener (restartable in T26).
	c.dbPath = filepath.Join(t.TempDir(), "refs.db")
	c.startRefs(ctx)
	t.Cleanup(c.shutdown)

	for i := 0; i < nodes; i++ {
		c.startStorageNode(ctx)
	}

	// gateway: gitwire + rpc over RemoteMeta + ScatteredStore.
	refsClient := storage.NewRefsClient(c.RefsURL)
	rmeta := &storage.RemoteMeta{Refs: refsClient}
	backend := storage.NewScatteredStore(refsClient, 3, "quorum")
	objects := storage.NewObjectServiceWithBackend(rmeta, backend, model.SHA1)
	objects.Addressing = storage.NewAddressing(c.mode, rmeta)
	gmux := http.NewServeMux()
	gitwire.NewServer(rmeta, objects, model.SHA1).Register(gmux)
	rpc.NewServer(rmeta, objects, model.SHA1).Register(gmux)
	c.GW = httptest.NewServer(gmux)
	return c
}

func (c *haCluster) startRefs(ctx context.Context) {
	c.t.Helper()
	if c.Meta != nil {
		_ = c.Meta.Close() // restart path: release the sqlite handle (Windows file lock)
	}
	meta, err := storage.NewSQLiteMetaStore(c.dbPath)
	require.NoError(c.t, err)
	c.Meta = meta
	c.RefsSrv = refs.NewServer(meta, refs.Options{
		DefaultReplicas:  3,
		SuspectAfter:     400 * time.Millisecond,
		DeadAfter:        1000 * time.Millisecond,
		ReconInterval:    time.Hour, // background loop idle; tests drive RunOnce
		ReconConcurrency: 4,
		AddressingMode:   c.mode,
	})
	if c.refsAddr == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(c.t, err)
		c.refsAddr = ln.Addr().String()
		c.refsLn = ln
	} else {
		// restart path: re-bind the same address (listener was closed)
		var ln net.Listener
		var err error
		for i := 0; i < 40; i++ {
			ln, err = net.Listen("tcp", c.refsAddr)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		require.NoError(c.t, err, "refs 重启应重新绑定同一地址")
		c.refsLn = ln
	}
	mux := http.NewServeMux()
	c.RefsSrv.Register(mux)
	c.RefsSrv.RunBackground(ctx)
	c.refsHTTP = &http.Server{Handler: mux}
	go func() { _ = c.refsHTTP.Serve(c.refsLn) }()
	c.RefsURL = "http://" + c.refsAddr
}

func (c *haCluster) startStorageNode(ctx context.Context) {
	c.t.Helper()
	id := fmt.Sprintf("s%d", haNodeSeq.Add(1))
	// The advertise address must exist before the node is built, so bind the
	// listener first and serve on it explicitly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(c.t, err)
	root := c.t.TempDir()
	node := storenode.New(id, root, c.RefsURL, ln.Addr().String())
	mux := http.NewServeMux()
	node.Register(mux)
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/internal/pack":
				c.packGets.Add(1)
			case "/internal/object":
				c.objGets.Add(1)
			}
		}
		mux.ServeHTTP(w, r)
	})
	ts := &httptest.Server{Listener: ln, Config: &http.Server{Handler: counting}}
	ts.Start()
	// The node's test lifetime is the HTTP server AND its heartbeat loop —
	// killNode cancels both (a real deployment loses the whole process).
	nctx, ncancel := context.WithCancel(ctx)
	go node.HeartbeatLoop(nctx, 100*time.Millisecond)
	c.Storage[id] = ts
	c.Roots[id] = root
	c.beats[id] = ncancel
}

// restartGateway replaces the gateway process state (fresh ObjectService,
// cold memory) — optionally carrying the on-disk pack cache across (T36:
// the cache is the only state a gateway is allowed to keep, docs/11 P2-1).
func (c *haCluster) restartGateway(cacheDir string) {
	c.t.Helper()
	if c.GW != nil {
		c.GW.Close()
	}
	refsClient := storage.NewRefsClient(c.RefsURL)
	rmeta := &storage.RemoteMeta{Refs: refsClient}
	backend := storage.NewScatteredStore(refsClient, 3, "quorum")
	if cacheDir != "" {
		cache, err := storage.NewDiskPackCache(cacheDir, 1<<30)
		require.NoError(c.t, err)
		backend.DiskCache = cache
	}
	objects := storage.NewObjectServiceWithBackend(rmeta, backend, model.SHA1)
	objects.Addressing = storage.NewAddressing(c.mode, rmeta)
	gmux := http.NewServeMux()
	gitwire.NewServer(rmeta, objects, model.SHA1).Register(gmux)
	rpc.NewServer(rmeta, objects, model.SHA1).Register(gmux)
	c.GW = httptest.NewServer(gmux)
}

func (c *haCluster) shutdown() {
	c.cancel()
	for id, ncancel := range c.beats {
		ncancel()
		delete(c.beats, id)
	}
	if c.refsHTTP != nil {
		_ = c.refsHTTP.Close()
	}
	if c.Meta != nil {
		_ = c.Meta.Close() // release the sqlite file before TempDir cleanup
	}
	for _, ts := range c.Storage {
		if ts != nil {
			ts.Close()
		}
	}
	if c.GW != nil {
		c.GW.Close()
	}
}

// killNode stops one live storage node deterministically and waits for the
// health table to mark it dead.
func (c *haCluster) killNode() string {
	c.t.Helper()
	for id, ts := range c.Storage {
		if ts == nil {
			continue
		}
		ts.CloseClientConnections()
		ts.Close()
		c.Storage[id] = nil
		if ncancel, ok := c.beats[id]; ok {
			ncancel()
			delete(c.beats, id)
		}
		time.Sleep(1500 * time.Millisecond) // > dead threshold with heartbeats stopped
		c.RefsSrv.Health.Tick()
		for _, st := range c.RefsSrv.Health.Snapshot() {
			if st.ID == id {
				require.Equal(c.t, refs.StateDead, st.State, "节点 %s 应在阈值后标记 dead", id)
			}
		}
		return id
	}
	c.t.Fatal("no live storage node to kill")
	return ""
}

func (c *haCluster) addrOf(id string) string {
	c.t.Helper()
	for _, st := range c.RefsSrv.Health.Snapshot() {
		if st.ID == id {
			return st.Addr
		}
	}
	c.t.Fatal("unknown node " + id)
	return ""
}

func (c *haCluster) putPlacement(t *testing.T, repo string, replicas int) {
	t.Helper()
	body := fmt.Sprintf(`{"replicas":%d}`, replicas)
	req, _ := http.NewRequest(http.MethodPut, c.gwURL()+"/api/v1/repos/"+repo+"/placement", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "placement PUT 失败")
}

func (c *haCluster) everyPackHolders(t *testing.T, repo string) map[string][]string {
	t.Helper()
	packs, err := c.Meta.ListPacks(context.Background(), repo)
	require.NoError(t, err)
	out := map[string][]string{}
	for _, p := range packs {
		out[string(p.PackOID)] = p.Nodes
	}
	return out
}

func (c *haCluster) gwURL() string { return c.GW.URL }

// asServer adapts the gateway to the shared helpers (repoURL etc.).
func (c *haCluster) asServer() *server { return &server{Server: c.GW} }

// --- T23: scattered placement; registration is the fact ---

func TestHAClusterScatteredPlacement(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 4)
	seed := seedRepo(t, c.asServer(), "ha1")

	packs := c.everyPackHolders(t, "ha1")
	require.NotEmpty(t, packs, "push 后应有 pack 注册")
	for packOID, holders := range packs {
		assert.Len(t, holders, 3, "R=3：pack %s 持有集应为 3（实际 %v）", packOID, holders)
		assert.Len(t, dedup(holders), 3, "持有集应无重复")
		for _, id := range holders {
			addr := c.addrOf(id)
			raw, err := storage.NewNodeClient("http://" + addr).GetPack(context.Background(), "ha1", model.OID(packOID))
			require.NoError(t, err, "任一持有节点应能服务 pack %s（节点 %s）", packOID, id)
			assert.True(t, storage.VerifyPackChecksum(model.OID(packOID), raw), "pack 校验和应通过")
		}
	}

	// The git view through the gateway is complete and correct.
	g := cloneRepo(t, c.asServer(), c.gwURL()+"/r/ha1.git")
	assert.Equal(t, "readme-v1\n", readFile(t, filepath.Join(g, "README.md")))
	assert.Equal(t, 1, countCommits(t, g))

	// Incremental push lands on a fresh random placement.
	write(t, filepath.Join(seed, "lib", "a.txt"), "a-v2\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "ha1: second")
	git(t, seed, "push", "-q", c.gwURL()+"/r/ha1.git", "main")
	assert.Equal(t, 2, countCommits(t, cloneRepo(t, c.asServer(), c.gwURL()+"/r/ha1.git")))

	// Cluster status endpoint reports four healthy nodes.
	resp, err := http.Get(c.gwURL() + "/api/v1/cluster/nodes")
	require.NoError(t, err)
	defer resp.Body.Close()
	var cs struct {
		Cluster bool `json:"cluster"`
		Nodes   []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"nodes"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cs))
	assert.True(t, cs.Cluster)
	assert.Len(t, cs.Nodes, 4)
	for _, n := range cs.Nodes {
		assert.Equal(t, "healthy", n.State)
	}
}

// --- T24: node failure → reads and writes continue; reconciler restores R ---

func TestHANodeFailureAndRecovery(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 4)
	seed := seedRepo(t, c.asServer(), "ha2")
	write(t, filepath.Join(seed, "lib", "a.txt"), "a-v2\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "ha2: second")
	git(t, seed, "push", "-q", c.gwURL()+"/r/ha2.git", "main")

	deadID := c.killNode()

	// Writes continue: a new push avoids (or quorum-masks) the dead node.
	write(t, filepath.Join(seed, "lib", "a.txt"), "a-v3\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "ha2: during failure")
	git(t, seed, "push", "-q", c.gwURL()+"/r/ha2.git", "main")

	// Reads continue: a fresh clone carries all three commits.
	g := cloneRepo(t, c.asServer(), c.gwURL()+"/r/ha2.git")
	assert.Equal(t, "a-v3\n", readFile(t, filepath.Join(g, "lib", "a.txt")))
	assert.Equal(t, 3, countCommits(t, g))

	// Reconciler restores the replica target for packs that lost a holder.
	res := c.RefsSrv.Recon.RunOnce(context.Background())
	assert.Greater(t, res.Copied, 0, "恢复应至少复制一个 pack（UnderReplicated=%d）", res.UnderReplicated)
	for packOID, holders := range c.everyPackHolders(t, "ha2") {
		assert.Len(t, holders, 3, "恢复后 pack %s 应回到 3 副本（实际 %v）", packOID, holders)
		assert.NotContains(t, holders, deadID, "dead 节点应已移出持有集")
	}
	// And the repository still clones after the convergence pass.
	g2 := cloneRepo(t, c.asServer(), c.gwURL()+"/r/ha2.git")
	assert.Equal(t, "a-v3\n", readFile(t, filepath.Join(g2, "lib", "a.txt")))
}

// --- T25: dynamic per-repo replica adjustment (3 → 2 → 3) ---

func TestHAPlacementDynamic(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 4)
	seedRepo(t, c.asServer(), "ha3")

	for packOID, holders := range c.everyPackHolders(t, "ha3") {
		require.Len(t, holders, 3, "初始 R=3（pack %s: %v）", packOID, holders)
	}

	// Lower the target to 2: the reconciler trims (register first, delete last).
	c.putPlacement(t, "ha3", 2)
	res := c.RefsSrv.Recon.RunOnce(context.Background())
	assert.Greater(t, res.Trimmed, 0, "R 3→2 应触发剪枝")
	for packOID, holders := range c.everyPackHolders(t, "ha3") {
		assert.Len(t, holders, 2, "R=2 后 pack %s 持有集应为 2（实际 %v）", packOID, holders)
	}
	// Reads still fine after trimming.
	g := cloneRepo(t, c.asServer(), c.gwURL()+"/r/ha3.git")
	assert.Equal(t, "readme-v1\n", readFile(t, filepath.Join(g, "README.md")))

	// Raise back to 3: under-replicated packs get a fresh holder.
	c.putPlacement(t, "ha3", 3)
	res = c.RefsSrv.Recon.RunOnce(context.Background())
	assert.Greater(t, res.Copied, 0, "R 2→3 应触发补齐")
	for packOID, holders := range c.everyPackHolders(t, "ha3") {
		assert.Len(t, holders, 3, "R=3 后 pack %s 持有集应为 3（实际 %v）", packOID, holders)
	}
}

// --- T26: refs restart keeps committed state (SQLite RPO=0) ---

func TestHARefsRestart(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 4)
	seed := seedRepo(t, c.asServer(), "ha4")
	write(t, filepath.Join(seed, "lib", "a.txt"), "a-v2\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "ha4: before restart")
	git(t, seed, "push", "-q", c.gwURL()+"/r/ha4.git", "main")

	// Stop the refs service and restart it on the same address + database.
	_ = c.refsHTTP.Close()
	time.Sleep(100 * time.Millisecond)
	c.startRefs(context.Background())

	// The health table is empty after restart; heartbeats re-register the
	// nodes within one interval.
	deadline := time.Now().Add(3 * time.Second)
	for len(c.RefsSrv.Health.Healthy()) < 4 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	require.Len(t, c.RefsSrv.Health.Healthy(), 4, "重启后节点应经心跳重新注册")

	// Push continues (auto-created repo and timeline heads survived), and a
	// fresh clone sees the pre-restart commit.
	write(t, filepath.Join(seed, "lib", "a.txt"), "a-v3\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "ha4: after restart")
	git(t, seed, "push", "-q", c.gwURL()+"/r/ha4.git", "main")
	g := cloneRepo(t, c.asServer(), c.gwURL()+"/r/ha4.git")
	assert.Equal(t, "a-v3\n", readFile(t, filepath.Join(g, "lib", "a.txt")))
	assert.Equal(t, 3, countCommits(t, g))
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// --- T36 gateway disk pack cache (docs/11 P2-1, B6) ---
// The gateway is stateless except for its caches — all memory. A restarted
// gateway's first clone re-paid every pack transfer and (post-B2) every
// object read as per-object RPCs. With cache.pack_dir, whole packs persist
// on disk: after one warm-up clone, a full gateway restart serves the next
// clone entirely from local state — the node request counters freeze.
func TestHAGatewayDiskCache(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 3)
	s := c.asServer()
	src := seedRepo(t, s, "t36")
	git(t, src, "push", "-q", c.gwURL()+"/r/t36.git", "main")

	// Warm-up: a clone through a cache-less gateway pulls pack(s) and object
	// reads from the nodes (whatever the counters reach, they are non-zero).
	fresh1 := cloneRepo(t, s, s.repoURL("t36"))
	fsckClean(t, fresh1)
	packAfterWarm := c.packGets.Load()
	objAfterWarm := c.objGets.Load()
	require.Greater(t, packAfterWarm+objAfterWarm, int32(0), "预热克隆应有节点请求")

	// First restart WITH the cache: the clone fills it (pack transfer lands
	// on disk; object reads may still cross).
	cacheDir := filepath.Join(t.TempDir(), "gw-cache")
	c.restartGateway(cacheDir)
	fresh2 := cloneRepo(t, s, c.gwURL()+"/r/t36.git")
	fsckClean(t, fresh2)
	require.True(t, hasCommit(t, fresh2, headOf(t, src)))
	packAfterFill := c.packGets.Load()
	objAfterFill := c.objGets.Load()

	// Second restart on the SAME cache directory: the clone must be served
	// without a single node request — object reads come from the cheap
	// whole-pack path (disk cache → decode → memoize), the response pack
	// from the cache.
	c.restartGateway(cacheDir)
	fresh3 := cloneRepo(t, s, c.gwURL()+"/r/t36.git")
	fsckClean(t, fresh3)
	require.True(t, hasCommit(t, fresh3, headOf(t, src)))
	assert.Equal(t, packAfterFill, c.packGets.Load(), "缓存命中后不应再有整包节点请求")
	assert.Equal(t, objAfterFill, c.objGets.Load(), "缓存命中后不应再有单对象节点请求")
}

// TestHACorruptReplicaFallback (review fix E): a corrupted pack file on one
// holder must not surface as a mid-decode failure or poison the gateway
// disk cache — the buffered whole-pack path trailer-verifies each holder
// and falls through to a healthy replica, and the streaming tee drops its
// cache entry on a trailer mismatch.
func TestHACorruptReplicaFallback(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 3)
	s := c.asServer()
	src := seedRepo(t, s, "t41")
	git(t, src, "push", "-q", c.gwURL()+"/r/t41.git", "main")

	// One repo pack, registered with its holders; corrupt it on the FIRST
	// holder (objects.node_id points there — reads try it first).
	packs, err := c.Meta.ListPacks(context.Background(), "t41")
	require.NoError(t, err)
	require.NotEmpty(t, packs)
	first := packs[0]
	require.NotEmpty(t, first.Nodes)

	corrupt := func(nodeID string, oid model.OID) {
		packFile := filepath.Join(c.Roots[nodeID], "t41", "packs", string(oid[:2]), "pack-"+string(oid)+".pack")
		raw, rerr := os.ReadFile(packFile)
		require.NoError(t, rerr)
		require.Greater(t, len(raw), 64)
		raw[len(raw)/2] ^= 0xff // same-length flip: only a checksum can catch it
		require.NoError(t, os.WriteFile(packFile, raw, 0o644))
	}

	// Case 1: buffered whole-pack read (fresh gateway, cold memory) — the
	// corrupt first holder is skipped in favor of a healthy replica. The
	// clone is the oracle: without the fallback the gateway fetches the
	// corrupt bytes, the trailer check kills the decode, the clone dies.
	corrupt(first.Nodes[0], first.PackOID)
	fresh := cloneRepo(t, s, c.gwURL()+"/r/t41.git")
	fsckClean(t, fresh)
	require.True(t, hasCommit(t, fresh, headOf(t, src)))

	// Case 2: one healthy replica left — reads still succeed, and the
	// cache-enabled gateway keeps serving (the tee drop-on-mismatch is
	// pinned by unit tests on the reader itself).
	cacheDir := filepath.Join(t.TempDir(), "gw-cache")
	c.restartGateway(cacheDir)
	if len(first.Nodes) > 1 {
		corrupt(first.Nodes[1], first.PackOID)
	}
	fresh2 := cloneRepo(t, s, c.gwURL()+"/r/t41.git")
	fsckClean(t, fresh2)
	require.True(t, hasCommit(t, fresh2, headOf(t, src)))
}

// renameViaRPC drives the production rename path (rpc handler →
// ObjectService.RenameRepo → fence, node moves, registry flip).
func (c *haCluster) renameViaRPC(t *testing.T, from, to string) {
	t.Helper()
	resp, err := http.Post(c.gwURL()+"/api/v1/repos/"+from+"/rename",
		"application/json", strings.NewReader(`{"to":"`+to+`"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "rename %s→%s 应走通生产路径", from, to)
}

// TestHARenameWriteBarrier (review fix F): a push that races a rename must
// either fail cleanly (client retries under the settled name) or complete
// before the fence — never land a confirmed-but-invisible write under the
// dying name. Deterministic interleave: hold the rename in its move window
// (fence installed), attempt the push, assert rejection and an unchanged
// registry, then complete the rename through the production path.
func TestHARenameWriteBarrier(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 3)
	s := c.asServer()
	ctx := context.Background()
	src := seedRepo(t, s, "t42")
	seedHead := headOf(t, src)

	packsBefore, err := c.Meta.ListPacks(ctx, "t42")
	require.NoError(t, err)
	tl := model.Timeline{Repo: "t42", Anchor: model.RootAnchor, Name: "main"}
	headBefore, err := c.Meta.Timeline(ctx, tl)
	require.NoError(t, err)

	// Hold the rename mid-flight: the fence is up, the node moves have not
	// run — exactly the window a concurrent push would slip into.
	require.NoError(t, c.Meta.BeginRename(ctx, "t42", "t42n"))

	appendLine(t, filepath.Join(src, "README.md"), "racing-push\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "racing push during rename")
	if out, perr := gitTry(src, "push", "-q", c.gwURL()+"/r/t42.git", "main"); perr == nil {
		t.Fatalf("围栏期间的 push 必须失败（干净拒绝而非已确认不可见写）\n%s", out)
	}

	// The rejected push left nothing behind: same packs, same head.
	packsAfter, err := c.Meta.ListPacks(ctx, "t42")
	require.NoError(t, err)
	assert.Equal(t, len(packsBefore), len(packsAfter), "被拒 push 不得注册 pack")
	headAfter, err := c.Meta.Timeline(ctx, tl)
	require.NoError(t, err)
	assert.Equal(t, headBefore.Head, headAfter.Head, "被拒 push 不得移动 ref")

	// Complete the rename through the production path: BeginRename
	// re-installs the same fence idempotently, the nodes move, the flip
	// lifts it. The pre-rename data must answer under the new name.
	c.renameViaRPC(t, "t42", "t42n")
	renamed := cloneRepo(t, s, c.gwURL()+"/r/t42n.git")
	fsckClean(t, renamed)
	require.True(t, hasCommit(t, renamed, seedHead), "rename 后旧数据应在新名下完整可见")

	// Fence lifted: writes land under the new name; the old name is gone.
	git(t, src, "push", "-q", c.gwURL()+"/r/t42n.git", "main")
	_, err = gitTry("", "ls-remote", c.gwURL()+"/r/t42.git")
	assert.Error(t, err, "旧名字在翻转后不应再存在")
}

// TestHARenameVsPushRace: the statistical guard over the same invariant —
// rename and push run concurrently; whatever the interleaving, an ACKED
// push is fully visible under the settled name (never silently lost).
func TestHARenameVsPushRace(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 3)
	s := c.asServer()
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("t42r%d", i)
		src := seedRepo(t, s, name)
		seedHead := headOf(t, src)
		appendLine(t, filepath.Join(src, "README.md"), fmt.Sprintf("race-%d\n", i))
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", "concurrent with rename")

		pushErr := make(chan error, 1)
		go func() {
			_, err := gitTry(src, "push", "-q", c.gwURL()+"/r/"+name+".git", "main")
			pushErr <- err
		}()
		c.renameViaRPC(t, name, name+"n")
		err := <-pushErr

		renamed := cloneRepo(t, s, c.gwURL()+"/r/"+name+"n.git")
		fsckClean(t, renamed)
		if err != nil {
			// Rejected push: only the seed is visible; the client would retry.
			assert.Equal(t, 1, countCommits(t, renamed), "被拒 push 的提交不应出现")
			require.True(t, hasCommit(t, renamed, seedHead))
		} else {
			// Acked push: BOTH commits visible — the barrier's core promise.
			assert.Equal(t, 2, countCommits(t, renamed), "已确认 push 不得静默丢失（i=%d）", i)
		}
	}
}

// nodePackPath builds the on-disk path of one pack on one node (fixture
// reaches straight into the shard root — the sweep's oracle view).
func nodePackPath(c *haCluster, nodeID, key string, oid model.OID) string {
	return filepath.Join(c.Roots[nodeID], key, "packs", string(oid[:2]), "pack-"+string(oid)+".pack")
}

// ageFile backdates a file for the sweep grace window.
func ageFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	require.NoError(t, os.Chtimes(path, old, old))
}

// TestHAFleetSweepOrphans (review fix D): bytes no registry row vouches for
// are reclaimed by the fleet sweep, while live data and young (possibly
// in-flight) files are untouched. Scenarios: an unregistered pack, a pack
// registered but no longer held on this node (a trim whose delete failed),
// a whole storage key no repo resolves to, crashed-landing tmp files — and
// the grace that protects a fresh landing.
func TestHAFleetSweepOrphans(t *testing.T) {
	needGit(t)
	c := startHACluster(t, 3)
	s := c.asServer()
	ctx := context.Background()
	src := seedRepo(t, s, "t43")
	liveHead := headOf(t, src)

	packs, err := c.Meta.ListPacks(ctx, "t43")
	require.NoError(t, err)
	require.NotEmpty(t, packs)
	liveOID := packs[0].PackOID
	holder := packs[0].Nodes[0]

	// (a) Unregistered pack: real bytes under an OID the registry never saw.
	orphanOID := model.OID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	raw, err := os.ReadFile(nodePackPath(c, holder, "t43", liveOID))
	require.NoError(t, err)
	orphanPath := nodePackPath(c, holder, "t43", orphanOID)
	require.NoError(t, os.MkdirAll(filepath.Dir(orphanPath), 0o755))
	require.NoError(t, os.WriteFile(orphanPath, raw, 0o644))
	ageFile(t, orphanPath, 2*time.Hour)

	// (b) Young unregistered pack: inside the grace — must survive (a
	// byte-land → register span is milliseconds; an hour is the margin).
	youngOID := model.OID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	youngPath := nodePackPath(c, holder, "t43", youngOID)
	require.NoError(t, os.MkdirAll(filepath.Dir(youngPath), 0o755))
	require.NoError(t, os.WriteFile(youngPath, raw, 0o644)) // mtime = now

	// (c) Unheld pack: registry trimmed the holder set but the node delete
	// failed — bytes remain on the trimmed node.
	var keep []string
	for _, id := range packs[0].Nodes {
		if id != holder {
			keep = append(keep, id)
		}
	}
	require.NoError(t, c.Meta.SetPackNodes(ctx, "t43", liveOID, keep))
	ageFile(t, nodePackPath(c, holder, "t43", liveOID), 2*time.Hour)

	// (d) Whole-key orphan: a storage key no repo resolves to.
	ghostOID := model.OID("cccccccccccccccccccccccccccccccccccccccc")
	ghostPath := nodePackPath(c, holder, "t43ghost", ghostOID)
	require.NoError(t, os.MkdirAll(filepath.Dir(ghostPath), 0o755))
	require.NoError(t, os.WriteFile(ghostPath, raw, 0o644))
	ageFile(t, ghostPath, 2*time.Hour)

	// (e) Crashed-landing tmp leftover.
	tmpPath := filepath.Join(c.Roots[holder], "t43", "packs", ".put-crashed")
	require.NoError(t, os.WriteFile(tmpPath, raw, 0o644))
	ageFile(t, tmpPath, 2*time.Hour)

	// Dry run: counts the reclaim, deletes nothing.
	dry := c.RefsSrv.Recon.SweepOnce(ctx, refs.SweepOptions{MinAge: time.Hour, DryRun: true})
	assert.Equal(t, 3, dry.KeysDropped+dry.PacksDropped, "dry run 应只计数不删除")
	assert.Equal(t, 1, dry.KeysDropped)
	assert.Equal(t, 2, dry.PacksDropped)
	_, err = os.Stat(orphanPath)
	require.NoError(t, err, "dry run 不得删除")

	// Real pass through the HTTP endpoint.
	resp, err := http.Post(c.RefsURL+"/internal/sweep", "application/json",
		strings.NewReader(`{"min_age_sec":3600}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var res refs.SweepResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&res))
	resp.Body.Close()
	assert.Equal(t, 1, res.KeysDropped, "ghost key 应被整键回收")
	assert.Equal(t, 2, res.PacksDropped, "孤儿 pack + 去持有化 pack 应被回收")

	_, err = os.Stat(orphanPath)
	assert.True(t, os.IsNotExist(err), "孤儿 pack 应被清扫")
	_, err = os.Stat(nodePackPath(c, holder, "t43", liveOID))
	assert.True(t, os.IsNotExist(err), "被修剪节点的 pack 应被清扫")
	_, err = os.Stat(filepath.Join(c.Roots[holder], "t43ghost"))
	assert.True(t, os.IsNotExist(err), "ghost key 目录应被清扫")
	_, err = os.Stat(tmpPath)
	assert.True(t, os.IsNotExist(err), "崩溃残留 tmp 应被清扫")
	_, err = os.Stat(youngPath)
	require.NoError(t, err, "grace 内的新落地文件必须保留")

	// Live data untouched: other holders still serve the seed pack.
	fresh := cloneRepo(t, s, c.gwURL()+"/r/t43.git")
	fsckClean(t, fresh)
	require.True(t, hasCommit(t, fresh, liveHead))
}

// TestHAFleetSweepHashedDeletedRepo: hashed addressing keeps a deleted
// repo's immutable storage key (docs/03 §6) — the fleet sweep is the
// promised reclamation. After the sweep the @hashed trees are gone on
// every node while the live repo's key stays.
func TestHAFleetSweepHashedDeletedRepo(t *testing.T) {
	needGit(t)
	c := startHAClusterMode(t, 3, "hashed")
	s := c.asServer()
	ctx := context.Background()
	seedRepo(t, s, "t44h")
	staysSrc := seedRepo(t, s, "t44h-stays")
	staysHead := headOf(t, staysSrc)

	// Resolve both keys BEFORE the delete — the dead repo's rows go away
	// with it, but the key itself is what the sweep judges by.
	addr := storage.NewAddressing("hashed", c.Meta)
	deadKey, err := addr.Key(ctx, "t44h")
	require.NoError(t, err)
	liveKey, err := addr.Key(ctx, "t44h-stays")
	require.NoError(t, err)
	require.NotEqual(t, deadKey, liveKey)

	// The hashed-mode delete: registry rows only, the immutable key stays
	// (ObjectService.DeleteRepo's hashed branch).
	require.NoError(t, c.Meta.DeleteRepo(ctx, "t44h"))

	// Both keys still on every node; the dead one gets old enough to sweep.
	hashed := 0
	for _, root := range c.Roots {
		key := filepath.Join(root, "@hashed")
		_ = filepath.WalkDir(key, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || d.Name() != "packs" {
				return nil
			}
			hashed++
			parent := filepath.Dir(path)
			_ = filepath.WalkDir(parent, func(p string, f os.DirEntry, e error) error {
				if e == nil && !f.IsDir() && strings.HasPrefix(f.Name(), "pack-") {
					ageFile(t, p, 2*time.Hour)
				}
				return nil
			})
			return nil
		})
	}
	require.Equal(t, 6, hashed, "两仓库 × 三节点应有 6 个 key 目录")

	res := c.RefsSrv.Recon.SweepOnce(ctx, refs.SweepOptions{MinAge: time.Hour})
	assert.Equal(t, 3, res.KeysDropped, "被删仓库的 3 个副本 key 应被整键回收")

	for id, root := range c.Roots {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(deadKey)))
		assert.True(t, os.IsNotExist(err), "节点 %s 上被删仓库的 key 应被回收", id)
		_, err = os.Stat(filepath.Join(root, filepath.FromSlash(liveKey), "packs"))
		assert.NoError(t, err, "节点 %s 上存活仓库的 key 必须保留", id)
	}

	// The live repo answers after the sweep (its key was never a candidate).
	fresh := cloneRepo(t, s, c.gwURL()+"/r/t44h-stays.git")
	fsckClean(t, fresh)
	require.True(t, hasCommit(t, fresh, staysHead))
}
