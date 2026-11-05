package web

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

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
		if p == "/" || p == "/index.html" {
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
