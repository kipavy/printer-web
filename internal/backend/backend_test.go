package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/ipp"
	"github.com/kipavy/printer-web/internal/ipp/ipptest"
)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func writeFile(t *testing.T, name, content string) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestIPP(t *testing.T, attrs map[string][]any) (*ippBackend, *ipptest.Server) {
	srv := ipptest.NewServer(t, attrs)
	b := newIPP(config.Config{Backend: "ipp", PrinterURI: srv.URL})
	return b, srv
}

func TestIPPCaps(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("pages-per-minute", 20)
	srv.SetAttr("pages-per-minute-color", 10)
	srv.SetAttr("media-source-supported", "auto", "tray-1")
	c, err := b.Caps(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Color || !c.Duplex || !c.Quality || !c.CanTrack || !c.CanCancel {
		t.Errorf("caps = %+v", c)
	}
	if strings.Join(c.Papers, ",") != "A4,Letter,A5" {
		t.Errorf("papers = %v", c.Papers)
	}
	if c.Model != "Fake Printer 1000" || c.PPM != 20 || c.PPMColor != 10 || c.DPI != 600 {
		t.Errorf("caps = %+v", c)
	}
	if len(c.Trays) != 2 || len(c.Formats) == 0 {
		t.Errorf("trays/formats = %v %v", c.Trays, c.Formats)
	}
}

func TestIPPCapsMonoSimplex(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("color-supported", false)
	srv.SetAttr("sides-supported", "one-sided")
	srv.DeleteAttr("print-quality-supported")
	c, err := b.Caps(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.Color || c.Duplex || c.Quality {
		t.Errorf("caps = %+v", c)
	}
}

func TestIPPStatus(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("printer-state", 4)
	srv.SetAttr("printer-state-reasons", "toner-low-warning", "media-empty-error", "none")
	srv.SetAttr("marker-names", "Black", "Cyan")
	srv.SetAttr("marker-colors", "#000000", "#00FFFF")
	srv.SetAttr("marker-levels", 80, -2)
	st, err := b.Status(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StatePrinting {
		t.Errorf("state = %s", st.State)
	}
	if strings.Join(st.Reasons, ",") != "toner-low,media-empty" {
		t.Errorf("reasons = %v", st.Reasons)
	}
	if len(st.Supplies) != 2 || st.Supplies[0] != (Supply{"Black", "#000000", 80}) || st.Supplies[1].Level != -1 {
		t.Errorf("supplies = %+v", st.Supplies)
	}
}

func TestIPPStatusNoneDropped(t *testing.T) {
	b, _ := newTestIPP(t, nil)
	st, err := b.Status(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateReady || len(st.Reasons) != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestIPPStatusUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	b := newIPP(config.Config{Backend: "ipp", PrinterURI: "ipp://" + addr + "/ipp/print"})
	st, err := b.Status(ctx(t))
	if err != nil || st.State != StateUnreachable {
		t.Errorf("status = %+v, err = %v", st, err)
	}
}

func TestIPPCache(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	now := time.Now()
	b.cache.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := b.Status(ctx(t)); err != nil {
			t.Fatal(err)
		}
	}
	if n := srv.OpCount(ipp.OpGetPrinterAttributes); n != 1 {
		t.Fatalf("ops = %d, want 1", n)
	}
	now = now.Add(11 * time.Second)
	if _, err := b.Status(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if n := srv.OpCount(ipp.OpGetPrinterAttributes); n != 2 {
		t.Fatalf("ops = %d, want 2", n)
	}
}

func submitIPP(t *testing.T, formats []string, o Options, setup ...func(*ipptest.Server)) (*ipptest.Server, *[]string, string) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("document-format-supported", anySlice(formats)...)
	for _, s := range setup {
		s(srv)
	}
	pdf := writeFile(t, "in.pdf", "%PDF-1.4 fake")
	var asked []string
	var converted string
	b.convert = func(_ context.Context, in, format string, _ bool, _, _ float64, tmp string, _ ...string) (string, error) {
		asked = append(asked, format)
		if format == "application/pdf" {
			return in, nil
		}
		converted = filepath.Join(tmp, "conv.bin")
		return converted, os.WriteFile(converted, []byte("RASTER-DATA"), 0o600)
	}
	b.tmp = t.TempDir()
	h, err := b.Submit(ctx(t), pdf, "doc", o)
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != 1 {
		t.Errorf("handle = %+v", h)
	}
	if _, err := os.Stat(pdf); err != nil {
		t.Errorf("input pdf was removed: %v", err)
	}
	return srv, &asked, converted
}

func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func TestIPPSubmitPrefersPDF(t *testing.T) {
	srv, asked, _ := submitIPP(t, []string{"image/urf", "application/pdf", "image/pwg-raster"}, Options{Copies: 2})
	if len(*asked) != 1 || (*asked)[0] != "application/pdf" {
		t.Errorf("asked = %v", *asked)
	}
	j := srv.Jobs()[0]
	if string(j.Document) != "%PDF-1.4 fake" || j.ContentLength <= int64(len(j.Document)) || j.Chunked {
		t.Errorf("job = %+v", j)
	}
}

func TestIPPSubmitPWGAndCleanup(t *testing.T) {
	srv, asked, converted := submitIPP(t, []string{"application/postscript", "image/pwg-raster"}, Options{})
	if (*asked)[0] != "image/pwg-raster" {
		t.Errorf("asked = %v", *asked)
	}
	j := srv.Jobs()[0]
	if string(j.Document) != "RASTER-DATA" || j.ContentLength <= 11 || j.Chunked {
		t.Errorf("job = %+v", j)
	}
	if _, err := os.Stat(converted); !os.IsNotExist(err) {
		t.Errorf("converted file not removed: %v", err)
	}
}

func TestIPPSubmitOrder(t *testing.T) {
	for _, tc := range []struct {
		formats []string
		want    string
	}{
		{[]string{"application/postscript", "image/urf"}, "image/urf"},
		{[]string{"application/postscript"}, "application/postscript"},
	} {
		_, asked, _ := submitIPP(t, tc.formats, Options{})
		if (*asked)[0] != tc.want {
			t.Errorf("%v: asked %v, want %s", tc.formats, *asked, tc.want)
		}
	}
}

func TestIPPSubmitNoFormat(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("document-format-supported", "image/jpeg")
	pdf := writeFile(t, "in.pdf", "x")
	if _, err := b.Submit(ctx(t), pdf, "d", Options{}); err == nil {
		t.Fatal("want error")
	}
}

func TestIPPSubmitAttrs(t *testing.T) {
	srv, _, _ := submitIPP(t, []string{"application/pdf"}, Options{
		Copies: 3, Sides: "long", Color: false, Paper: "A4", Orientation: "landscape", Quality: "fine", Tray: "tray-2",
	}, func(s *ipptest.Server) {
		s.SetAttr("media-source-supported", "auto", "tray-2")
	})
	a := srv.Jobs()[0].Attrs
	want := map[string]any{
		"copies": int32(3), "sides": "two-sided-long-edge", "print-color-mode": "monochrome",
		"media": "iso_a4_210x297mm", "orientation-requested": int32(4), "print-quality": int32(5),
		"media-source": "tray-2",
	}
	for k, v := range want {
		if len(a[k]) != 1 || a[k][0] != v {
			t.Errorf("%s = %v, want %v", k, a[k], v)
		}
	}
}

func TestIPPSubmitAutoOrientationColor(t *testing.T) {
	srv, _, _ := submitIPP(t, []string{"application/pdf"}, Options{Copies: 1, Sides: "short", Color: true, Paper: "Letter", Orientation: "auto"})
	a := srv.Jobs()[0].Attrs
	if _, ok := a["orientation-requested"]; ok {
		t.Error("orientation sent for auto")
	}
	if a["print-color-mode"][0] != "color" || a["sides"][0] != "two-sided-short-edge" || a["media"][0] != "na_letter_8.5x11in" {
		t.Errorf("attrs = %v", a)
	}
}

func TestIPPTrackAndCancel(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetJobStates(ipp.JobPending, ipp.JobProcessing, ipp.JobCompleted)
	pdf := writeFile(t, "in.pdf", "%PDF")
	h, err := b.Submit(ctx(t), pdf, "d", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []JobState
	for i := 0; i < 3; i++ {
		s, err := b.Track(ctx(t), h)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if got[0] != JobPrinting || got[1] != JobDone || got[2] != JobDone {
		t.Errorf("states = %v", got)
	}
	srv.SetJobStates(ipp.JobPending)
	h2, _ := b.Submit(ctx(t), pdf, "d", Options{})
	if err := b.Cancel(ctx(t), h2); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Track(ctx(t), h2); s != JobCanceled {
		t.Errorf("state = %s", s)
	}
}

func TestIPPTrackAborted(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetJobStates(ipp.JobAborted)
	h, err := b.Submit(ctx(t), writeFile(t, "in.pdf", "%PDF"), "d", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Track(ctx(t), h); s != JobFailed {
		t.Errorf("state = %s", s)
	}
}

func TestNewSelectsBackend(t *testing.T) {
	for name, want := range map[string]string{"ipp": "*backend.ippBackend", "cups": "*backend.ippBackend", "raw": "*backend.rawBackend", "command": "*backend.commandBackend"} {
		b, err := New(config.Config{Backend: name, PrinterURI: "ipp://127.0.0.1:631/x"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := typeName(b); got != want {
			t.Errorf("%s: %s", name, got)
		}
	}
	if _, err := New(config.Config{Backend: "nope"}, ""); err == nil {
		t.Error("want error")
	}
}

func typeName(b Backend) string {
	switch b.(type) {
	case *ippBackend:
		return "*backend.ippBackend"
	case *rawBackend:
		return "*backend.rawBackend"
	case *commandBackend:
		return "*backend.commandBackend"
	}
	return "?"
}

// --- raw ---

func listen(t *testing.T) (string, <-chan []byte) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ch := make(chan []byte, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(c)
			c.Close()
			ch <- b
		}
	}()
	return l.Addr().String(), ch
}

func rawConfig(addr, lang string) config.Config {
	return config.Config{
		Backend: "raw", PrinterURI: "socket://" + addr, RawLanguage: lang, Model: "M", PPM: 12, DPI: 600,
		Declared: config.Caps{Color: true, Duplex: true, Papers: []string{"A4"}, Trays: []string{"t1"}},
	}
}

func TestRawSubmitPS(t *testing.T) {
	addr, ch := listen(t)
	b := newRaw(rawConfig(addr, "ps"))
	var asked string
	b.convert = func(_ context.Context, _, format string, _ bool, _, _ float64, tmp string, _ ...string) (string, error) {
		asked = format
		p := filepath.Join(tmp, "o.ps")
		return p, os.WriteFile(p, []byte("%!PS-Adobe-3.0\nshowpage\n"), 0o600)
	}
	b.tmp = t.TempDir()
	if _, err := b.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{Copies: 2}); err != nil {
		t.Fatal(err)
	}
	got := string(<-ch)
	if asked != "application/postscript" || !strings.HasPrefix(got, "%!PS") {
		t.Errorf("asked %q got %q", asked, got)
	}
	if strings.Count(got, "%!PS-Adobe") != 2 {
		t.Errorf("copies: %q", got)
	}
	if entries, _ := os.ReadDir(b.tmp); len(entries) != 0 {
		t.Errorf("temp left: %v", entries)
	}
}

func TestRawSubmitPCL(t *testing.T) {
	addr, ch := listen(t)
	b := newRaw(rawConfig(addr, "pcl"))
	var asked string
	b.convert = func(_ context.Context, _, format string, _ bool, _, _ float64, tmp string, _ ...string) (string, error) {
		asked = format
		p := filepath.Join(tmp, "o.pcl")
		return p, os.WriteFile(p, []byte("\x1bE"), 0o600)
	}
	b.tmp = t.TempDir()
	if _, err := b.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{}); err != nil {
		t.Fatal(err)
	}
	if got := <-ch; asked != "application/vnd.hp-pcl" || string(got) != "\x1bE" {
		t.Errorf("asked %q got %q", asked, got)
	}
}

func TestRawStatusCapsTrackCancel(t *testing.T) {
	addr, _ := listen(t)
	b := newRaw(rawConfig(addr, "ps"))
	if st, _ := b.Status(ctx(t)); st.State != StateReady {
		t.Errorf("state = %s", st.State)
	}
	c, _ := b.Caps(ctx(t))
	if !c.Color || !c.Duplex || c.Model != "M" || c.PPM != 12 || c.DPI != 600 || c.CanTrack || c.CanCancel || c.Papers[0] != "A4" {
		t.Errorf("caps = %+v", c)
	}
	if s, err := b.Track(ctx(t), Handle{}); s != JobSent || err != nil {
		t.Errorf("track = %s %v", s, err)
	}
	if err := b.Cancel(ctx(t), Handle{}); !errors.Is(err, ErrNotSupported) {
		t.Errorf("cancel = %v", err)
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	b2 := newRaw(rawConfig(dead, "ps"))
	if st, _ := b2.Status(ctx(t)); st.State != StateUnreachable {
		t.Errorf("state = %s", st.State)
	}
	if _, err := b2.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{}); err == nil {
		t.Error("submit to dead socket should fail")
	}
}

// --- command ---

// TestHelperProcess is re-executed as the print command.
func TestHelperProcess(t *testing.T) {
	out := os.Getenv("HELPER_OUT")
	if out == "" {
		return
	}
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
		}
	}
	data, _ := json.Marshal(args)
	_ = os.WriteFile(out, data, 0o600)
	if msg := os.Getenv("HELPER_STDERR"); msg != "" {
		os.Stderr.WriteString(msg)
	}
	if os.Getenv("HELPER_SLEEP") != "" {
		time.Sleep(30 * time.Second)
	}
	if os.Getenv("HELPER_FAIL") != "" {
		os.Exit(3)
	}
	os.Exit(0)
}

func helperBackend(t *testing.T, tmpl ...string) (*commandBackend, string) {
	out := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("HELPER_OUT", out)
	cmd := append([]string{os.Args[0], "-test.run=^TestHelperProcess$", "--"}, tmpl...)
	b, err := newCommand(config.Config{Backend: "command", Command: cmd})
	if err != nil {
		t.Fatal(err)
	}
	return b, out
}

func readArgv(t *testing.T, out string) []string {
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var a []string
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCommandArgv(t *testing.T) {
	b, out := helperBackend(t, "{file}", "n={name}", "c={copies}", "d={duplex}", "k={color}", "p={paper}", "o={orientation}", "q={quality}", "t={tray}", "{mono_flag}", "{landscape_flag}")
	_, err := b.Submit(ctx(t), "/x/a.pdf", "My doc", Options{Copies: 2, Sides: "short", Color: false, Paper: "A4", Orientation: "landscape", Quality: "fine", Tray: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{abs("/x/a.pdf"), "n=My doc", "c=2", "d=short", "k=mono", "p=A4", "o=landscape", "q=fine", "t=t1", "--mono", "--landscape"}
	if got := readArgv(t, out); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("argv = %q\nwant %q", got, want)
	}
}

func TestCommandFlagsRemoved(t *testing.T) {
	b, out := helperBackend(t, "a", "{mono_flag}", "{landscape_flag}", "b", "{duplex}/{color}")
	if _, err := b.Submit(ctx(t), "f", "n", Options{Color: true, Orientation: "portrait"}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, out); strings.Join(got, "|") != "a|b|none/color" {
		t.Errorf("argv = %q", got)
	}
}

func TestNoShell(t *testing.T) {
	b, out := helperBackend(t, "{name}", "{file}")
	name := "x; rm -rf /"
	weird := "/tmp/$(id) `id`.pdf"
	if _, err := b.Submit(ctx(t), weird, name, Options{}); err != nil {
		t.Fatal(err)
	}
	got := readArgv(t, out)
	if len(got) != 2 || got[0] != name || got[1] != abs(weird) {
		t.Errorf("argv = %q", got)
	}
}

func TestNoReexpansion(t *testing.T) {
	got := buildArgv([]string{"{name}"}, "f", "{file}", Options{})
	if len(got) != 1 || got[0] != "{file}" {
		t.Errorf("argv = %q", got)
	}
}

func TestCommandFailure(t *testing.T) {
	b, _ := helperBackend(t, "x")
	t.Setenv("HELPER_FAIL", "1")
	t.Setenv("HELPER_STDERR", "paper jam in tray")
	_, err := b.Submit(ctx(t), "f", "n", Options{})
	if err == nil || !strings.Contains(err.Error(), "paper jam in tray") {
		t.Errorf("err = %v", err)
	}
}

func TestCommandStatusAndNoOps(t *testing.T) {
	b, _ := helperBackend(t, "x")
	if st, _ := b.Status(ctx(t)); st.State != StateReady {
		t.Errorf("state = %s", st.State)
	}
	missing, _ := newCommand(config.Config{Backend: "command", Command: []string{"definitely-not-a-binary-xyz", "{file}"}})
	if st, _ := missing.Status(ctx(t)); st.State != StateUnreachable {
		t.Errorf("state = %s", st.State)
	}
	if _, err := missing.Submit(ctx(t), "f", "n", Options{}); err == nil {
		t.Error("missing binary should fail")
	}
	if s, err := b.Track(ctx(t), Handle{}); s != JobSent || err != nil {
		t.Errorf("track = %s %v", s, err)
	}
	if err := b.Cancel(ctx(t), Handle{}); !errors.Is(err, ErrNotSupported) {
		t.Errorf("cancel = %v", err)
	}
	c, _ := b.Caps(ctx(t))
	if c.CanTrack || c.CanCancel {
		t.Errorf("caps = %+v", c)
	}
}

// --- fix round 1 ---

func abs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func TestCacheSingleFlight(t *testing.T) {
	c := newCache(time.Second)
	var n atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.do(context.Background(), "k", func() (any, error) {
				n.Add(1)
				time.Sleep(50 * time.Millisecond)
				return 7, nil
			})
			if err != nil || v != 7 {
				t.Errorf("v=%v err=%v", v, err)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Errorf("fetches = %d, want 1", n.Load())
	}
}

func TestCacheFailureTTL(t *testing.T) {
	c := newCache(10 * time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }
	n := 0
	fetch := func() (any, error) { n++; return nil, errors.New("boom") }
	c.do(context.Background(), "k", fetch)
	c.do(context.Background(), "k", fetch)
	if n != 1 {
		t.Fatalf("fetches = %d", n)
	}
	now = now.Add(4 * time.Second)
	c.do(context.Background(), "k", fetch)
	if n != 2 {
		t.Fatalf("failure cached too long: %d", n)
	}
}

func TestCacheSkipsCancelled(t *testing.T) {
	c := newCache(time.Second)
	cx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	fetch := func() (any, error) { n++; return nil, cx.Err() }
	c.do(cx, "k", fetch)
	c.do(cx, "k", fetch)
	if n != 2 {
		t.Errorf("cancelled error was cached: %d", n)
	}
}

func TestIPPStatusCtxCancelled(t *testing.T) {
	b, _ := newTestIPP(t, nil)
	cx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Status(cx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestIPPCachedSlicesAreCloned(t *testing.T) {
	b, _ := newTestIPP(t, nil)
	c1, _ := b.Caps(ctx(t))
	c1.Papers[0] = "X"
	c2, _ := b.Caps(ctx(t))
	if c2.Papers[0] == "X" {
		t.Error("cache returned shared slice")
	}
}

func TestIPPSupplyWasteAndNotAccepting(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("marker-names", "Black", "Waste Toner")
	srv.SetAttr("marker-types", "toner", "waste-toner")
	srv.SetAttr("marker-levels", 50, 10)
	srv.SetAttr("printer-is-accepting-jobs", false)
	st, err := b.Status(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Supplies) != 1 || st.Supplies[0].Name != "Black" {
		t.Errorf("supplies = %+v", st.Supplies)
	}
	if st.State != StateStopped {
		t.Errorf("state = %s", st.State)
	}
}

func TestIPPQualityNeedsTwoValues(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("print-quality-supported", 4)
	if c, _ := b.Caps(ctx(t)); c.Quality {
		t.Error("Quality true with one value")
	}
}

func TestIPPPapersDefaultFirstAndVariants(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("media-supported", "iso_a4_210x297mm", "na_legal_8.5x14in", "na_letter_8.5x11in", "iso_a3_297x420mm", "custom_foo_1x1mm")
	srv.SetAttr("media-default", "na_letter_8.5x11in")
	c, _ := b.Caps(ctx(t))
	if got := strings.Join(c.Papers, ","); got != "Letter,A4,Legal,A3" {
		t.Errorf("papers = %s", got)
	}
}

func TestIPPSubmitGates(t *testing.T) {
	// Simplex mono printer without trays or quality: duplex, quality and tray are dropped.
	srv, _, _ := submitIPP(t, []string{"application/pdf"}, Options{Sides: "long", Quality: "fine", Tray: "tray-9"},
		func(s *ipptest.Server) {
			s.SetAttr("sides-supported", "one-sided")
			s.SetAttr("print-quality-supported", 4)
			s.SetAttr("media-source-supported", "auto")
		})
	a := srv.Jobs()[0].Attrs
	for _, k := range []string{"sides", "print-quality", "media-source"} {
		if _, ok := a[k]; ok {
			t.Errorf("%s was sent", k)
		}
	}
	// one-sided is always allowed.
	srv, _, _ = submitIPP(t, []string{"application/pdf"}, Options{Sides: "one"},
		func(s *ipptest.Server) { s.SetAttr("sides-supported", "one-sided") })
	if got := srv.Jobs()[0].Attrs["sides"]; len(got) != 1 || got[0] != "one-sided" {
		t.Errorf("sides = %v", got)
	}
}

func TestIPPSubmitUnsupportedPaper(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	_, err := b.Submit(ctx(t), writeFile(t, "in.pdf", "%PDF"), "d", Options{Paper: "Legal"})
	if err == nil || !strings.Contains(err.Error(), "paper not supported") {
		t.Errorf("err = %v", err)
	}
	_, err = b.Submit(ctx(t), writeFile(t, "in.pdf", "%PDF"), "d", Options{Paper: "bogus"})
	if err == nil {
		t.Error("unknown paper accepted")
	}
	if len(srv.Jobs()) != 0 {
		t.Error("job was sent")
	}
}

func TestIPPCopiesClamped(t *testing.T) {
	for in, want := range map[int]int32{0: 1, -3: 1, 5: 5, 500: 99} {
		srv, _, _ := submitIPP(t, []string{"application/pdf"}, Options{Copies: in})
		if got := srv.Jobs()[0].Attrs["copies"][0]; got != want {
			t.Errorf("copies %d -> %v, want %d", in, got, want)
		}
	}
}

func TestIPPMimeCaseInsensitive(t *testing.T) {
	srv, asked, _ := submitIPP(t, []string{"Application/PDF"}, Options{})
	_ = srv
	if (*asked)[0] != "application/pdf" {
		t.Errorf("asked = %v", *asked)
	}
}

func TestIPPTrackPurgedAndUnknown(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetJobStates(ipp.JobPending)
	h, err := b.Submit(ctx(t), writeFile(t, "in.pdf", "%PDF"), "d", Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetJobState(h.ID, 42)
	if s, err := b.Track(ctx(t), h); s != JobPrinting || err != nil {
		t.Errorf("unknown state: %s %v", s, err)
	}
	srv.PurgeJob(h.ID)
	if s, err := b.Track(ctx(t), h); s != JobDone || err != nil {
		t.Errorf("purged: %s %v", s, err)
	}
}

func TestSubmitErrorCleansTemp(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.FailOp(ipp.OpPrintJob, ipp.StatusNotPossible)
	b.tmp = t.TempDir()
	b.convert = func(_ context.Context, _, _ string, _ bool, _, _ float64, tmp string, _ ...string) (string, error) {
		p := filepath.Join(tmp, "conv.bin")
		return p, os.WriteFile(p, []byte("X"), 0o600)
	}
	srv.SetAttr("document-format-supported", "image/pwg-raster")
	if _, err := b.Submit(ctx(t), writeFile(t, "in.pdf", "%PDF"), "d", Options{}); err == nil {
		t.Fatal("want error")
	}
	if entries, _ := os.ReadDir(b.tmp); len(entries) != 0 {
		t.Errorf("temp left: %v", entries)
	}
}

func TestRawStatusBusyAndCache(t *testing.T) {
	addr, _ := listen(t)
	b := newRaw(rawConfig(addr, "ps"))
	b.busy.Add(1)
	// Close the listener's reachability: busy must not dial.
	b.addr = "127.0.0.1:1"
	if st, _ := b.Status(ctx(t)); st.State != StatePrinting {
		t.Errorf("state = %s", st.State)
	}
	b.busy.Add(-1)
	cx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Status(cx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestRawCopiesClamped(t *testing.T) {
	addr, ch := listen(t)
	b := newRaw(rawConfig(addr, "ps"))
	b.tmp = t.TempDir()
	b.convert = func(_ context.Context, _, _ string, _ bool, _, _ float64, tmp string, _ ...string) (string, error) {
		p := filepath.Join(tmp, "o.ps")
		return p, os.WriteFile(p, []byte("%!PS\nshowpage\n"), 0o600)
	}
	if _, err := b.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{Copies: 1000}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(<-ch), "showpage"); n != 99 {
		t.Errorf("copies = %d", n)
	}
}

func TestCommandDashName(t *testing.T) {
	b, out := helperBackend(t, "{name}", "x")
	if _, err := b.Submit(ctx(t), "f", "-rf", Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, out); strings.Join(got, "|") != "rf|x" {
		t.Errorf("argv = %q", got)
	}
	// A name made only of dashes disappears.
	if _, err := b.Submit(ctx(t), "f", "---", Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, out); strings.Join(got, "|") != "x" {
		t.Errorf("argv = %q", got)
	}
}

func TestCommandRejectsUnsafeValues(t *testing.T) {
	b, _ := helperBackend(t, "{paper}")
	for _, o := range []Options{{Paper: "-x"}, {Tray: "a b"}, {Quality: "--evil"}, {Paper: "A4;ls"}, {Sides: "diag"}, {Orientation: "up"}} {
		if _, err := b.Submit(ctx(t), "f", "n", o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestCommandEmptyElementDropped(t *testing.T) {
	b, out := helperBackend(t, "a", "{paper}", "b", "--tray={tray}")
	if _, err := b.Submit(ctx(t), "f", "n", Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, out); strings.Join(got, "|") != "a|b|--tray=" {
		t.Errorf("argv = %q", got)
	}
}

func TestCommandEmbeddedFlagRejected(t *testing.T) {
	for _, tok := range []string{"--x={mono_flag}", "{landscape_flag}y"} {
		if _, err := New(config.Config{Backend: "command", Command: []string{"prog", tok}}, ""); err == nil {
			t.Errorf("%q accepted", tok)
		}
	}
	if _, err := New(config.Config{Backend: "command", Command: []string{"prog", "{mono_flag}"}}, ""); err != nil {
		t.Error(err)
	}
}

func TestCommandCopiesClamped(t *testing.T) {
	if got := buildArgv([]string{"p", "{copies}"}, "f", "n", Options{Copies: 5000}); got[1] != "99" {
		t.Errorf("argv = %q", got)
	}
	if got := buildArgv([]string{"p", "{copies}"}, "f", "n", Options{}); got[1] != "1" {
		t.Errorf("argv = %q", got)
	}
}

func TestTailWriter(t *testing.T) {
	w := &tailWriter{n: 5}
	w.Write([]byte("abc"))
	w.Write([]byte("defgh"))
	if string(w.buf) != "defgh" {
		t.Errorf("buf = %q", w.buf)
	}
}

func TestCommandTimeout(t *testing.T) {
	b, _ := helperBackend(t, "x")
	t.Setenv("HELPER_SLEEP", "1")
	b.timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := b.Submit(ctx(t), "f", "n", Options{})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestCommandDefaultPaper(t *testing.T) {
	if got := withDefaultPaper(Options{}, []string{"A4", "A5"}); got.Paper != "A4" {
		t.Errorf("paper = %q, want A4", got.Paper)
	}
	if got := withDefaultPaper(Options{Paper: "A5"}, []string{"A4"}); got.Paper != "A5" {
		t.Errorf("paper = %q, want A5", got.Paper)
	}
	if got := withDefaultPaper(Options{}, nil); got.Paper != "" {
		t.Errorf("paper = %q, want empty", got.Paper)
	}
}
