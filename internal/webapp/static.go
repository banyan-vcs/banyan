// static.go — the SPA: files from dist/ with an immutable cache on the
// hashed assets and an SPA fallback to index.html for every non-API GET
// (every path is a route). rel is normalized through path.Clean on an
// absolute path, which strips any ../ traversal before it reaches the
// filesystem join.
package webapp

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type staticServer struct {
	root string
}

func newStaticServer(dist string) *staticServer { return &staticServer{root: dist} }

var mimeTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".png":   "image/png",
	".svg":   "image/svg+xml",
	".map":   "application/json",
	".txt":   "text/plain; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
}

func (st *staticServer) serve(w http.ResponseWriter, rel string) {
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" {
		rel = "index.html"
	}
	full := filepath.Join(st.root, filepath.FromSlash(rel))
	data, err := os.ReadFile(full)
	if err != nil {
		// SPA fallback: every non-API GET is an application route.
		data, err = os.ReadFile(filepath.Join(st.root, "index.html"))
		if err != nil {
			http.Error(w, "dist/ missing — build the SPA first (npm run build in web/)", http.StatusInternalServerError)
			return
		}
		rel = "index.html"
	}
	ctype := mimeTypes[strings.ToLower(filepath.Ext(full))]
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	cache := "no-cache"
	if strings.HasPrefix(rel, "assets/") {
		cache = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", cache)
	_, _ = w.Write(data)
}
