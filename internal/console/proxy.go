package console

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
)

// --- git traffic (docs/17 §1-§2) ---

// handleGit routes /{group}/{project}[/path][.git]/{info/refs,git-upload-pack,
// git-receive-pack}: resolve the logical location, authenticate the PAT,
// apply the coarse gate (existence + repo/anchor access), then stream the
// request to the gateway's physical URL under a signed identity. The gateway
// keeps the deep per-ref enforcement (protected, personal branches).
func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	target, endpoint, ok := parseGitURL(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	service := serviceOf(endpoint, r.URL.RawQuery)
	if service == "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	loc, err := s.DB.Resolve(ctx, target.group, target.project, target.sub)
	if err != nil {
		http.NotFound(w, r) // unknown or archived: plain 404 (existence hygiene)
		return
	}

	// Authenticate. Missing credentials on a private repo challenge; public
	// repos serve anonymous readers via the signed pseudo-identity.
	u := s.gitIdentity(r)
	if loc.Project.Visibility != Public && u.ID == 0 {
		w.Header().Set("WWW-Authenticate", `Basic realm="banyan"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	if service == "git-receive-pack" {
		ok, err := s.DB.CanPush(ctx, u, loc)
		if err != nil || !ok {
			http.Error(w, "permission denied", http.StatusForbidden)
			return
		}
	} else {
		ok, err := s.DB.CanFetch(ctx, u, loc)
		if err != nil || !ok {
			http.NotFound(w, r)
			return
		}
	}

	// Physical URL: /r/{repo_uid}.git[/tree/{sub}]/{endpoint}.
	phys := "/r/" + loc.Project.RepoUID + ".git"
	if loc.Sub != "" {
		phys += "/tree/" + loc.Sub
	}
	name := u.Username
	if u.ID == 0 {
		name = auth.AnonymousUser // public anonymous reader
	}
	s.proxyToGateway(w, r, phys+"/"+endpoint, r.URL.RawQuery, name)
}

// gitIdentity resolves the caller for git traffic. Invalid credentials are
// indistinguishable from none (the coarse gate still applies); the deep
// checks on the gateway only ever see the signed identity anyway.
func (s *Server) gitIdentity(r *http.Request) User {
	tok, basicUser, ok := tokenFromRequest(r)
	if !ok {
		return User{}
	}
	u, err := s.DB.LookupToken(r.Context(), HashToken(tok))
	if err != nil || (basicUser != "" && basicUser != u.Username) {
		return User{}
	}
	return u
}

type gitTarget struct {
	group, project, sub string
}

// parseGitURL splits a logical git URL. The service endpoint ends the URL;
// ".git" on the last repo segment is optional decoration. Segment 1 is the
// group, segment 2 the project, the remainder the subtree path.
func parseGitURL(path string) (t gitTarget, endpoint string, ok bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 3 {
		return t, "", false
	}
	end := ""
	switch {
	case segs[len(segs)-1] == "git-upload-pack" || segs[len(segs)-1] == "git-receive-pack":
		end = segs[len(segs)-1]
		segs = segs[:len(segs)-1]
	case len(segs) >= 2 && segs[len(segs)-2] == "info" && segs[len(segs)-1] == "refs":
		end = "info/refs"
		segs = segs[:len(segs)-2]
	default:
		return t, "", false
	}
	if len(segs) < 2 {
		return t, "", false
	}
	if last := segs[len(segs)-1]; strings.HasSuffix(last, ".git") {
		segs[len(segs)-1] = strings.TrimSuffix(last, ".git")
	}
	sub, err := CleanSub(strings.Join(segs[2:], "/"))
	if err != nil {
		return t, "", false
	}
	return gitTarget{group: segs[0], project: segs[1], sub: sub}, end, true
}

// serviceOf derives the requested git service from endpoint + query.
func serviceOf(endpoint, query string) string {
	if endpoint == "git-upload-pack" || endpoint == "git-receive-pack" {
		return endpoint
	}
	for _, kv := range strings.Split(query, "&") {
		if strings.TrimSpace(kv) == "service=git-upload-pack" {
			return "git-upload-pack"
		}
		if strings.TrimSpace(kv) == "service=git-receive-pack" {
			return "git-receive-pack"
		}
	}
	return ""
}

// --- the proxy core ---

// hop-by-hop headers never cross the proxy (RFC 7230 §6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// proxyToGateway streams one request to the gateway under a signed identity.
// Bodies flow through chunk-by-chunk (no whole-buffer anywhere — the T46
// discipline applies to the product plane as much as to the storage plane),
// and responses flush per chunk so sideband progress stays live.
func (s *Server) proxyToGateway(w http.ResponseWriter, r *http.Request, path, query, user string) {
	url := s.Gateway + path
	if query != "" {
		url += "?" + query
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		http.Error(w, "proxy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	// The console is the credential boundary: the client's Authorization
	// never crosses, the signed vouching does.
	out.Header.Del("Authorization")
	out.Header.Set(auth.IdentityHeaderName, auth.SignIdentity(s.Secret, user, time.Now()))
	out.ContentLength = r.ContentLength

	resp, err := proxyTransport().RoundTrip(out)
	if err != nil {
		http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body)
}

// streamCopy pumps body chunks with a flush after each — sideband progress
// frames must not queue up behind a buffering writer.
func streamCopy(w http.ResponseWriter, body io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 64<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

var sharedTransport *http.Transport

// proxyTransport: no redirect following (the gateway answers directly), sane
// timeouts that never bound an in-flight pack stream.
func proxyTransport() *http.Transport {
	if sharedTransport == nil {
		sharedTransport = &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        64,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}
	}
	return sharedTransport
}

// --- management-API passthrough (docs/17 §6) ---

// proxyAPI forwards every /api/v1 request the console does not own to the
// gateway, remapping the repo argument wherever it appears: as the path
// segment after /repos/, as the JSON body's "repo" field, or as the ?repo=
// query value. Logical {group}/{project} names become repo_uids; anything
// that does not resolve passes through unchanged (physical names for
// cluster operations keep working).
func (s *Server) proxyAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := r.URL.Path
	query := r.URL.Query()

	// Path: /api/v1/repos/{group}/{project}/... → /api/v1/repos/{repo_uid}/...
	if rest, ok := strings.CutPrefix(path, "/api/v1/repos/"); ok {
		segs := strings.Split(rest, "/")
		if len(segs) >= 2 {
			if uid, ok := s.mapRepo(ctx, segs[0]+"/"+segs[1]); ok {
				segs = append([]string{uid}, segs[2:]...)
			}
			path = "/api/v1/repos/" + strings.Join(segs, "/")
		}
	}

	// Query: repo=g/p
	if name := query.Get("repo"); name != "" {
		if uid, ok := s.mapRepo(ctx, name); ok {
			query.Set("repo", uid)
		}
	}

	var body io.Reader = r.Body
	clen := r.ContentLength
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil && len(raw) > 0 {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				if name, _ := m["repo"].(string); name != "" {
					if uid, ok := s.mapRepo(ctx, name); ok {
						m["repo"] = uid
					}
					if remade, err := json.Marshal(m); err == nil {
						raw = remade
					}
				}
			}
			body = bytes.NewReader(raw)
			clen = int64(len(raw))
		}
	}

	out, err := http.NewRequestWithContext(ctx, r.Method, s.Gateway+path+"?"+query.Encode(), body)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.Header.Del("Authorization")
	// Management traffic runs as the internal console identity: the caller
	// already passed the console's own gates for the console-owned surface.
	out.Header.Set(auth.IdentityHeaderName, auth.SignIdentity(s.Secret, "console", time.Now()))
	out.ContentLength = clen

	resp, err := proxyTransport().RoundTrip(out)
	if err != nil {
		apiErr(w, http.StatusBadGateway, "UPSTREAM", err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body)
}

// mapRepo resolves a logical "group/project" (or deeper path) to its
// repo_uid; misses pass through.
func (s *Server) mapRepo(ctx context.Context, name string) (string, bool) {
	segs := strings.Split(strings.Trim(name, "/"), "/")
	if len(segs) < 2 {
		return "", false
	}
	loc, err := s.DB.Resolve(ctx, segs[0], segs[1], "")
	if err != nil {
		return "", false
	}
	return loc.Project.RepoUID, true
}
