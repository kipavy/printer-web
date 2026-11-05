package backend

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kipavy/printer-web/internal/config"
)

func TestPaperSizeTable(t *testing.T) {
	for name, want := range map[string][2]float64{
		"A4": {595.28, 841.89}, "letter": {612, 792}, "Legal": {612, 1008}, "A5": {419.53, 595.28},
		"A3": {841.89, 1190.55}, "A6": {297.64, 419.53}, "B5": {498.9, 708.66},
	} {
		w, h, ok := PaperSize(name)
		if !ok || w != want[0] || h != want[1] {
			t.Errorf("%s: %v %v %v", name, w, h, ok)
		}
	}
	if _, _, ok := PaperSize("Tabloid"); ok {
		t.Error("unknown paper")
	}
	if s := sheetFor("", []string{"Letter", "A4"}); s.w != 612 {
		t.Errorf("default = %+v", s)
	}
	if s := sheetFor("A5", []string{"Letter"}); s.w != 419.53 {
		t.Errorf("chosen = %+v", s)
	}
	if s := sheetFor("Custom", nil); s.w != 595.28 || s.h != 841.89 {
		t.Errorf("fallback = %+v", s)
	}
}

// fakeConv records what the backend asked for.
type fakeConv struct {
	w, h  float64
	extra []string
	body  string
}

func (f *fakeConv) conv(_ context.Context, _, _ string, _ bool, w, h float64, tmp string, extra ...string) (string, error) {
	f.w, f.h, f.extra = w, h, extra
	p := filepath.Join(tmp, "o.bin")
	return p, os.WriteFile(p, []byte(f.body), 0o600)
}

func TestRawDuplexPS(t *testing.T) {
	for sides, want := range map[string]string{
		"long":  "%!PS\n<< /Duplex true /Tumble false >> setpagedevice\n%!PS-Adobe-3.0\n",
		"short": "%!PS\n<< /Duplex true /Tumble true >> setpagedevice\n%!PS-Adobe-3.0\n",
		"one":   "%!PS\n<< /Duplex false >> setpagedevice\n%!PS-Adobe-3.0\n",
	} {
		addr, ch := listen(t)
		b := newRaw(rawConfig(addr, "ps"))
		f := &fakeConv{body: "%!PS-Adobe-3.0\n"}
		b.convert, b.tmp = f.conv, t.TempDir()
		if _, err := b.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{Sides: sides}); err != nil {
			t.Fatal(err)
		}
		if got := string(<-ch); got != want {
			t.Errorf("%s: stream %q", sides, got)
		}
		if len(f.extra) != 0 {
			t.Errorf("%s: ps gets no gs flags: %v", sides, f.extra)
		}
	}
	// Duplex not declared: the stream is untouched.
	addr, ch := listen(t)
	cfg := rawConfig(addr, "ps")
	cfg.Declared.Duplex = false
	b := newRaw(cfg)
	b.convert, b.tmp = (&fakeConv{body: "%!PS-Adobe-3.0\n"}).conv, t.TempDir()
	if _, err := b.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{Sides: "long"}); err != nil {
		t.Fatal(err)
	}
	if got := string(<-ch); got != "%!PS-Adobe-3.0\n" {
		t.Errorf("no duplex: %q", got)
	}
}

func TestRawDuplexPCL(t *testing.T) {
	for sides, want := range map[string]string{
		"long":  "-dDuplex=true -dTumble=false",
		"short": "-dDuplex=true -dTumble=true",
		"one":   "-dDuplex=false",
	} {
		addr, ch := listen(t)
		b := newRaw(rawConfig(addr, "pcl"))
		f := &fakeConv{body: "\x1bE"}
		b.convert, b.tmp = f.conv, t.TempDir()
		if _, err := b.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{Sides: sides}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(f.extra, " "); got != want {
			t.Errorf("%s: flags %q", sides, got)
		}
		if got := <-ch; string(got) != "\x1bE" {
			t.Errorf("%s: pcl stream changed: %q", sides, got)
		}
	}
}

func TestRawCapsDuplex(t *testing.T) {
	for lang, want := range map[string]bool{"ps": true, "pcl": true, "zpl": false} {
		b := newRaw(rawConfig("127.0.0.1:1", lang))
		if c, _ := b.Caps(ctx(t)); c.Duplex != want {
			t.Errorf("%s: duplex %v", lang, c.Duplex)
		}
	}
}

func TestRawPaperSize(t *testing.T) {
	addr, ch := listen(t)
	cfg := rawConfig(addr, "ps")
	cfg.Declared.Papers = []string{"Letter", "A4"}
	b := newRaw(cfg)
	f := &fakeConv{body: "%!PS\n"}
	b.convert, b.tmp = f.conv, t.TempDir()
	if _, err := b.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{}); err != nil {
		t.Fatal(err)
	}
	<-ch
	if f.w != 612 || f.h != 792 {
		t.Errorf("default paper = %vx%v", f.w, f.h)
	}
	if _, err := b.Submit(ctx(t), writeFile(t, "a.pdf", "x"), "n", Options{Paper: "A5"}); err != nil {
		t.Fatal(err)
	}
	<-ch
	if f.w != 419.53 || f.h != 595.28 {
		t.Errorf("A5 = %vx%v", f.w, f.h)
	}
}

func TestIPPPaperSize(t *testing.T) {
	b, srv := newTestIPP(t, nil)
	srv.SetAttr("document-format-supported", "image/pwg-raster")
	f := &fakeConv{body: "RaS2"}
	b.convert, b.tmp = f.conv, t.TempDir()
	if _, err := b.Submit(ctx(t), writeFile(t, "in.pdf", "%PDF"), "d", Options{}); err != nil {
		t.Fatal(err)
	}
	c, _ := b.Caps(ctx(t))
	if len(c.Papers) == 0 {
		t.Fatal("fake printer has no papers")
	}
	wantW, wantH, _ := PaperSize(c.Papers[0])
	if f.w != wantW || f.h != wantH {
		t.Errorf("paper %vx%v, want %vx%v (%s)", f.w, f.h, wantW, wantH, c.Papers[0])
	}
}

func TestNewUsesTmp(t *testing.T) {
	dir := t.TempDir()
	b, _ := New(config.Config{Backend: "raw", PrinterURI: "socket://127.0.0.1:9100", RawLanguage: "ps"}, dir)
	if b.(*rawBackend).tmp != dir {
		t.Error("raw tmp")
	}
	b, _ = New(config.Config{Backend: "ipp", PrinterURI: "ipp://127.0.0.1/x"}, dir)
	if b.(*ippBackend).tmp != dir {
		t.Error("ipp tmp")
	}
}

func probeBackend(t *testing.T, probe string) *commandBackend {
	t.Helper()
	b, err := newCommand(config.Config{Backend: "command", Command: []string{os.Args[0]}, PrinterProbe: probe})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCommandProbe(t *testing.T) {
	addr, _ := listen(t)
	if st, _ := probeBackend(t, addr).Status(ctx(t)); st.State != StateReady {
		t.Errorf("listening probe: %s", st.State)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	b := probeBackend(t, dead)
	if st, _ := b.Status(ctx(t)); st.State != StateUnreachable {
		t.Errorf("dead probe: %s", st.State)
	}
	if st, _ := b.Status(ctx(t)); st.State != StateUnreachable {
		t.Errorf("cached: %s", st.State)
	}
	// Without a probe the command backend stays "ready".
	if st, _ := probeBackend(t, "").Status(ctx(t)); st.State != StateReady {
		t.Errorf("no probe: %s", st.State)
	}
}

func TestCommandProbePrintingWhileSubmitting(t *testing.T) {
	b, _ := helperBackend(t, "x")
	b.probe = newProbe("127.0.0.1:1")
	t.Setenv("HELPER_SLEEP", "1")
	b.timeout = 2 * time.Second
	done := make(chan struct{})
	go func() { defer close(done); _, _ = b.Submit(context.Background(), "f", "n", Options{}) }()
	deadline := time.Now().Add(5 * time.Second)
	for b.probe.busy.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st, _ := b.Status(ctx(t)); st.State != StatePrinting {
		t.Errorf("state while submitting = %s", st.State)
	}
	<-done
	if st, _ := b.Status(ctx(t)); st.State != StateUnreachable {
		t.Errorf("state after = %s", st.State)
	}
}

func TestCommandDefaultPaperBeforeValidate(t *testing.T) {
	// The default paper is validated too: an unsafe declared paper is refused.
	b, _ := helperBackend(t, "{paper}")
	b.cfg.Declared.Papers = []string{"-bad"}
	if _, err := b.Submit(ctx(t), "f", "n", Options{}); err == nil {
		t.Error("unsafe default paper accepted")
	}
	b.cfg.Declared.Papers = []string{"A4"}
	if _, err := b.Submit(ctx(t), "f", "n", Options{}); err != nil {
		t.Error(err)
	}
}

func TestStateLogOncePerChange(t *testing.T) {
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	var s stateLog
	s.note(nil)
	s.note(errors.New("dial tcp: refused"))
	s.note(errors.New("dial tcp: refused"))
	s.note(nil)
	s.note(nil)
	out := buf.String()
	if strings.Count(out, "printer unreachable: dial tcp: refused") != 1 || strings.Count(out, "printer reachable again") != 1 {
		t.Errorf("log = %q", out)
	}
}
