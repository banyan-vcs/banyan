package rpc

import (
	"net/http"
	"path"
	"strings"

	"github.com/banyan-vcs/banyan/internal/service"
)

// GET /api/v1/repos/<repo>/raw/<path>?rev=<branch>&sub= — a file's bytes
// with a DISPLAY content-type chosen from the path (the README's images;
// docs/gateway/service-api.md). The bytes are the same /api/v1/objects
// serves; what this face adds is the content-type an <img> needs and the
// ref:path addressing the console links by. Read posture matches objects
// and tree: any authenticated member, the object graph decides what
// exists. nosniff rides every answer; SVG additionally carries a CSP that
// neuters scripts — a hostile SVG navigated to directly must not run in
// the web origin (as an <img> it never runs scripts either way).
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: "GET only"}})
		return
	}
	q := r.URL.Query()
	file := strings.Join(parts[2:], "/")
	res, err := s.Svc.Resolve(r.Context(), service.ResolveRequest{
		Repo: parts[0], Sub: q.Get("sub"), Rev: q.Get("rev"), Path: file,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	if res.Type != "blob" {
		writeJSON(w, http.StatusBadRequest, errEnvelope{Error: errBody{Code: "INVALID_ARG", Message: file + " is not a file"}})
		return
	}
	_, data, err := s.Svc.GetObject(r.Context(), parts[0], res.OID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	ct, svg := rawContentType(file)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if svg {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// rawContentType answers the display content-type for a repo path.
// Unknown extensions download (octet-stream + nosniff) instead of
// rendering — text and friends keep riding the code browser, where the
// console already types them properly.
func rawContentType(name string) (ct string, svg bool) {
	switch strings.ToLower(path.Ext(name)) {
	case ".png":
		return "image/png", false
	case ".jpg", ".jpeg":
		return "image/jpeg", false
	case ".gif":
		return "image/gif", false
	case ".webp":
		return "image/webp", false
	case ".bmp":
		return "image/bmp", false
	case ".avif":
		return "image/avif", false
	case ".ico":
		return "image/x-icon", false
	case ".svg":
		return "image/svg+xml", true
	default:
		return "application/octet-stream", false
	}
}
