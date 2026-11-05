package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kipavy/printer-web/internal/backend"
	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/docs"
	"github.com/kipavy/printer-web/internal/queue"
	embedded "github.com/kipavy/printer-web/web"
)

type fakeBackend struct {
	st    backend.Status
	stErr error
	caps  backend.Caps
}

func (f *fakeBackend) Caps(context.Context) (backend.Caps, error)     { return f.caps, nil }
func (f *fakeBackend) Status(context.Context) (backend.Status, error) { return f.st, f.stErr }
func (f *fakeBackend) Submit(context.Context, string, string, backend.Options) (backend.Handle, error) {
	return backend.Handle{}, nil
}
func (f *fakeBackend) Track(context.Context, backend.Handle) (backend.JobState, error) {
	return backend.JobSent, nil
}
func (f *fakeBackend) Cancel(context.Context, backend.Handle) error { return backend.ErrNotSupported }

type env struct {
	s   *Server
	ts  *httptest.Server
	dir string
	fb  *fakeBackend
}

func newEnv(t *testing.T, mod func(*config.Config)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		PrinterName: "Test", MaxUploadBytes: 1 << 20, DraftTTL: time.Minute, DefaultLang: "auto",
		Pricing: config.Pricing{Currency: "EUR", Sheet: 0.5},
	}
	if mod != nil {
		mod(&cfg)
	}
	fb := &fakeBackend{st: backend.Status{State: backend.StateReady}, caps: backend.Caps{Papers: []string{"A4"}}}
	d := queue.NewDrafts(dir, cfg.DraftTTL, nil)
	q := queue.NewQueue(fb, d, dir, nil)
	assets := fstest.MapFS{
		"index.html": {Data: []byte("<html>hi</html>")},
		"qr.html":    {Data: []byte("<html>qr</html>")},
		"app.js":     {Data: []byte("var a=1;")},
		"style.css":  {Data: []byte("a{}")},
	}
	s := New(cfg, fb, d, q, dir, assets)
	s.detect = func(p string) (docs.Kind, error) {
		b, _ := os.ReadFile(p)
		switch {
		case len(b) == 0:
			return 0, docs.ErrEmpty
		case bytes.HasPrefix(b, []byte("%PDF")):
			return docs.KindPDF, nil
		case bytes.HasPrefix(b, []byte("TEXT")):
			return docs.KindText, nil
		}
		return 0, docs.ErrUnsupported
	}
	s.normalize = func(_ context.Context, in string, k docs.Kind, _, _ float64, tmp string) (string, error) {
		if k == docs.KindPDF {
			return in, nil
		}
		out := filepath.Join(tmp, "norm.pdf")
		return out, os.WriteFile(out, []byte("%PDF-converted"), 0o600)
	}
	s.pageCount = func(context.Context, string) (int, error) { return 3, nil }
	s.inkCov = func(context.Context, string) ([]docs.Coverage, error) {
		return []docs.Coverage{{K: 0.05}, {K: 0.05}, {K: 0.05}}, nil
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &env{s: s, ts: ts, dir: dir, fb: fb}
}

type client struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
}

func (e *env) client(t *testing.T) *client { return &client{t: t, base: e.ts.URL} }

func (c *client) do(req *http.Request) (*http.Response, []byte) {
	c.t.Helper()
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, ck := range resp.Cookies() {
		if ck.Name == cookieName {
			c.cookie = ck
		}
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (c *client) get(path string) (*http.Response, []byte) {
	req, _ := http.NewRequest("GET", c.base+path, nil)
	return c.do(req)
}

func (c *client) postJSON(path string, v any) (*http.Response, []byte) {
	b, _ := json.Marshal(v)
	req, _ := http.NewRequest("POST", c.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *client) delete(path string) (*http.Response, []byte) {
	req, _ := http.NewRequest("DELETE", c.base+path, nil)
	req.Header.Set("X-Requested-With", "printer-web")
	return c.do(req)
}

func (c *client) upload(name, content string) (*http.Response, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write([]byte(content))
	_ = mw.Close()
	req, _ := http.NewRequest("POST", c.base+"/api/uploads", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "printer-web")
	return c.do(req)
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json %q: %v", b, err)
	}
	return m
}

func leftovers(t *testing.T, dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

func TestUploadRejects(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.MaxUploadBytes = 1000 })
	c := e.client(t)
	cases := []struct {
		name, content string
		status        int
		key           string
	}{
		{"a.bin", "garbage", 415, "unsupported"},
		{"a.pdf", "", 422, "empty"},
		{"big.pdf", "%PDF" + strings.Repeat("x", 5000), 413, "too_large"},
	}
	for _, tc := range cases {
		resp, body := c.upload(tc.name, tc.content)
		if resp.StatusCode != tc.status || decode(t, body)["error"] != tc.key {
			t.Errorf("%s: got %d %s, want %d %s", tc.name, resp.StatusCode, body, tc.status, tc.key)
		}
	}
	// conversion failure and zero pages
	e.s.normalize = func(context.Context, string, docs.Kind, float64, float64, string) (string, error) {
		return "", errors.New("boom")
	}
	resp, body := c.upload("t.txt", "TEXT hello")
	if resp.StatusCode != 422 || decode(t, body)["error"] != "conversion_failed" {
		t.Errorf("conversion: %d %s", resp.StatusCode, body)
	}
	e.s.normalize = func(_ context.Context, in string, _ docs.Kind, _, _ float64, _ string) (string, error) {
		return in, nil
	}
	e.s.pageCount = func(context.Context, string) (int, error) { return 0, nil }
	resp, body = c.upload("a.pdf", "%PDF-1")
	if resp.StatusCode != 422 || decode(t, body)["error"] != "empty" {
		t.Errorf("zero pages: %d %s", resp.StatusCode, body)
	}
	if n := leftovers(t, e.dir); n != 0 {
		t.Errorf("%d files left in the drafts dir", n)
	}
}

func TestUploadBusy(t *testing.T) {
	e := newEnv(t, nil)
	e.s.busyFor = 20 * time.Millisecond
	e.s.sem <- struct{}{}
	e.s.sem <- struct{}{}
	resp, body := e.client(t).upload("a.pdf", "%PDF-1")
	if resp.StatusCode != 503 || decode(t, body)["error"] != "busy" {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
	if n := leftovers(t, e.dir); n != 0 {
		t.Errorf("%d files left", n)
	}
}

func TestUploadAndPrintFlow(t *testing.T) {
	e := newEnv(t, nil)
	c := e.client(t)
	resp, body := c.upload("../../doc.txt", "TEXT hello")
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	up := decode(t, body)
	id := up["id"].(string)
	if up["pages"] != float64(3) || up["name"] != "doc.txt" || up["kind"] != "text" {
		t.Errorf("upload = %v", up)
	}
	if n := leftovers(t, e.dir); n != 1 { // original removed, only the PDF stays
		t.Errorf("%d files in drafts dir, want 1", n)
	}

	resp, body = c.get("/api/uploads/" + id + "/pdf")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(string(body), "%PDF") {
		t.Errorf("pdf: %d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}

	opts := map[string]any{"copies": 2, "sides": "long", "color": false}
	resp, body = c.postJSON("/api/estimate", map[string]any{"upload": id, "pages": "1-3", "options": opts})
	est := decode(t, body)
	// 3 pages duplex = 2 sheets * 2 copies = 4 sheets * 0.5
	if resp.StatusCode != 200 || est["sheets"] != float64(4) || est["cost"] != float64(2) || est["currency"] != "EUR" {
		t.Errorf("estimate: %d %s", resp.StatusCode, body)
	}
	resp, body = c.postJSON("/api/estimate", map[string]any{"upload": id, "pages": "9", "options": opts})
	if resp.StatusCode != 400 || decode(t, body)["error"] != "bad_pages" {
		t.Errorf("estimate bad pages: %d %s", resp.StatusCode, body)
	}
	resp, body = c.postJSON("/api/estimate", map[string]any{"upload": id, "options": map[string]any{"copies": 100}})
	if resp.StatusCode != 400 || decode(t, body)["error"] != "bad_request" {
		t.Errorf("copies 100: %d %s", resp.StatusCode, body)
	}

	job := map[string]any{"upload": id, "key": "k1", "pages": "1-2", "options": opts}
	resp, body = c.postJSON("/api/jobs", job)
	j := decode(t, body)
	if resp.StatusCode != 201 || j["state"] != "queued" || j["position"] != float64(1) || j["mine"] != true {
		t.Fatalf("job: %d %s", resp.StatusCode, body)
	}
	// same key: same job
	resp, body = c.postJSON("/api/jobs", job)
	if resp.StatusCode != 201 || decode(t, body)["id"] != j["id"] {
		t.Errorf("idempotent: %d %s", resp.StatusCode, body)
	}
	// upload is consumed
	resp, body = c.postJSON("/api/jobs", map[string]any{"upload": id, "key": "k2", "options": opts})
	if resp.StatusCode != 404 || decode(t, body)["error"] != "not_found" {
		t.Errorf("reuse: %d %s", resp.StatusCode, body)
	}
	resp, body = c.postJSON("/api/jobs", map[string]any{"upload": id, "options": opts})
	if resp.StatusCode != 400 || decode(t, body)["error"] != "bad_key" {
		t.Errorf("no key: %d %s", resp.StatusCode, body)
	}

	resp, body = c.get("/api/jobs")
	list := decode(t, body)
	if resp.StatusCode != 200 || len(list["queue"].([]any)) != 1 || len(list["history"].([]any)) != 0 {
		t.Errorf("list: %s", body)
	}
	resp, _ = c.get("/api/jobs/" + j["id"].(string))
	if resp.StatusCode != 200 {
		t.Errorf("get job: %d", resp.StatusCode)
	}
	resp, _ = c.delete("/api/jobs/" + j["id"].(string))
	if resp.StatusCode != 204 {
		t.Errorf("cancel: %d", resp.StatusCode)
	}
	resp, body = c.delete("/api/jobs/" + j["id"].(string))
	if resp.StatusCode != 404 {
		t.Errorf("cancel twice: %d %s", resp.StatusCode, body)
	}
	if n := leftovers(t, e.dir); n != 0 {
		t.Errorf("%d files left after cancel", n)
	}
}

func TestEstimateOmitsCostWhenPricingOff(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Pricing = config.Pricing{} })
	c := e.client(t)
	_, body := c.upload("a.pdf", "%PDF-1")
	id := decode(t, body)["id"].(string)
	_, body = c.postJSON("/api/estimate", map[string]any{"upload": id, "options": map[string]any{}})
	m := decode(t, body)
	if _, has := m["cost"]; has || m["sheets"] != float64(3) {
		t.Errorf("estimate = %s", body)
	}
}

func TestDraftExpired(t *testing.T) {
	e := newEnv(t, nil)
	now := time.Now()
	e.s.drafts = queue.NewDrafts(e.dir, time.Minute, func() time.Time { return now })
	c := e.client(t)
	_, body := c.upload("a.pdf", "%PDF-1")
	id := decode(t, body)["id"].(string)
	now = now.Add(time.Hour)
	resp, body := c.postJSON("/api/estimate", map[string]any{"upload": id, "options": map[string]any{}})
	if resp.StatusCode != 410 || decode(t, body)["error"] != "expired" {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		key    string
	}{
		{queue.ErrNotFound, 404, "not_found"},
		{queue.ErrForbidden, 404, "not_found"},
		{queue.ErrExpired, 410, "expired"},
		{queue.ErrBadPages, 400, "bad_pages"},
		{queue.ErrBadKey, 400, "bad_key"},
		{queue.ErrQueueFull, 503, "queue_full"},
		{queue.ErrNotActive, 409, "not_active"},
		{queue.ErrPrinter, 502, "printer_error"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeErr(rec, tc.err)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"`+tc.key+`"`) {
			t.Errorf("%v: %d %s", tc.err, rec.Code, rec.Body)
		}
	}
}

func TestPrinterEndpointUnreachable(t *testing.T) {
	e := newEnv(t, nil)
	e.fb.stErr = errors.New("dial tcp: no route")
	resp, body := e.client(t).get("/api/printer")
	m := decode(t, body)
	if resp.StatusCode != 200 || m["state"] != "unreachable" {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
	e.fb.stErr = nil
	e.fb.st = backend.Status{State: backend.StateReady, Reasons: []string{"toner-low"},
		Supplies: []backend.Supply{{Name: "Black", Color: "black", Level: 40}}}
	_, body = e.client(t).get("/api/printer")
	m = decode(t, body)
	if m["state"] != "ready" || len(m["supplies"].([]any)) != 1 || m["caps"] == nil {
		t.Errorf("printer = %s", body)
	}
}

func TestConfigEndpoint(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PrinterLocation = "Hall" })
	_, body := e.client(t).get("/api/config")
	m := decode(t, body)
	if m["name"] != "Test" || m["location"] != "Hall" || m["lang"] != "auto" || m["showNames"] != false ||
		!strings.HasPrefix(m["publicURL"].(string), "http://127.0.0.1:") {
		t.Errorf("config = %s", body)
	}
	e2 := newEnv(t, func(c *config.Config) { c.PublicURL = "http://printer.local:28431" })
	_, body = e2.client(t).get("/api/config")
	if decode(t, body)["publicURL"] != "http://printer.local:28431" {
		t.Errorf("config = %s", body)
	}
}

func TestVisitorCookieIsolation(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.client(t), e.client(t)
	resp, body := a.upload("a.pdf", "%PDF-1")
	id := decode(t, body)["id"].(string)
	ck := a.cookie
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Secure || len(ck.Value) != 32 || ck.MaxAge != 365*24*3600 {
		t.Fatalf("cookie = %+v (resp %d)", ck, resp.StatusCode)
	}
	// visitor B sees nothing of A's draft
	for _, p := range []string{"/api/uploads/" + id + "/pdf"} {
		resp, body := b.get(p)
		if resp.StatusCode != 404 || decode(t, body)["error"] != "not_found" {
			t.Errorf("B %s: %d %s", p, resp.StatusCode, body)
		}
	}
	resp, _ = b.delete("/api/uploads/" + id)
	if resp.StatusCode != 404 {
		t.Errorf("B delete draft: %d", resp.StatusCode)
	}
	resp, _ = b.postJSON("/api/estimate", map[string]any{"upload": id, "options": map[string]any{}})
	if resp.StatusCode != 404 {
		t.Errorf("B estimate: %d", resp.StatusCode)
	}
	resp, _ = b.postJSON("/api/jobs", map[string]any{"upload": id, "key": "x", "options": map[string]any{}})
	if resp.StatusCode != 404 {
		t.Errorf("B job: %d", resp.StatusCode)
	}
	// A prints; B cannot see the name, get or cancel the job
	_, body = a.postJSON("/api/jobs", map[string]any{"upload": id, "key": "x", "options": map[string]any{}})
	jid := decode(t, body)["id"].(string)
	resp, _ = b.get("/api/jobs/" + jid)
	if resp.StatusCode != 404 {
		t.Errorf("B get job: %d", resp.StatusCode)
	}
	resp, _ = b.delete("/api/jobs/" + jid)
	if resp.StatusCode != 404 {
		t.Errorf("B cancel: %d", resp.StatusCode)
	}
	_, body = b.get("/api/jobs")
	q := decode(t, body)["queue"].([]any)
	if len(q) != 1 || q[0].(map[string]any)["name"] != "" || q[0].(map[string]any)["mine"] != false {
		t.Errorf("B list = %s", body)
	}
	_, body = a.get("/api/jobs")
	q = decode(t, body)["queue"].([]any)
	if q[0].(map[string]any)["mine"] != true {
		t.Errorf("A list = %s", body)
	}
	// B's cookie differs, and a malformed cookie is replaced
	if b.cookie == nil || b.cookie.Value == ck.Value {
		t.Errorf("B cookie = %+v", b.cookie)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/config", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "not-hex"})
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	got := ""
	for _, c := range r.Cookies() {
		if c.Name == cookieName {
			got = c.Value
		}
	}
	if !validVisitor(got) {
		t.Errorf("malformed cookie not replaced: %q", got)
	}
}

func TestSecureCookieOnTLS(t *testing.T) {
	e := newEnv(t, nil)
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Host = "printer.local"
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	cs := rec.Result().Cookies()
	if len(cs) != 1 || !cs[0].Secure {
		t.Errorf("cookies = %+v", cs)
	}
}

func TestCSRF(t *testing.T) {
	e := newEnv(t, nil)
	c := e.client(t)
	_, body := c.upload("a.pdf", "%PDF-1")
	id := decode(t, body)["id"].(string)

	do := func(method, path, ctype, header, payload string) int {
		req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(payload))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if header != "" {
			req.Header.Set("X-Requested-With", header)
		}
		resp, _ := c.do(req)
		return resp.StatusCode
	}
	forbidden := []struct{ method, path, ctype, header string }{
		{"POST", "/api/jobs", "text/plain", ""},
		{"POST", "/api/jobs", "application/x-www-form-urlencoded", ""},
		{"POST", "/api/jobs", "", ""},
		{"POST", "/api/estimate", "text/plain", ""},
		{"POST", "/api/uploads", "multipart/form-data; boundary=x", ""},
		{"POST", "/api/uploads", "application/json", ""},
		{"DELETE", "/api/uploads/" + id, "", ""},
		{"DELETE", "/api/jobs/abc", "", "other"},
	}
	for _, tc := range forbidden {
		if got := do(tc.method, tc.path, tc.ctype, tc.header, "{}"); got != 403 {
			t.Errorf("%+v: got %d, want 403", tc, got)
		}
	}
	if got := do("POST", "/api/estimate", "application/json; charset=utf-8", "", "{bad"); got != 400 {
		t.Errorf("malformed json: got %d, want 400", got)
	}
	if got := do("DELETE", "/api/uploads/"+id, "", "printer-web", ""); got != 204 {
		t.Errorf("delete with header: got %d", got)
	}
}

func TestStaticServed(t *testing.T) {
	e := newEnv(t, nil)
	c := e.client(t)
	cases := []struct{ path, ctype, cache string }{
		{"/", "text/html", "no-cache"},
		{"/qr", "text/html", ""},
		{"/app.js", "text/javascript", ""},
		{"/style.css", "text/css", ""},
	}
	for _, tc := range cases {
		resp, body := c.get(tc.path)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), tc.ctype) || len(body) == 0 {
			t.Errorf("%s: %d %q", tc.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if tc.cache != "" && resp.Header.Get("Cache-Control") != tc.cache {
			t.Errorf("%s: Cache-Control %q", tc.path, resp.Header.Get("Cache-Control"))
		}
	}
	if resp, _ := c.get("/nope.js"); resp.StatusCode != 404 {
		t.Errorf("missing file: %d", resp.StatusCode)
	}
}

// The real embedded front end: pages, scripts, styles, manifest, vendor files and fonts.
func TestStaticServedEmbedded(t *testing.T) {
	e := newEnv(t, nil)
	e.s.assets = embedded.FS()
	ts := httptest.NewServer(e.s.Handler())
	t.Cleanup(ts.Close)
	c := &client{t: t, base: ts.URL}
	cases := []struct{ path, ctype string }{
		{"/", "text/html"},
		{"/qr", "text/html"},
		{"/app.js", "text/javascript"},
		{"/i18n.js", "text/javascript"},
		{"/qr.js", "text/javascript"},
		{"/app.css", "text/css"},
		{"/qr.css", "text/css"},
		{"/manifest.webmanifest", "application/manifest+json"},
		{"/icon.svg", "image/svg+xml"},
		{"/vendor/pdf.min.js", "text/javascript"},
		{"/vendor/pdf.worker.min.js", "text/javascript"},
		{"/vendor/qrcode.min.js", "text/javascript"},
		{"/vendor/fonts/BricolageGrotesque-latin.woff2", "font/woff2"},
		{"/vendor/fonts/AtkinsonHyperlegible-400-latin.woff2", "font/woff2"},
	}
	for _, tc := range cases {
		resp, body := c.get(tc.path)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), tc.ctype) || len(body) == 0 {
			t.Errorf("%s: %d %q", tc.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	for _, p := range []string{"/vendor", "/vendor/", "/vendor/fonts"} {
		if resp, _ := c.get(p); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestIndexRedirectAndNoListing(t *testing.T) {
	e := newEnv(t, nil)
	e.s.assets = fstest.MapFS{
		"index.html":     {Data: []byte("x")},
		"vendor/a.js":    {Data: []byte("x")},
		"vendor/b.woff2": {Data: []byte("x")},
	}
	h := e.s.Handler()
	for path, want := range map[string]int{"/index.html": 301, "/vendor": 404, "/vendor/": 404, "/vendor/a.js": 200, "/": 200} {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
	req := httptest.NewRequest("GET", "/index.html", nil)
	req.Host = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Location") != "./" {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
}

func TestEstimateNoCostWithoutCoverage(t *testing.T) {
	e := newEnv(t, nil)
	e.s.inkCov = func(context.Context, string) ([]docs.Coverage, error) { return nil, errors.New("no gs") }
	c := e.client(t)
	_, body := c.upload("a.pdf", "%PDF-1")
	id := decode(t, body)["id"].(string)
	resp, body := c.postJSON("/api/estimate", map[string]any{"upload": id, "options": map[string]any{}})
	m := decode(t, body)
	if resp.StatusCode != 200 || m["sheets"] != float64(3) {
		t.Fatalf("estimate: %d %s", resp.StatusCode, body)
	}
	if _, has := m["cost"]; has {
		t.Errorf("cost present without coverage: %s", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, nil)
	c := e.client(t)
	resp, _ := c.get("/api/config")
	want := map[string]string{
		"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
	}
	for k, v := range want {
		if resp.Header.Get(k) != v {
			t.Errorf("%s = %q", k, resp.Header.Get(k))
		}
	}
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") || !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", got)
	}
	_, body := c.upload("a.pdf", "%PDF-1")
	id := decode(t, body)["id"].(string)
	resp, _ = c.get("/api/uploads/" + id + "/pdf")
	if resp.Header.Get("Content-Security-Policy") != "sandbox" || resp.Header.Get("Content-Disposition") != "inline" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("pdf headers = %v", resp.Header)
	}
	if len(resp.Header.Values("Content-Security-Policy")) != 1 {
		t.Errorf("CSP values = %v", resp.Header.Values("Content-Security-Policy"))
	}
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"a\u202etxt.pdf": "atxt.pdf",
		"..":             "document",
		".":              "document",
		"":               "document",
		"\x00":           "document",
		"dir/../x.pdf":   "x.pdf",
		`C:\tmp\y.pdf`:   "y.pdf",
	}
	for in, want := range cases {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := []rune(cleanName(strings.Repeat("é", 300))); len(got) != 120 {
		t.Errorf("length = %d", len(got))
	}
}

func TestJSONLimits(t *testing.T) {
	e := newEnv(t, nil)
	c := e.client(t)
	big := `{"upload":"` + strings.Repeat("a", 100<<10) + `"}`
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/estimate", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	resp, body := c.do(req)
	if resp.StatusCode != 413 || decode(t, body)["error"] != "too_large" {
		t.Errorf("big: %d %s", resp.StatusCode, body)
	}
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/estimate", strings.NewReader(`{"upload":"x"} {"a":1}`))
	req.Header.Set("Content-Type", "application/json")
	resp, body = c.do(req)
	if resp.StatusCode != 400 || decode(t, body)["error"] != "bad_request" {
		t.Errorf("trailing: %d %s", resp.StatusCode, body)
	}
	// unknown fields are allowed (reaches the draft lookup)
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/estimate", strings.NewReader(`{"upload":"x","future":1}`+"\n"))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, _ = c.do(req)
	if resp.StatusCode != 404 {
		t.Errorf("charset content type: %d", resp.StatusCode)
	}
}

func TestUploadMissingFilePart(t *testing.T) {
	e := newEnv(t, nil)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("other", "x")
	_ = mw.Close()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/uploads", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "printer-web")
	resp, body := e.client(t).do(req)
	if resp.StatusCode != 400 || decode(t, body)["error"] != "bad_request" {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
}

func TestHostGuard(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PublicURL = "https://print.example.org" })
	h := e.s.Handler()
	cases := map[string]int{
		"192.168.1.50:28431": 200, "[::1]:28431": 200, "localhost:28431": 200, "printer.local": 200,
		"nas.lan": 200, "x.home.arpa": 200, "svc.internal:80": 200, "print.example.org": 200,
		"evil.example.com": 421, "evil.example.com:28431": 421, "": 421,
	}
	for host, want := range cases {
		req := httptest.NewRequest("GET", "/api/config", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %q: %d, want %d", host, rec.Code, want)
		}
		if want == 421 && !strings.Contains(rec.Body.String(), "misdirected") {
			t.Errorf("host %q: body %s", host, rec.Body)
		}
	}
}
