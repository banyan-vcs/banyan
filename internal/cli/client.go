package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/cliproto"
)

// client is the thin workspace's transport (docs/client/cli.md §4): plain JSON RPC to
// the httpd frontend. Error envelopes (docs/gateway/service-api.md §3.2) become *apiError so
// Run can map codes to exit codes (1 conflict / 2 server / 3 network).
//
// Two HTTP clients share one transport: `http` keeps a 60s wall clock for
// small JSON calls, `raw` has NO overall timeout for streams (fetch-trees
// frames, blob GETs, batch frames) — a big clone over a slow link must
// not die mid-body at an arbitrary second mark; dial/TLS/header timeouts
// still bound a dead peer (docs/operations/performance.md §9 D).
type client struct {
	base string
	http *http.Client
	raw  *http.Client
	// authHeader rides every request when a session credential (or a URL
	// userinfo override) exists for this server — docs/client/cli.md §4.5.
	authHeader string
	// refreshSSH: the server's SSH address when the credential came from
	// the banyan-auth exec — a 401 re-mints silently.
	refreshSSH string
	// caps/capsRoot: the cached server capabilities (multi-version
	// contract, docs/client/cli.md §4.4) — nil caps = unknown server, the
	// in-protocol fallbacks carry those.
	caps     *serverCaps
	capsRoot string
}

type apiError struct {
	Code    string
	Message string
	Status  int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// netError marks transport-level failures (connection refused, timeout) —
// exit code 3, distinct from server-side errors.
type netError struct{ err error }

func (e *netError) Error() string { return e.err.Error() }
func (e *netError) Unwrap() error { return e.err }

func isConflictErr(err error) bool {
	api, ok := err.(*apiError)
	return ok && strings.HasPrefix(api.Code, "CONFLICT_")
}

func newClient(base string) *client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8, // the batch/parallel lanes keep several streams hot
	}
	c := &client{
		base: strings.TrimSuffix(base, "/"),
		http: &http.Client{Timeout: 60 * time.Second, Transport: tr},
		raw:  &http.Client{Transport: tr},
	}
	// Session credential for this server (banyan login; docs/client/cli.md
	// §4.5) — short-lived, attached to every request. Expired ones still
	// ride: the 401 path re-mints over SSH when that's how they arrived.
	if cred := credForHost(c.base); cred != nil && cred.Token != "" {
		c.authHeader = basicHeaderValue(cred.User, cred.Token)
		if cred.Method == "ssh" {
			c.refreshSSH = cred.SSHAddr
		}
	}
	return c
}

// setBasic overrides the credential from a URL's userinfo
// (http://alice:demo@host/... now authenticates instead of being dropped).
func (c *client) setBasic(user, pass string) {
	if user == "" {
		return
	}
	c.authHeader = basicHeaderValue(user, pass)
	c.refreshSSH = ""
}

// reauth mints a fresh session over SSH after a 401; false when this
// credential can't refresh (web path: the user re-runs banyan login).
func (c *client) reauth() bool {
	if c.refreshSSH == "" {
		return false
	}
	cred := credForHost(c.base)
	if cred == nil || cred.Token == "" {
		return false
	}
	minted, err := sshMint(c.refreshSSH, cred.User)
	if err != nil {
		return false
	}
	putCred(c.base, credential{User: minted.User, Token: minted.Token, Expires: minted.Expires, Method: "ssh", SSHAddr: c.refreshSSH})
	c.authHeader = basicHeaderValue(minted.User, minted.Token)
	return true
}

// authorize stamps the session header when present.
func (c *client) authorize(req *http.Request) {
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
}

func (c *client) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	// Snapshot the body: the 401 reauth retry must re-send it whole
	// (callers pass in-memory readers — uploads are pre-chunked).
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = io.ReadAll(body)
	}
	send := func() (*http.Response, error) {
		var r io.Reader
		if bodyBytes != nil {
			r = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequest(method, c.base+path, r)
		if err != nil {
			return nil, &netError{err}
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		c.authorize(req)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, &netError{err}
		}
		return resp, nil
	}
	resp, err := send()
	if err == nil && resp.StatusCode == http.StatusUnauthorized && c.reauth() {
		resp.Body.Close()
		return send() // fresh session, one silent retry
	}
	return resp, err
}

// hint401 decorates a final 401 with the re-login line when the stored
// credential cannot refresh (the web path — SSH sessions re-mint silently,
// so they never surface here). The raw envelope gives the user no direction.
func (c *client) hint401(err error) error {
	if api, ok := err.(*apiError); ok && api.Status == http.StatusUnauthorized && c.refreshSSH == "" && c.authHeader != "" {
		api.Message += " — re-login: banyan login " + c.base
	}
	return err
}

// post sends a JSON request; a 2xx response is decoded into out (when non-nil).
func (c *client) post(path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	resp, err := c.do(http.MethodPost, path, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.hint401(decodeResp(resp, out))
}

// postRaw sends a JSON request and returns the raw STREAM response on the
// no-wall-clock client (callers inspect Content-Type and parse frames).
func (c *client) postRaw(path string, in any) (*http.Response, error) {
	return c.postRawOn(c.raw, path, in)
}

func (c *client) postRawOn(hc *http.Client, path string, in any) (*http.Response, error) {
	var bodyBytes []byte
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		bodyBytes = raw
	}
	send := func() (*http.Response, error) {
		var body io.Reader
		if bodyBytes != nil {
			body = bytes.NewReader(bodyBytes) // fresh reader per attempt: the 401 retry re-sends
		}
		req, err := http.NewRequest(http.MethodPost, c.base+path, body)
		if err != nil {
			return nil, &netError{err}
		}
		req.Header.Set("Content-Type", "application/json")
		c.authorize(req)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, &netError{err}
		}
		if resp.StatusCode != http.StatusOK {
			apiErr := envelopeErr(resp) // reads the body BEFORE the close
			resp.Body.Close()
			return nil, apiErr
		}
		return resp, nil
	}
	resp, err := send()
	if api, ok := err.(*apiError); ok && api.Status == http.StatusUnauthorized && c.reauth() {
		return send() // fresh session, one silent retry
	}
	return resp, c.hint401(err)
}

func (c *client) getJSON(path string, out any) error {
	resp, err := c.do(http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.hint401(decodeResp(resp, out))
}

func (c *client) getBytes(path string) ([]byte, error) {
	resp, err := c.rawGet(path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, envelopeErr(resp)
	}
	return io.ReadAll(resp.Body)
}

// rawGet issues a GET on the no-wall-clock client — object bytes may be
// arbitrarily large on arbitrarily slow links.
func (c *client) rawGet(path string) (*http.Response, error) {
	send := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
		if err != nil {
			return nil, &netError{err}
		}
		c.authorize(req)
		resp, err := c.raw.Do(req)
		if err != nil {
			return nil, &netError{err}
		}
		return resp, nil
	}
	resp, err := send()
	if err == nil && resp.StatusCode == http.StatusUnauthorized && c.reauth() {
		resp.Body.Close()
		return send()
	}
	return resp, err
}

// getObjectsBatch fetches a chunk of objects over ONE connection as the
// CLI protocol's zlib frame (docs/operations/performance.md §9 D). A
// server without the endpoint answers a JSON error envelope — the caller
// falls back to per-oid GETs, so old servers keep working.
func (c *client) getObjectsBatch(repo string, oids []string) (map[string][]byte, error) {
	resp, err := c.postRaw("/api/v1/objects/batch", map[string]any{"repo": repo, "oids": oids})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), cliproto.ContentTypeObjects) {
		return nil, fmt.Errorf("objects/batch: unexpected content-type %q", resp.Header.Get("Content-Type"))
	}
	_, records, err := cliproto.ParseObjectsFrame(resp.Body)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(records))
	for _, o := range records {
		out[o.OID] = o.Data
	}
	return out, nil
}

func decodeResp(resp *http.Response, out any) error {
	if resp.StatusCode != http.StatusOK {
		return envelopeErr(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func envelopeErr(resp *http.Response) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err == nil && env.Error.Code != "" {
		return &apiError{Code: env.Error.Code, Message: env.Error.Message, Status: resp.StatusCode}
	}
	return &apiError{Code: "HTTP_" + resp.Status, Message: "unexpected response", Status: resp.StatusCode}
}

// --- typed API wrappers ---

// fetchTreesResponse is the shared CLI-protocol shape (internal/cliproto
// owns the framing both ends speak — v2 zlib records + deltas, v1 and
// JSON as the downgrade ladder).
type fetchTreesResponse = cliproto.FetchResult

// fetchFormat picks the framing hint: v2 unless the capability cache
// knows this server predates it (an old server would otherwise downgrade
// the whole clone to JSON framing — correct but 1.33x the bytes).
func (c *client) fetchFormat() string {
	if !c.caps.has("fetch-trees-bin2") {
		return "bin"
	}
	return "binz"
}

func (c *client) fetchTrees(repo, sub, rev, haveTree string, paths []string) (*fetchTreesResponse, error) {
	req := map[string]any{"repo": repo, "format": c.fetchFormat()}
	if sub != "" {
		req["sub"] = sub
	}
	if rev != "" {
		req["rev"] = rev
	}
	if haveTree != "" {
		req["haveTree"] = haveTree
	}
	if len(paths) > 0 {
		req["paths"] = paths
	}
	resp, err := c.postRaw("/api/v1/fetch-trees", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch ct := resp.Header.Get("Content-Type"); {
	case strings.HasPrefix(ct, cliproto.ContentTypeV2), strings.HasPrefix(ct, cliproto.ContentTypeV1):
		return cliproto.Parse(resp.Body) // magic sniff picks v1/v2 — older servers degrade silently
	}
	// Oldest server: JSON response (content negotiation by response type).
	var out fetchTreesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// putObjects uploads a commit's blobs in ≤64MiB chunks (docs/operations/
// performance.md §9 E): a huge commit neither builds one giant request
// body nor leans on any single POST succeeding. Each chunk rides the
// binary frames with per-record zlib (§9 A2); an older server that
// answers 400 on the unknown Content-Type gets the JSON body once — the
// multi-version ladder, cheap because it only ever fires on old servers.
var putChunkBytes = 64 << 20 // var: the chunking test shrinks it

func (c *client) putObjects(repo string, objs []cliproto.ObjectRecord) error {
	var chunk []cliproto.ObjectRecord
	var carried int
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		err := c.putObjectsOnce(repo, chunk)
		chunk = chunk[:0]
		carried = 0
		return err
	}
	for _, o := range objs {
		if len(chunk) > 0 && carried+len(o.Data) > putChunkBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		chunk = append(chunk, o)
		carried += len(o.Data)
	}
	return flush()
}

func (c *client) putObjectsOnce(repo string, objs []cliproto.ObjectRecord) error {
	if !c.caps.has("objects-frame") {
		// The cache knows: JSON only (no trial POST to burn).
		return c.putObjectsJSON(repo, objs)
	}
	var buf bytes.Buffer
	if err := cliproto.WriteObjectsFrame(&buf, repo, "sha1", objs); err != nil {
		return err
	}
	resp, err := c.do(http.MethodPost, "/api/v1/objects/content", &buf, cliproto.ContentTypeObjects)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		return nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		resp.Body.Close() // an older server: release the connection before the JSON retry
		c.capsStale()     // the cache was wrong or absent — next command rediscovers
		return c.putObjectsJSON(repo, objs)
	}
	defer resp.Body.Close()
	return envelopeErr(resp)
}

func (c *client) putObjectsJSON(repo string, objs []cliproto.ObjectRecord) error {
	var jsonObjs []map[string]any
	for _, o := range objs {
		jsonObjs = append(jsonObjs, map[string]any{"oid": o.OID, "type": o.Type, "data": o.Data})
	}
	return c.post("/api/v1/objects/content", map[string]any{"repo": repo, "objects": jsonObjs}, nil)
}

type commitResponse struct {
	Head     string `json:"head"`
	RootView string `json:"rootView"`
	Tree     string `json:"tree"`
}

func (c *client) commit(repo, sub, branch, base, message, name, email string, changes []map[string]any) (*commitResponse, error) {
	req := map[string]any{
		"repo": repo, "base": base, "message": message, "changes": changes,
		"author": map[string]string{"name": name, "email": email},
	}
	if sub != "" {
		req["sub"] = sub
	}
	if branch != "" {
		req["branch"] = branch
	}
	var out commitResponse
	if err := c.post("/api/v1/commit", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type syncResponse struct {
	Status   string `json:"status"` // uptodate | rebased
	Rebased  int    `json:"rebased"`
	Head     string `json:"head"`
	RootView string `json:"rootView"`
}

func (c *client) sync(repo, sub, branch string) (*syncResponse, error) {
	req := map[string]any{"repo": repo, "sub": sub}
	if branch != "" {
		req["branch"] = branch
	}
	var out syncResponse
	if err := c.post("/api/v1/sync", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type refEntry struct {
	Name string `json:"name"`
	Head string `json:"head"`
}

func (c *client) listRefs(repo, sub string) ([]refEntry, error) {
	q := url.Values{"repo": {repo}}
	if sub != "" {
		q.Set("sub", sub)
	}
	var out struct {
		Refs []refEntry `json:"refs"`
	}
	if err := c.getJSON("/api/v1/refs?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out.Refs, nil
}

type logRespEntry struct {
	OID     string `json:"oid"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	When    int64  `json:"when"`
}

func (c *client) log(repo, path string, limit int) ([]logRespEntry, error) {
	q := url.Values{"repo": {repo}, "limit": {fmt.Sprint(limit)}}
	if path != "" {
		q.Set("path", path)
	}
	var out struct {
		Entries []logRespEntry `json:"entries"`
	}
	if err := c.getJSON("/api/v1/log?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

func (c *client) getObject(repo, oid string) ([]byte, error) {
	return c.getBytes("/api/v1/objects?repo=" + url.QueryEscape(repo) + "&oid=" + url.QueryEscape(oid))
}
