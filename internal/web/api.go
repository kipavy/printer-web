package web

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/kipavy/printer-web/internal/backend"
	"github.com/kipavy/printer-web/internal/docs"
	"github.com/kipavy/printer-web/internal/queue"
)

const apiStatusTimeout = 10 * time.Second

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	pub := s.cfg.PublicURL
	if pub == "" {
		pub = "http://" + r.Host
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": s.cfg.PrinterName, "location": s.cfg.PrinterLocation,
		"publicURL": pub, "lang": s.cfg.DefaultLang, "showNames": s.cfg.QueueShowNames,
	})
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *Server) handlePrinter(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), apiStatusTimeout)
	defer cancel()
	state, reasons := string(backend.StateUnreachable), []string{}
	type supply struct {
		Name  string `json:"name"`
		Color string `json:"color"`
		Level int    `json:"level"`
	}
	supplies := []supply{}
	if st, err := s.b.Status(ctx); err == nil {
		state, reasons = string(st.State), nonNil(st.Reasons)
		for _, sp := range st.Supplies {
			supplies = append(supplies, supply{sp.Name, sp.Color, sp.Level})
		}
	}
	caps, _ := s.b.Caps(ctx) // zero value on error
	writeJSON(w, http.StatusOK, map[string]any{
		"state": state, "reasons": reasons, "supplies": supplies,
		"caps": map[string]any{
			"color": caps.Color, "duplex": caps.Duplex, "quality": caps.Quality,
			"papers": nonNil(caps.Papers), "trays": nonNil(caps.Trays),
			"formats": nonNil(caps.Formats), "canTrack": caps.CanTrack, "canCancel": caps.CanCancel,
			"model": caps.Model, "ppm": caps.PPM, "ppmColor": caps.PPMColor, "dpi": caps.DPI,
		},
	})
}

// paperSize returns the first supported paper's size in points (A4 default).
func paperSize(papers []string) (float64, float64) {
	if len(papers) > 0 {
		switch strings.ToUpper(strings.TrimSpace(papers[0])) {
		case "LETTER":
			return 612, 792
		case "LEGAL":
			return 612, 1008
		case "A5":
			return 419.53, 595.28
		case "A3":
			return 841.89, 1190.55
		}
	}
	return 595.28, 841.89
}

func cleanName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, n)
	if r := []rune(n); len(r) > 120 {
		n = string(r[:120])
	}
	if n == "." || n == "/" {
		return ""
	}
	return n
}

// filePart advances to the multipart part named "file".
func filePart(mr *multipart.Reader) (*multipart.Part, error) {
	for {
		p, err := mr.NextPart()
		if err != nil {
			return nil, err
		}
		if p.FormName() == "file" {
			return p, nil
		}
		_ = p.Close()
	}
}

func isTooLarge(err error) bool {
	var mb *http.MaxBytesError
	return errors.As(err, &mb)
}

func writeBodyErr(w http.ResponseWriter, err error) {
	if isTooLarge(err) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	writeError(w, http.StatusBadRequest, "bad_request")
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	part, err := filePart(mr)
	if err != nil {
		writeBodyErr(w, err)
		return
	}
	name := cleanName(part.FileName())
	f, err := os.CreateTemp(s.dir, "up-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	file := f.Name() // the file to delete on failure: original, then the PDF
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(file)
		}
	}()
	_, err = io.Copy(f, part)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeBodyErr(w, err)
		return
	}
	kind, err := s.detect(file)
	switch {
	case errors.Is(err, docs.ErrEmpty):
		writeError(w, http.StatusUnprocessableEntity, "empty")
		return
	case errors.Is(err, docs.ErrUnsupported):
		writeError(w, http.StatusUnsupportedMediaType, "unsupported")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	// At most two conversions at once.
	wait := time.NewTimer(s.busyFor)
	defer wait.Stop()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-wait.C:
		writeError(w, http.StatusServiceUnavailable, "busy")
		return
	case <-r.Context().Done():
		return
	}

	step := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(r.Context(), toolTimeout)
	}
	pw, ph := 595.28, 841.89
	if caps, err := s.b.Caps(r.Context()); err == nil {
		pw, ph = paperSize(caps.Papers)
	}
	ctx, cancel := step()
	pdf, err := s.normalize(ctx, file, kind, pw, ph, s.dir)
	cancel()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "conversion_failed")
		return
	}
	if !samePath(pdf, file) {
		_ = os.Remove(file) // the original is no longer needed
		file = pdf
	}
	ctx, cancel = step()
	pages, err := s.pageCount(ctx, pdf)
	cancel()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "conversion_failed")
		return
	}
	if pages < 1 {
		writeError(w, http.StatusUnprocessableEntity, "empty")
		return
	}
	ctx, cancel = step()
	cov, err := s.inkCov(ctx, pdf)
	cancel()
	if err != nil {
		cov = nil // the cost estimate is optional
	}
	dr := s.drafts.Add(queue.Draft{
		Visitor: visitorOf(r), Name: name, Kind: kind, PDF: pdf, Pages: pages, Coverage: cov,
	})
	keep = true
	writeJSON(w, http.StatusOK, map[string]any{
		"id": dr.ID, "name": dr.Name, "kind": kindName(kind), "pages": pages,
	})
}

func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && aa == bb
}

func kindName(k docs.Kind) string {
	switch k {
	case docs.KindPDF:
		return "pdf"
	case docs.KindPS:
		return "ps"
	case docs.KindImage:
		return "image"
	case docs.KindHEIC:
		return "heic"
	case docs.KindText:
		return "text"
	}
	return "unknown"
}

// jobOptions are the options sent by the client.
type jobOptions struct {
	Copies      *int   `json:"copies"`
	Sides       string `json:"sides"`
	Color       bool   `json:"color"`
	Paper       string `json:"paper"`
	Orientation string `json:"orientation"`
	Quality     string `json:"quality"`
	Tray        string `json:"tray"`
}

func (o jobOptions) toBackend() (backend.Options, bool) {
	copies := 1
	if o.Copies != nil {
		copies = *o.Copies
	}
	if copies < 1 || copies > 99 {
		return backend.Options{}, false
	}
	sides := o.Sides
	switch sides {
	case "":
		sides = "one"
	case "one", "long", "short":
	default:
		return backend.Options{}, false
	}
	return backend.Options{
		Copies: copies, Sides: sides, Color: o.Color, Paper: o.Paper,
		Orientation: o.Orientation, Quality: o.Quality, Tray: o.Tray,
	}, true
}

func (s *Server) handleEstimate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Upload  string     `json:"upload"`
		Pages   string     `json:"pages"`
		Options jobOptions `json:"options"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	opts, ok := req.Options.toBackend()
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	dr, err := s.drafts.Get(req.Upload, visitorOf(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	sel, err := docs.ParsePages(req.Pages, dr.Pages)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_pages")
		return
	}
	sheets := queue.Sheets(len(sel), opts.Copies, opts.Sides)
	out := map[string]any{"sheets": sheets}
	var cov []docs.Coverage
	for _, p := range sel {
		if p-1 < len(dr.Coverage) {
			cov = append(cov, dr.Coverage[p-1])
		}
	}
	if cost, ok := docs.EstimateCost(cov, sheets, opts.Copies, !opts.Color, s.cfg.Pricing); ok {
		out["cost"] = cost
		out["currency"] = s.cfg.Pricing.Currency
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUploadPDF(w http.ResponseWriter, r *http.Request) {
	dr, err := s.drafts.Get(r.PathValue("id"), visitorOf(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	f, err := os.Open(dr.PDF)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Server) handleUploadDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.drafts.Delete(r.PathValue("id"), visitorOf(r)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type jobJSON struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Sheets   int        `json:"sheets"`
	Pages    int        `json:"pages"`
	State    string     `json:"state"`
	Error    string     `json:"error,omitempty"`
	Position int        `json:"position"`
	Mine     bool       `json:"mine"`
	Created  time.Time  `json:"created"`
	Finished *time.Time `json:"finished,omitempty"`
}

func toJobJSON(v queue.View) jobJSON {
	j := jobJSON{
		ID: v.ID, Name: v.Name, Sheets: v.Sheets, Pages: v.Pages, State: v.State,
		Error: v.Error, Position: v.Position, Mine: v.Mine, Created: v.Created,
	}
	if !v.Finished.IsZero() {
		t := v.Finished
		j.Finished = &t
	}
	return j
}

func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Upload  string     `json:"upload"`
		Key     string     `json:"key"`
		Pages   string     `json:"pages"`
		Options jobOptions `json:"options"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	opts, ok := req.Options.toBackend()
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	j, err := s.q.Submit(queue.Request{
		Upload: req.Upload, Key: req.Key, Visitor: visitorOf(r), Pages: req.Pages, Opts: opts,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toJobJSON(queue.View{Job: j, Mine: true}))
}

func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	visitor := visitorOf(r)
	q, h := []jobJSON{}, []jobJSON{}
	for _, v := range s.q.List(visitor, s.cfg.QueueShowNames) {
		q = append(q, toJobJSON(v))
	}
	for _, v := range s.q.History(visitor, s.cfg.QueueShowNames) {
		h = append(h, toJobJSON(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": q, "history": h})
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	id, visitor := r.PathValue("id"), visitorOf(r)
	views := append(s.q.List(visitor, s.cfg.QueueShowNames), s.q.History(visitor, s.cfg.QueueShowNames)...)
	for _, v := range views {
		if v.ID == id && v.Mine {
			writeJSON(w, http.StatusOK, toJobJSON(v))
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found")
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.q.Cancel(r.PathValue("id"), visitorOf(r)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
