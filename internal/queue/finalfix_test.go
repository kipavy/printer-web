package queue

import (
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kipavy/printer-web/internal/backend"
)

func TestResolveOrientation(t *testing.T) {
	for _, c := range []struct{ in, doc, want string }{
		{"auto", "landscape", "landscape"},
		{"", "landscape", "landscape"},
		{"auto", "portrait", "portrait"},
		{"auto", "", "auto"},
		{"", "", ""},
		{"portrait", "landscape", "portrait"},
		{"landscape", "portrait", "landscape"},
	} {
		if got := resolveOrientation(backend.Options{Orientation: c.in}, c.doc).Orientation; got != c.want {
			t.Errorf("%q + %q = %q, want %q", c.in, c.doc, got, c.want)
		}
	}
}

func TestWorkerSubstitutesAutoOrientation(t *testing.T) {
	e := newEnv(t)
	e.b.caps.CanTrack = false
	submit := func(key, orient, docOrient string) {
		f, err := os.CreateTemp(e.tmp, "up-*.pdf")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		dr := e.d.Add(Draft{Visitor: "v", Name: key, PDF: f.Name(), Pages: 1, Orientation: docOrient})
		if _, err := e.q.Submit(Request{Upload: dr.ID, Key: key, Visitor: "v", Opts: backend.Options{Copies: 1, Orientation: orient}}); err != nil {
			t.Fatal(err)
		}
	}
	submit("a", "auto", "landscape")
	submit("b", "", "landscape")
	submit("c", "portrait", "landscape")
	submit("d", "auto", "")
	e.run(t)
	waitFor(t, "4 submits", func() bool { return len(e.b.submitted()) == 4 })
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	got := []string{}
	for _, o := range e.b.opts {
		got = append(got, o.Orientation)
	}
	if strings.Join(got, ",") != "landscape,landscape,portrait,auto" {
		t.Errorf("orientations = %v", got)
	}
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestWorkerLogsFailuresWithoutNames(t *testing.T) {
	buf := &syncBuf{}
	old := log.Writer()
	log.SetOutput(buf)
	defer log.SetOutput(old)

	e := newEnv(t)
	e.b.set(func(f *fakeBackend) {
		f.submitErr = errors.New("print command failed: exit status 3: printer said no")
	})
	a, _ := e.submit(t, "v", "secret-name.pdf", "ka")
	e.run(t)
	e.finished(t, a.ID)
	out := buf.String()
	if !strings.Contains(out, "job "+a.ID) || !strings.Contains(out, "printer_error") || !strings.Contains(out, "printer said no") {
		t.Errorf("log = %q", out)
	}
	if strings.Contains(out, "secret-name") {
		t.Errorf("log leaks the file name: %q", out)
	}
}

func TestWorkerLogsUnreachable(t *testing.T) {
	buf := &syncBuf{}
	old := log.Writer()
	log.SetOutput(buf)
	defer log.SetOutput(old)

	e := newEnv(t)
	e.b.state = backend.StateUnreachable
	a, _ := e.submit(t, "v", "a", "ka")
	e.run(t)
	if j := e.finished(t, a.ID); j.Error != "printer_unreachable" {
		t.Fatalf("%+v", j)
	}
	if out := buf.String(); !strings.Contains(out, "job "+a.ID+": failed (printer_unreachable)") {
		t.Errorf("log = %q", out)
	}
}
