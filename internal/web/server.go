// Package web is the HTTP server and JSON API of printer-web.
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kipavy/printer-web/internal/backend"
	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/docs"
	"github.com/kipavy/printer-web/internal/queue"
)

const (
	cookieName   = "pw_visitor"
	csrfHeader   = "X-Requested-With"
	csrfValue    = "printer-web"
	toolTimeout  = 2 * time.Minute
	maxJSONBytes = 64 << 10
)

// Server serves the API and the embedded front end.
type Server struct {
	cfg     config.Config
	b       backend.Backend
	drafts  *queue.Drafts
	q       *queue.Queue
	dir     string // drafts directory
	assets  fs.FS
	sem     chan struct{} // limits concurrent conversions
	busyFor time.Duration // how long an upload waits for a free slot

	// Injectable document tools.
	detect    func(path string) (docs.Kind, error)
	normalize func(ctx context.Context, in string, k docs.Kind, w, h float64, tmp string) (string, error)
	pageCount func(ctx context.Context, pdf string) (int, error)
	inkCov    func(ctx context.Context, pdf string) ([]docs.Coverage, error)
}

// New builds a Server. dir is the drafts directory, assets the front end.
func New(cfg config.Config, b backend.Backend, d *queue.Drafts, q *queue.Queue, dir string, assets fs.FS) *Server {
	mime.AddExtensionType(".woff2", "font/woff2")
	mime.AddExtensionType(".woff", "font/woff")
	mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")
	return &Server{
		cfg: cfg, b: b, drafts: d, q: q, dir: dir, assets: assets,
		sem: make(chan struct{}, 2), busyFor: 30 * time.Second,
		detect: docs.Detect, normalize: docs.Normalize,
		pageCount: docs.PageCount, inkCov: docs.InkCoverage,
	}
}

type ctxKey struct{}

// Handler returns the full HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/printer", s.handlePrinter)
	mux.HandleFunc("POST /api/uploads", s.handleUpload)
	mux.HandleFunc("GET /api/uploads/{id}/pdf", s.handleUploadPDF)
	mux.HandleFunc("DELETE /api/uploads/{id}", s.handleUploadDelete)
	mux.HandleFunc("POST /api/estimate", s.handleEstimate)
	mux.HandleFunc("POST /api/jobs", s.handleJobCreate)
	mux.HandleFunc("GET /api/jobs", s.handleJobList)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJobGet)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleJobDelete)
	mux.HandleFunc("GET /qr", s.handleQR)
	mux.Handle("GET /", s.staticHandler())
	return s.middleware(mux)
}

func validVisitor(v string) bool {
	if len(v) != 32 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil && v == strings.ToLower(v)
}

func newVisitor() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// middleware sets the visitor cookie and enforces the CSRF rules.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, "misdirected")
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", csp)
		visitor := ""
		if c, err := r.Cookie(cookieName); err == nil && validVisitor(c.Value) {
			visitor = c.Value
		}
		if visitor == "" {
			visitor = newVisitor()
			http.SetCookie(w, &http.Cookie{
				Name: cookieName, Value: visitor, Path: "/", HttpOnly: true,
				SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil,
				MaxAge: 365 * 24 * 3600,
			})
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		case http.MethodPost:
			if r.URL.Path == "/api/uploads" {
				if r.Header.Get(csrfHeader) != csrfValue {
					writeError(w, http.StatusForbidden, "forbidden")
					return
				}
			} else if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		default:
			if r.Header.Get(csrfHeader) != csrfValue {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, visitor)))
	})
}

func visitorOf(r *http.Request) string {
	v, _ := r.Context().Value(ctxKey{}).(string)
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, key string) {
	writeJSON(w, status, map[string]string{"error": key})
}

// writeErr maps a queue error to an HTTP response.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, queue.ErrNotFound), errors.Is(err, queue.ErrForbidden):
		writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, queue.ErrExpired):
		writeError(w, http.StatusGone, "expired")
	case errors.Is(err, queue.ErrBadPages):
		writeError(w, http.StatusBadRequest, "bad_pages")
	case errors.Is(err, queue.ErrBadKey):
		writeError(w, http.StatusBadRequest, "bad_key")
	case errors.Is(err, queue.ErrQueueFull):
		writeError(w, http.StatusServiceUnavailable, "queue_full")
	case errors.Is(err, queue.ErrNotActive):
		writeError(w, http.StatusConflict, "not_active")
	case errors.Is(err, queue.ErrPrinter):
		writeError(w, http.StatusBadGateway, "printer_error")
	default:
		writeError(w, http.StatusInternalServerError, "internal")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(v)
	if err == nil {
		if _, e2 := dec.Token(); e2 != io.EOF {
			err = errors.New("trailing data")
			if isTooLarge(e2) {
				err = e2
			}
		}
	}
	if err != nil {
		writeBodyErr(w, err)
		return false
	}
	return true
}

const csp = "default-src 'self'; script-src 'self'; worker-src 'self' blob:; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

// hostAllowed is a DNS-rebinding guard: only IPs, local names and the
// PUBLIC_URL host may be used to reach the server.
func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || host == "localhost" {
		return true
	}
	for _, suf := range []string{".local", ".lan", ".home.arpa", ".internal"} {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Hostname() != "" {
		return host == strings.ToLower(u.Hostname())
	}
	return false
}
