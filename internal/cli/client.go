package cli

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// client is the thin workspace's transport (docs/05 §4): plain JSON RPC to
// the httpd frontend. Error envelopes (docs/10 §3.2) become *apiError so
// Run can map codes to exit codes (1 conflict / 2 server / 3 network).
type client struct {
	base string
	http *http.Client
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
	return &client{
		base: strings.TrimSuffix(base, "/"),
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *client) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, &netError{err}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &netError{err}
	}
	return resp, nil
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
	return decodeResp(resp, out)
}

// postRaw sends a JSON request and returns the raw response (callers inspect
// Content-Type and parse accordingly).
func (c *client) postRaw(path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	resp, err := c.do(http.MethodPost, path, body, "application/json")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, envelopeErr(resp)
	}
	return resp, nil
}

func (c *client) getJSON(path string, out any) error {
	resp, err := c.do(http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResp(resp, out)
}

func (c *client) getBytes(path string) ([]byte, error) {
	resp, err := c.do(http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, envelopeErr(resp)
	}
	return io.ReadAll(resp.Body)
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

type fetchTreesResponse struct {
	Branch  string            `json:"branch"`
	Head    string            `json:"head"`
	Tree    string            `json:"tree"`
	Full    bool              `json:"full"`
	Trees   map[string][]byte `json:"trees"`
	Blobs   map[string][]byte `json:"blobs"`
	Pending []string          `json:"pending"`
}

func (c *client) fetchTrees(repo, sub, rev, haveTree string, paths []string) (*fetchTreesResponse, error) {
	req := map[string]any{"repo": repo, "format": "bin"} // binary framing (docs/10 §3.3)
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
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-banyan-fetch-trees") {
		return parseFetchTreesBinary(resp.Body)
	}
	// Older server: JSON response (content negotiation by response type).
	var out fetchTreesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// parseFetchTreesBinary decodes the binary framing:
// magic line, JSON header line, then records [kind][raw oid][uvarint size][content].
func parseFetchTreesBinary(r io.Reader) (*fetchTreesResponse, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	magic, err := br.ReadString('\n')
	if err != nil || magic != "banyan-fetch-1\n" {
		return nil, fmt.Errorf("fetch-trees: bad binary magic %q", magic)
	}
	hdrLine, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var hdr struct {
		Branch  string   `json:"branch"`
		Head    string   `json:"head"`
		Tree    string   `json:"tree"`
		Full    bool     `json:"full"`
		Pending []string `json:"pending"`
		Algo    string   `json:"algo"`
	}
	if err := json.Unmarshal([]byte(hdrLine), &hdr); err != nil {
		return nil, fmt.Errorf("fetch-trees: binary header: %w", err)
	}
	oidWidth := 20
	if hdr.Algo == "sha256" {
		oidWidth = 32
	}
	out := &fetchTreesResponse{
		Branch: hdr.Branch, Head: hdr.Head, Tree: hdr.Tree, Full: hdr.Full,
		Pending: hdr.Pending, Trees: map[string][]byte{}, Blobs: map[string][]byte{},
	}
	oidBuf := make([]byte, oidWidth)
	for {
		kind, err := br.ReadByte()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(br, oidBuf); err != nil {
			return nil, err
		}
		size, err := binary.ReadUvarint(br)
		if err != nil {
			return nil, err
		}
		content := make([]byte, size)
		if _, err := io.ReadFull(br, content); err != nil {
			return nil, err
		}
		oid := hex.EncodeToString(oidBuf)
		switch kind {
		case 't':
			out.Trees[oid] = content
		case 'b':
			out.Blobs[oid] = content
		default:
			return nil, fmt.Errorf("fetch-trees: bad record kind %q", kind)
		}
	}
}

func (c *client) putObjects(repo string, objs []map[string]any) error {
	return c.post("/api/v1/objects/content", map[string]any{"repo": repo, "objects": objs}, nil)
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
