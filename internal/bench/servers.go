package bench

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// counter accumulates bytes per direction across a scenario window.
type counter struct {
	mu        sync.Mutex
	reqBytes  int64 // client → server (pushes, requests)
	respBytes int64 // server → client (clone/fetch downloads)
}

func (c *counter) addReq(n int64)  { c.mu.Lock(); c.reqBytes += n; c.mu.Unlock() }
func (c *counter) addResp(n int64) { c.mu.Lock(); c.respBytes += n; c.mu.Unlock() }
func (c *counter) take() (req, resp int64) {
	c.mu.Lock()
	req, resp = c.reqBytes, c.respBytes
	c.reqBytes, c.respBytes = 0, 0
	c.mu.Unlock()
	return req, resp
}

type countingRW struct {
	http.ResponseWriter
	c *counter
}

func (w *countingRW) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.c.addResp(int64(n))
	return n, err
}

// countingHandler wraps a server and counts request body bytes in, response
// bytes out (wire-level accounting, headers excluded — same accounting for
// both banyan and the git baseline).
func countingHandler(next http.Handler, c *counter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > 0 {
			c.addReq(r.ContentLength)
		}
		next.ServeHTTP(&countingRW{ResponseWriter: w, c: c}, r)
	})
}

// BanyanServer is one in-process banyan stack (all-in-one role) behind the
// counting proxy — a "cold" server is simply a fresh instance after the seed
// push, exactly the state a restarted deployment boots into.
type BanyanServer struct {
	URL  string
	ctr  *counter
	srv  *httptest.Server
	meta *meta.MemoryMetaStore
}

func StartBanyan(shardRoot string) (*BanyanServer, error) {
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(shardRoot)
	nodes := []storage.Node{{ID: "b1", Local: true}}
	objects := storage.NewObjectService(ms, local, nodes, model.SHA1)
	objects.AutoConsolidate = true // deployment parity: post-materialization consolidation joins the synth and push packs
	// BANYAN_BENCH_CONSOLIDATE_MAX (bytes) raises Consolidate's per-group
	// byte cap — the A5 experiment for 发现⑤ (docs/operations/performance.md
	// §3b): the 256MiB default splits git.git's [134MiB synth, 310MiB push]
	// pair into two packs, no single pack covers the clone closure, and the
	// superset ladder dies into an unbounded online re-encode. Diagnostics
	// knob, not a tuning recommendation.
	if v := os.Getenv("BANYAN_BENCH_CONSOLIDATE_MAX"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			objects.ConsolidateMaxBytes = n
		}
	}
	mux := http.NewServeMux()
	gitwire.NewServer(ms, objects, model.SHA1).Register(mux)
	rpc.NewServer(ms, objects, model.SHA1).Register(mux)
	ctr := &counter{}
	srv := httptest.NewServer(countingHandler(mux, ctr))
	return &BanyanServer{URL: srv.URL, ctr: ctr, srv: srv, meta: ms}, nil
}

// SetDefaultBranch re-points the repo's advertised HEAD symref — real-sample
// mode needs it after the seed push (the auto-created repo defaults to main;
// git.git for one says master).
func (b *BanyanServer) SetDefaultBranch(repo, branch string) error {
	return b.meta.SetDefaultBranch(context.Background(), repo, branch)
}

func (b *BanyanServer) Stop() { b.srv.Close() }
func (b *BanyanServer) take() (int64, int64) {
	req, resp := b.ctr.take()
	return req, resp
}

// GitCGIServer serves a bare repo through the real `git http-backend` CGI —
// the peer baseline on the identical smart-HTTP transport. Every request
// spawns the CGI process (stateless by design; "warm" for the baseline is OS
// page cache, which is honest for what stock git hosting looks like here).
type GitCGIServer struct {
	URL         string
	ctr         *counter
	srv         *httptest.Server
	projectRoot string
	execPath    string
}

func StartGitCGI(projectRoot string) (*GitCGIServer, error) {
	out, err := git("", "--exec-path")
	if err != nil {
		return nil, err
	}
	execPath := strings.TrimSpace(out)
	ctr := &counter{}
	g := &GitCGIServer{ctr: ctr, projectRoot: projectRoot, execPath: execPath}
	mux := http.NewServeMux()
	mux.HandleFunc("/", g.serve)
	srv := httptest.NewServer(countingHandler(mux, ctr))
	g.URL = srv.URL
	g.srv = srv
	return g, nil
}

func (g *GitCGIServer) Stop() { g.srv.Close() }
func (g *GitCGIServer) take() (int64, int64) {
	req, resp := g.ctr.take()
	return req, resp
}

func (g *GitCGIServer) serve(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command(g.execPath + string(os.PathSeparator) + "git-http-backend")
	cmd.Env = append(os.Environ(),
		"GIT_PROJECT_ROOT="+g.projectRoot,
		"GIT_HTTP_EXPORT_ALL=1",
		"REQUEST_METHOD="+r.Method,
		"PATH_INFO="+r.URL.Path,
		"QUERY_STRING="+r.URL.RawQuery,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		fmt.Sprintf("CONTENT_LENGTH=%d", r.ContentLength),
	)
	if gp := r.Header.Get("Git-Protocol"); gp != "" {
		cmd.Env = append(cmd.Env, "GIT_PROTOCOL="+gp)
	}
	// CGI passes request headers as HTTP_* variables — http-backend needs
	// HTTP_CONTENT_ENCODING to inflate the gzipped request bodies git's
	// remote-curl sends for large fetch negotiations (real samples: dozens
	// of want lines; without it the backend parses gzip bytes as pkt-lines
	// and dies with "bad line length character").
	if ce := r.Header.Get("Content-Encoding"); ce != "" {
		cmd.Env = append(cmd.Env, "HTTP_CONTENT_ENCODING="+ce)
	}
	cmd.Stdin = r.Body
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if e, ok := err.(*exec.ExitError); ok {
			msg += ": " + strings.TrimSpace(string(e.Stderr))
		}
		http.Error(w, "git http-backend: "+msg, http.StatusBadGateway)
		return
	}
	// CGI: headers then a blank line, then the body.
	head, body, ok := bytes.Cut(out, []byte("\r\n\r\n"))
	if !ok {
		head, body, ok = bytes.Cut(out, []byte("\n\n"))
		if !ok {
			http.Error(w, "git http-backend: malformed CGI output", http.StatusBadGateway)
			return
		}
	}
	status := http.StatusOK
	for _, line := range bytes.Split(head, []byte("\n")) {
		k, v, found := bytes.Cut(line, []byte(":"))
		if !found {
			continue
		}
		key, val := string(k), string(bytes.TrimSpace(v))
		if strings.EqualFold(key, "Status") {
			fmt.Sscanf(val, "%d", &status)
			continue
		}
		w.Header().Set(key, val)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// serverIface abstracts the two targets for the scenario runner.
type serverIface interface {
	RepoURL() string
	take() (req, resp int64)
	Stop()
}

type banyanAdapter struct{ b *BanyanServer }

func (a banyanAdapter) RepoURL() string      { return a.b.URL }
func (a banyanAdapter) take() (int64, int64) { return a.b.take() }
func (a banyanAdapter) Stop()                { a.b.Stop() }

type gitAdapter struct{ g *GitCGIServer }

func (a gitAdapter) RepoURL() string      { return a.g.URL }
func (a gitAdapter) take() (int64, int64) { return a.g.take() }
func (a gitAdapter) Stop()                { a.g.Stop() }
