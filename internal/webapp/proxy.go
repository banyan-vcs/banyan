// proxy.go — the authenticated reverse proxy to the gateway: same-origin
// from the SPA's point of view, the session's credentials injected
// server-side. Bodies and responses are buffered (the console's calls are
// JSON metadata plus bounded blobs); the path forwarded is the
// BASE_PATH-stripped one.
package webapp

import (
	"bytes"
	"io"
	"net/http"
)

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, pathname string) {
	sess, ok := s.sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req, err := http.NewRequest(r.Method, s.opts.Gateway+pathname+"?"+r.URL.RawQuery, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(body) > 0 {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	req.Header.Set("Authorization", sess.Basic())
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if acc := r.Header.Get("Accept"); acc != "" {
		req.Header.Set("Accept", acc)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "banyan unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "banyan read: " + err.Error()})
		return
	}
	for _, h := range []string{"Content-Type", "X-Banyan-Type"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}
