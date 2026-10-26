// Server-side receive hooks (docs/04 §7): an HTTP-callback registry kept
// in the metadata plane — no gateway-local state, so every gateway in a
// multi-gateway deployment evaluates the same hooks. The API
// (/api/v1/repos/<repo>/hooks, docs/05) upserts registrations of shape
// {id, dir, events[], url, secret, timeout_ms}; at push time the serving
// gateway POSTs one JSON document per matching hook:
//
//	{
//	  "event": "pre-receive" | "update" | "post-receive",
//	  "repo": "monorepo", "directory": "lib/nested", "user": "dev",
//	  "repo_bytes": 123456,
//	  "updates": [{"old": "<40-zero>", "new": "<oid>", "ref": "refs/heads/main"}]
//	}
//
// 2xx allows the push; any other status (or transport failure/timeout)
// declines it — the response body's first line rides back to the client,
// like a git hook's stderr. update payloads carry the single ref under
// test; post-receive carries the applied set and its outcome is only
// logged. Matching: every hook whose dir is the pushed directory or an
// ancestor of it ("" = repo level) fires, most specific first — a subtree
// push to lib/nested consults hooks scoped at lib/nested, lib and the repo
// root; a root push consults repo-level hooks only. Zero OIDs render as
// the all-zero hex of the hash width.
package gitwire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// hookCallTimeout is the per-callback budget when a registration doesn't
// override it.
const hookCallTimeout = 10 * time.Second

// WebhookRunner evaluates registered hooks as HTTP callbacks. The zero
// value is ready (default client); Server.Hooks nil-tolerance lives in
// hooksClient.
type WebhookRunner struct {
	HTTP *http.Client
}

// hookUpdateInfo is one ref update in the callback payload.
type hookUpdateInfo struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

type hookPayload struct {
	Event     string           `json:"event"`
	Repo      string           `json:"repo"`
	Directory string           `json:"directory"`
	User      string           `json:"user"`
	RepoBytes int64            `json:"repo_bytes"`
	Updates   []hookUpdateInfo `json:"updates"`
}

// hookDeclined marks a callback's refusal (non-2xx, transport failure or
// timeout — all fail-closed for pre-receive/update: an unreachable policy
// endpoint must not become allow-by-default).
type hookDeclined struct {
	hook   string
	output string
}

func (d *hookDeclined) Error() string {
	msg := d.hook + " hook declined"
	if tail := firstLine(d.output); tail != "" {
		msg += ": " + tail
	}
	return msg
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// hookOID renders an OID in payload formats: zero OIDs (branch create /
// delete) print as the canonical all-zero hex of the hash width.
func hookOID(o model.OID, algo model.HashAlgo) string {
	if o.IsZero() {
		return strings.Repeat("0", 2*storage.RawOIDWidth(algo))
	}
	return string(o)
}

// matchHooks selects the hooks registered for (pushed directory, event),
// most specific directory first — dir "" matches every push; "lib"
// matches pushes to lib and lib/…; a root push matches only "".
func matchHooks(hooks []storage.HookInfo, dir, event string) []storage.HookInfo {
	dir = strings.Trim(dir, "/")
	var out []storage.HookInfo
	for _, h := range hooks {
		if h.Dir != "" && dir != h.Dir && !strings.HasPrefix(dir, h.Dir+"/") {
			continue // hook scoped to a different subtree
		}
		if !hasEvent(h, event) {
			continue
		}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return hookDepth(out[i].Dir) > hookDepth(out[j].Dir)
	})
	return out
}

func hasEvent(h storage.HookInfo, event string) bool {
	for _, e := range h.Events {
		if e == event {
			return true
		}
	}
	return false
}

// hookDepth is the directory depth of a hook's scope: 0 = repo level,
// 1 = "lib", 2 = "lib/nested".
func hookDepth(dir string) int {
	if dir == "" {
		return 0
	}
	return 1 + strings.Count(dir, "/")
}

// fireHooks evaluates every matching hook for one event, most specific
// first, short-circuiting on the first decline. bestEffort (post-receive)
// logs failures instead of failing the (already applied) push.
func (s *Server) fireHooks(ctx context.Context, vr VirtualRepo, user string, repoBytes int64,
	event string, updates []hookUpdateInfo, bestEffort bool) error {
	hooks, err := s.Meta.ListHooks(ctx, vr.Repo)
	if err != nil {
		if bestEffort {
			return nil
		}
		return fmt.Errorf("hook registry: %w", err)
	}
	dir := strings.Trim(strings.TrimPrefix(string(vr.Sub), "/"), "/")
	payload := hookPayload{
		Event:     event,
		Repo:      vr.Repo,
		Directory: dir,
		User:      user,
		RepoBytes: repoBytes,
		Updates:   updates,
	}
	for _, h := range matchHooks(hooks, dir, event) {
		cerr := s.hooksClient().call(ctx, h, payload)
		if cerr == nil {
			continue
		}
		if bestEffort {
			log.Printf("receive-pack %s%s: %s hook %s: %v", vr.Repo, vr.Sub, event, h.ID, cerr)
			continue
		}
		return cerr
	}
	return nil
}

// hooksClient adapts the optional Server.Hooks override to a usable runner.
func (s *Server) hooksClient() *WebhookRunner {
	if s.Hooks != nil {
		return s.Hooks
	}
	return defaultWebhookRunner
}

var defaultWebhookRunner = &WebhookRunner{}

// call delivers one callback and classifies the outcome.
func (r *WebhookRunner) call(ctx context.Context, h storage.HookInfo, payload hookPayload) error {
	client := http.DefaultClient
	if r != nil && r.HTTP != nil {
		client = r.HTTP
	}
	timeout := time.Duration(h.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = hookCallTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, rerr := http.NewRequestWithContext(cctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if rerr != nil {
		return rerr
	}
	req.Header.Set("Content-Type", "application/json")
	if h.Secret != "" {
		req.Header.Set("X-Banyan-Hook-Secret", h.Secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return &hookDeclined{hook: h.ID, output: fmt.Sprintf("callback timed out after %s", timeout)}
		}
		return &hookDeclined{hook: h.ID, output: err.Error()}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &hookDeclined{hook: h.ID, output: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(out))}
	}
	return nil
}
