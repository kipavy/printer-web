package web

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// Minimal images (scratch, distroless) have no mime.types: register what the UI needs.
func init() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
}

func (s *Server) staticHandler() http.Handler {
	fsrv := http.FileServerFS(s.assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if p != "/" {
			if st, err := fs.Stat(s.assets, strings.TrimPrefix(p, "/")); err == nil && st.IsDir() {
				http.NotFound(w, r)
				return
			}
		}
		switch {
		case strings.HasPrefix(p, "/vendor/"):
			// pinned third-party files: safe to cache
			w.Header().Set("Cache-Control", "public, max-age=86400")
		case p == "/" || strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css"):
			w.Header().Set("Cache-Control", "no-cache")
		}
		fsrv.ServeHTTP(w, r)
	})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.assets, "qr.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
