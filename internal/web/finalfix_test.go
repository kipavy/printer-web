package web

import (
	"context"
	"errors"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/docs"
)

func captureLog(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

func TestCleanNameWhitespace(t *testing.T) {
	for _, in := range []string{" ", "   \t ", "  ", "dir/  "} {
		if got := cleanName(in); got != "document" {
			t.Errorf("cleanName(%q) = %q", in, got)
		}
	}
}

func TestHostGuardTrailingDot(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PublicURL = "http://printbox" })
	h := e.s.Handler()
	for host, want := range map[string]int{"printer.local.": 200, "printer.local.:28431": 200, "printbox.": 200, "printbox": 200, "evil.com.": 421} {
		req := httptest.NewRequest("GET", "/api/config", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%q: %d, want %d", host, rec.Code, want)
		}
	}
}

func TestMisdirectedPageAndHeaders(t *testing.T) {
	buf := captureLog(t)
	e := newEnv(t, nil)
	h := e.s.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "printbox:28431"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := get("/")
	if rec.Code != 421 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("page: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if body := rec.Body.String(); !strings.Contains(body, "printbox:28431") || !strings.Contains(body, "PUBLIC_URL") {
		t.Errorf("body = %q", body)
	}
	for _, k := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Referrer-Policy"} {
		if rec.Header().Get(k) == "" {
			t.Errorf("421 lacks %s", k)
		}
	}
	rec = get("/api/printer")
	if rec.Code != 421 || !strings.Contains(rec.Body.String(), `"misdirected"`) {
		t.Errorf("api: %d %s", rec.Code, rec.Body)
	}
	get("/qr")
	if n := strings.Count(buf.String(), `refused request for host "printbox:28431"`); n != 1 {
		t.Errorf("logged %d times: %q", n, buf.String())
	}
}

func TestUploadStoresOrientationAndLogsFailures(t *testing.T) {
	buf := captureLog(t)
	e := newEnv(t, nil)
	e.s.orientation = func(string) string { return "landscape" }
	c := e.client(t)
	resp, body := c.upload("secret-plan.pdf", "%PDF-1.4 x")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	id, _ := decode(t, body)["id"].(string)
	dr, err := e.s.drafts.Get(id, c.cookie.Value)
	if err != nil || dr.Orientation != "landscape" {
		t.Errorf("draft %+v %v", dr, err)
	}

	e.s.normalize = func(context.Context, string, docs.Kind, float64, float64, string) (string, error) {
		return "", errors.New("enscript: exit status 1")
	}
	if resp, _ := c.upload("secret-notes.txt", "TEXT hi"); resp.StatusCode != 422 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	out := buf.String()
	if !strings.Contains(out, "conversion_failed") || !strings.Contains(out, "enscript: exit status 1") {
		t.Errorf("log = %q", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("log leaks the file name: %q", out)
	}
}

func TestPaperSizeShared(t *testing.T) {
	for _, c := range []struct {
		papers []string
		w, h   float64
	}{
		{nil, 595.28, 841.89}, {[]string{"Letter"}, 612, 792}, {[]string{"a6"}, 297.64, 419.53},
		{[]string{"B5"}, 498.9, 708.66}, {[]string{"Custom"}, 595.28, 841.89},
	} {
		if w, h := paperSize(c.papers); w != c.w || h != c.h {
			t.Errorf("%v: %v x %v", c.papers, w, h)
		}
	}
}
