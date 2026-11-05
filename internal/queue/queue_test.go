package queue

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kipavy/printer-web/internal/backend"
)

type fakeBackend struct {
	mu        sync.Mutex
	caps      backend.Caps
	state     backend.State
	submitErr error
	trackErrs int // transient errors before tracking succeeds
	tracks    []backend.JobState
	calls     []string // names submitted
	canceled  []int
	entered   chan string   // receives the name on Submit entry (if non-nil)
	gate      chan struct{} // Submit waits for it (if non-nil)
	nextID    int
	statusIn  chan struct{} // if set, Status signals it then blocks until ctx is done
}

func newFake() *fakeBackend {
	return &fakeBackend{
		caps:  backend.Caps{CanTrack: true, CanCancel: true},
		state: backend.StateReady,
	}
}

func (f *fakeBackend) Caps(context.Context) (backend.Caps, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps, nil
}

func (f *fakeBackend) Status(ctx context.Context) (backend.Status, error) {
	if f.statusIn != nil {
		f.statusIn <- struct{}{}
		<-ctx.Done()
		return backend.Status{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return backend.Status{State: f.state}, nil
}

func (f *fakeBackend) Submit(ctx context.Context, pdf, name string, o backend.Options) (backend.Handle, error) {
	if f.entered != nil {
		f.entered <- name
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return backend.Handle{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return backend.Handle{}, f.submitErr
	}
	f.calls = append(f.calls, name)
	f.nextID++
	return backend.Handle{ID: f.nextID}, nil
}

func (f *fakeBackend) Track(context.Context, backend.Handle) (backend.JobState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trackErrs > 0 {
		f.trackErrs--
		return "", errors.New("transient")
	}
	if len(f.tracks) == 0 {
		return backend.JobPrinting, nil
	}
	s := f.tracks[0]
	if len(f.tracks) > 1 {
		f.tracks = f.tracks[1:]
	}
	return s, nil
}

func (f *fakeBackend) Cancel(_ context.Context, h backend.Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, h.ID)
	return nil
}

func (f *fakeBackend) submitted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeBackend) set(fn func(f *fakeBackend)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type env struct {
	q   *Queue
	d   *Drafts
	b   *fakeBackend
	c   *clock
	tmp string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	b := newFake()
	c := &clock{t: time.Unix(1000, 0)}
	tmp := t.TempDir()
	d := NewDrafts(tmp, 15*time.Minute, c.now)
	q := NewQueue(b, d, tmp, c.now)
	q.poll = time.Millisecond
	q.prepare = func(_ context.Context, pdf string, pages []int, tmp string) (string, error) {
		f, err := os.CreateTemp(tmp, "prep-*.pdf")
		if err != nil {
			return "", err
		}
		f.Close()
		return f.Name(), nil
	}
	return &env{q: q, d: d, b: b, c: c, tmp: tmp}
}

func (e *env) upload(t *testing.T, visitor, name string, pages int) Draft {
	t.Helper()
	f, err := os.CreateTemp(e.tmp, "up-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	return e.d.Add(Draft{Visitor: visitor, Name: name, PDF: f.Name(), Pages: pages})
}

func (e *env) submit(t *testing.T, visitor, name string, key string) (Job, Draft) {
	t.Helper()
	dr := e.upload(t, visitor, name, 4)
	j, err := e.q.Submit(Request{Upload: dr.ID, Key: key, Visitor: visitor, Opts: backend.Options{Copies: 1, Sides: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	return j, dr
}

func (e *env) run(t *testing.T) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.q.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel, done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (e *env) finished(t *testing.T, id string) Job {
	t.Helper()
	var got Job
	waitFor(t, "job "+id+" to finish", func() bool {
		for _, v := range e.q.History("x", true) {
			if v.ID == id {
				got = v.Job
				return true
			}
		}
		return false
	})
	return got
}

func (e *env) waitPrinting(t *testing.T, visitor string) {
	waitFor(t, "printing", func() bool {
		l := e.q.List(visitor, true)
		return len(l) > 0 && l[0].State == "printing"
	})
}

func TestSheets(t *testing.T) {
	cases := []struct {
		pages, copies int
		sides         string
		want          int
	}{
		{5, 1, "one", 5}, {5, 2, "one", 10}, {5, 1, "long", 3}, {4, 3, "short", 6}, {1, 1, "long", 1}, {0, 1, "one", 0},
	}
	for _, c := range cases {
		if got := Sheets(c.pages, c.copies, c.sides); got != c.want {
			t.Errorf("Sheets(%d,%d,%s)=%d want %d", c.pages, c.copies, c.sides, got, c.want)
		}
	}
}

func TestSubmitIdempotent(t *testing.T) {
	e := newEnv(t)
	dr := e.upload(t, "v1", "a.pdf", 4)
	r := Request{Upload: dr.ID, Key: "k1", Visitor: "v1", Pages: "1-3", Opts: backend.Options{Copies: 500, Sides: "long"}}
	j1, err := e.q.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	if j1.Pages != 3 || j1.Sheets != 2*99 || j1.State != "queued" || j1.Position != 1 || j1.Name != "a.pdf" {
		t.Fatalf("job %+v", j1)
	}
	j2, err := e.q.Submit(r)
	if err != nil || j2.ID != j1.ID {
		t.Fatalf("same key: %v %v", j2, err)
	}
	r.Key = "k2"
	if _, err := e.q.Submit(r); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new key same upload: %v", err)
	}
	if n := len(e.q.List("v1", true)); n != 1 {
		t.Fatalf("%d jobs", n)
	}
}

func TestSubmitValidation(t *testing.T) {
	e := newEnv(t)
	dr := e.upload(t, "v1", "a.pdf", 4)
	if _, err := e.q.Submit(Request{Upload: dr.ID, Key: "k", Visitor: "v2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other visitor: %v", err)
	}
	if _, err := e.q.Submit(Request{Upload: dr.ID, Key: "k", Visitor: "v1", Pages: "9"}); !errors.Is(err, ErrBadPages) {
		t.Fatalf("bad pages: %v", err)
	}
	// A rejected request must not consume the draft.
	if _, err := e.d.Get(dr.ID, "v1"); err != nil {
		t.Fatalf("draft consumed: %v", err)
	}
	if _, err := e.q.Submit(Request{Upload: "zzz", Key: "k", Visitor: "v1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	j, err := e.q.Submit(Request{Upload: dr.ID, Key: "k", Visitor: "v1"})
	if err != nil || j.Sheets != 4 {
		t.Fatalf("%+v %v", j, err) // copies 0 clamps to 1
	}
}

func TestSubmitExpiredDraft(t *testing.T) {
	e := newEnv(t)
	dr := e.upload(t, "v1", "a.pdf", 2)
	e.c.t = e.c.t.Add(time.Hour)
	if _, err := e.q.Submit(Request{Upload: dr.ID, Key: "k", Visitor: "v1"}); !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
}

func TestSerialOrderAndPositions(t *testing.T) {
	e := newEnv(t)
	e.b.entered = make(chan string, 3)
	e.b.gate = make(chan struct{})
	e.b.tracks = []backend.JobState{backend.JobDone}
	j1, _ := e.submit(t, "v1", "one", "k1")
	j2, _ := e.submit(t, "v1", "two", "k2")
	j3, _ := e.submit(t, "v2", "three", "k3")
	if j1.Position != 1 || j2.Position != 2 || j3.Position != 3 {
		t.Fatalf("positions %d %d %d", j1.Position, j2.Position, j3.Position)
	}
	e.run(t)
	if n := <-e.b.entered; n != "one" {
		t.Fatalf("first %s", n)
	}
	list := e.q.List("v1", true)
	if len(list) != 3 || list[0].State != "sending" || list[1].Position != 2 || list[2].Position != 3 {
		t.Fatalf("list %+v", list)
	}
	select {
	case n := <-e.b.entered:
		t.Fatalf("%s submitted in parallel", n)
	case <-time.After(20 * time.Millisecond):
	}
	close(e.b.gate)
	for _, id := range []string{j1.ID, j2.ID, j3.ID} {
		if j := e.finished(t, id); j.State != "done" {
			t.Fatalf("%s: %+v", id, j)
		}
	}
	got := e.b.submitted()
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("order %v", got)
	}
	if len(e.q.List("v1", true)) != 0 {
		t.Fatal("active list should be empty")
	}
}

func TestQueueVisibilityAndCancel(t *testing.T) {
	e := newEnv(t)
	e.b.entered = make(chan string, 4)
	e.b.gate = make(chan struct{})
	e.b.tracks = []backend.JobState{backend.JobPrinting}
	a, _ := e.submit(t, "alice", "alice.pdf", "ka")
	b, _ := e.submit(t, "bob", "bob.pdf", "kb")
	c, _ := e.submit(t, "alice", "alice2.pdf", "kc")

	if l := e.q.List("alice", false); len(l) != 3 {
		t.Fatalf("want 3 active jobs, got %d", len(l))
	}
	for _, v := range e.q.List("alice", false) {
		switch v.ID {
		case a.ID, c.ID:
			if !v.Mine || v.Name == "" {
				t.Fatalf("own job %+v", v)
			}
		case b.ID:
			if v.Mine || v.Name != "" {
				t.Fatalf("other job %+v", v)
			}
		}
	}
	for _, v := range e.q.List("alice", true) {
		if v.Name == "" {
			t.Fatalf("showNames: %+v", v)
		}
	}

	if err := e.q.Cancel(b.ID, "alice"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other's job: %v", err)
	}
	if err := e.q.Cancel("nope", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	e.q.mu.Lock()
	cfile := e.q.jobs[c.ID].pdf
	e.q.mu.Unlock()
	if err := e.q.Cancel(c.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if exists(cfile) {
		t.Fatal("canceled job file kept")
	}
	if len(e.q.List("alice", true)) != 2 {
		t.Fatal("job not removed")
	}
	if _, err := e.q.Submit(Request{Upload: "x", Key: "kc", Visitor: "alice"}); err == nil {
		t.Fatal("canceled key should not resolve to a job")
	}

	e.run(t)
	<-e.b.entered // alice.pdf is in Submit
	close(e.b.gate)
	e.waitPrinting(t, "alice")
	if err := e.q.Cancel(a.ID, "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	if err := e.q.Cancel(a.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if j := e.finished(t, a.ID); j.State != "canceled" {
		t.Fatalf("%+v", j)
	}
	e.b.mu.Lock()
	n := len(e.b.canceled)
	e.b.mu.Unlock()
	if n != 1 {
		t.Fatalf("backend cancel calls %d", n)
	}
	// history: bob sees alice's job without its name, alice sees it
	if h := e.q.History("bob", false); len(h) < 1 || h[0].ID != a.ID || h[0].Name != "" || h[0].Mine {
		t.Fatalf("history masking: %+v", h)
	}
	if h := e.q.History("alice", false); len(h) < 1 || h[0].Name != "alice.pdf" || !h[0].Mine {
		t.Fatalf("own history: %+v", h)
	}
}

func TestCancelPrintingNeedsCapability(t *testing.T) {
	e := newEnv(t)
	e.b.caps.CanCancel = false
	e.b.tracks = []backend.JobState{backend.JobPrinting}
	a, _ := e.submit(t, "alice", "a.pdf", "ka")
	e.run(t)
	e.waitPrinting(t, "alice")
	if err := e.q.Cancel(a.ID, "alice"); !errors.Is(err, ErrNotActive) {
		t.Fatalf("got %v", err)
	}
}

func TestWorkerUnreachableBackendFailsJobAndContinues(t *testing.T) {
	e := newEnv(t)
	e.b.state = backend.StateUnreachable
	e.b.tracks = []backend.JobState{backend.JobDone}
	a, _ := e.submit(t, "v1", "a.pdf", "ka")
	b, _ := e.submit(t, "v1", "b.pdf", "kb")
	e.run(t)
	ja := e.finished(t, a.ID)
	if ja.State != "failed" || ja.Error != "printer_unreachable" {
		t.Fatalf("%+v", ja)
	}
	e.finished(t, b.ID)
	if len(e.b.submitted()) != 0 {
		t.Fatal("must not submit to an unreachable printer")
	}
	e.b.set(func(f *fakeBackend) { f.state = backend.StateReady })
	c, _ := e.submit(t, "v1", "c.pdf", "kc")
	if j := e.finished(t, c.ID); j.State != "done" {
		t.Fatalf("%+v", j)
	}
}

func TestWorkerErrorKeys(t *testing.T) {
	e := newEnv(t)
	e.b.submitErr = &net.OpError{Op: "dial", Err: errors.New("refused")}
	a, _ := e.submit(t, "v", "a", "ka")
	e.run(t)
	if j := e.finished(t, a.ID); j.Error != "printer_unreachable" {
		t.Fatalf("%+v", j)
	}
	e.b.set(func(f *fakeBackend) { f.submitErr = errors.New("boom") })
	b, _ := e.submit(t, "v", "b", "kb")
	if j := e.finished(t, b.ID); j.Error != "printer_error" {
		t.Fatalf("%+v", j)
	}
	// prepare is only read by the worker between jobs; the queue is idle here.
	e.q.mu.Lock()
	e.q.prepare = func(context.Context, string, []int, string) (string, error) { return "", errors.New("gs") }
	e.q.mu.Unlock()
	c, _ := e.submit(t, "v", "c", "kc")
	if j := e.finished(t, c.ID); j.Error != "conversion_failed" {
		t.Fatalf("%+v", j)
	}
}

func TestNonTrackingEndsSent(t *testing.T) {
	e := newEnv(t)
	e.b.caps.CanTrack = false
	a, _ := e.submit(t, "v", "a", "ka")
	e.run(t)
	if j := e.finished(t, a.ID); j.State != "sent" || j.Finished.IsZero() {
		t.Fatalf("%+v", j)
	}
}

func TestTrackRetriesTransientAndCapEndsSent(t *testing.T) {
	e := newEnv(t)
	e.b.trackErrs = 3
	e.b.tracks = []backend.JobState{backend.JobPrinting, backend.JobDone}
	a, _ := e.submit(t, "v", "a", "ka")
	e.run(t)
	if j := e.finished(t, a.ID); j.State != "done" {
		t.Fatalf("%+v", j)
	}

	e.q.mu.Lock()
	e.q.trackMax = 20 * time.Millisecond
	e.q.mu.Unlock()
	e.b.set(func(f *fakeBackend) { f.tracks = []backend.JobState{backend.JobPrinting} })
	b, _ := e.submit(t, "v", "b", "kb")
	if j := e.finished(t, b.ID); j.State != "sent" {
		t.Fatalf("%+v", j)
	}
}

func TestTrackFailed(t *testing.T) {
	e := newEnv(t)
	e.b.tracks = []backend.JobState{backend.JobFailed}
	a, _ := e.submit(t, "v", "a", "ka")
	e.run(t)
	if j := e.finished(t, a.ID); j.State != "failed" || j.Error != "printer_error" {
		t.Fatalf("%+v", j)
	}
}

func TestFilesDeletedAfterJob(t *testing.T) {
	e := newEnv(t)
	e.b.tracks = []backend.JobState{backend.JobDone}
	ok, dr1 := e.submit(t, "v", "ok", "k1")
	e.run(t)
	e.finished(t, ok.ID)
	if exists(dr1.PDF) {
		t.Fatal("upload kept after success")
	}
	e.b.set(func(f *fakeBackend) { f.submitErr = errors.New("boom") })
	bad, dr2 := e.submit(t, "v", "bad", "k2")
	e.finished(t, bad.ID)
	if exists(dr2.PDF) {
		t.Fatal("upload kept after failure")
	}
	waitFor(t, "temp dir empty", func() bool {
		entries, _ := os.ReadDir(e.tmp)
		return len(entries) == 0
	})
}

func TestHistoryBoundedAndKeysPruned(t *testing.T) {
	e := newEnv(t)
	e.b.caps.CanTrack = false
	e.run(t)
	var last Job
	for i := 0; i < 25; i++ {
		last, _ = e.submit(t, "v", "n", string(rune('a'+i)))
		e.finished(t, last.ID)
	}
	if n := len(e.q.History("v", true)); n != 20 {
		t.Fatalf("history %d", n)
	}
	e.q.mu.Lock()
	n := len(e.q.keys)
	e.q.mu.Unlock()
	if n != 20 {
		t.Fatalf("keys %d", n)
	}
	if h := e.q.History("v", true); h[0].ID != last.ID {
		t.Fatal("history not newest first")
	}
}

func (e *env) tmpEmpty(t *testing.T) {
	t.Helper()
	waitFor(t, "temp dir empty", func() bool {
		entries, _ := os.ReadDir(e.tmp)
		return len(entries) == 0
	})
}

func TestShutdownDrainsQueuedJobs(t *testing.T) {
	e := newEnv(t)
	e.b.entered = make(chan string, 2)
	e.b.gate = make(chan struct{}) // never released: Submit ends with ctx
	a, _ := e.submit(t, "v", "a", "ka")
	b, drb := e.submit(t, "v", "b", "kb")
	cancel, done := e.run(t)
	<-e.b.entered
	cancel()
	<-done
	// Submit was interrupted: the printer may have the job.
	if j := e.finished(t, a.ID); j.State != "sent" {
		t.Fatalf("in-flight: %+v", j)
	}
	jb := e.finished(t, b.ID)
	if jb.State != "canceled" || jb.Error != "server_stopped" {
		t.Fatalf("queued: %+v", jb)
	}
	if exists(drb.PDF) {
		t.Fatal("queued file kept")
	}
	e.tmpEmpty(t)
}

func TestShutdownDuringStatusCancels(t *testing.T) {
	e := newEnv(t)
	e.b.statusIn = make(chan struct{}, 1)
	a, _ := e.submit(t, "v", "a", "ka")
	cancel, _ := e.run(t)
	<-e.b.statusIn
	cancel()
	if j := e.finished(t, a.ID); j.State != "canceled" || j.Error != "server_stopped" {
		t.Fatalf("%+v", j)
	}
	e.tmpEmpty(t)
}

func TestShutdownDuringPrepareCancels(t *testing.T) {
	e := newEnv(t)
	in := make(chan struct{}, 1)
	e.q.prepare = func(ctx context.Context, _ string, _ []int, _ string) (string, error) {
		in <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	a, _ := e.submit(t, "v", "a", "ka")
	cancel, _ := e.run(t)
	<-in
	cancel()
	if j := e.finished(t, a.ID); j.State != "canceled" || j.Error != "server_stopped" {
		t.Fatalf("%+v", j)
	}
	e.tmpEmpty(t)
}

func TestShutdownWhilePrintingEndsSent(t *testing.T) {
	e := newEnv(t)
	a, _ := e.submit(t, "v", "a", "ka")
	cancel, _ := e.run(t)
	e.waitPrinting(t, "v")
	cancel()
	if j := e.finished(t, a.ID); j.State != "sent" {
		t.Fatalf("%+v", j)
	}
	e.tmpEmpty(t)
}

func TestPrepareErrorDeletesFiles(t *testing.T) {
	e := newEnv(t)
	e.q.prepare = func(context.Context, string, []int, string) (string, error) { return "", errors.New("gs") }
	a, _ := e.submit(t, "v", "a", "ka")
	e.run(t)
	if j := e.finished(t, a.ID); j.Error != "conversion_failed" {
		t.Fatalf("%+v", j)
	}
	e.tmpEmpty(t)
}

func TestCancelPrintingDeletesPrepFile(t *testing.T) {
	e := newEnv(t)
	a, _ := e.submit(t, "v", "a", "ka")
	e.run(t)
	e.waitPrinting(t, "v")
	if err := e.q.Cancel(a.ID, "v"); err != nil {
		t.Fatal(err)
	}
	if j := e.finished(t, a.ID); j.State != "canceled" {
		t.Fatalf("%+v", j)
	}
	e.tmpEmpty(t)
}

func TestSubmitEmptyKey(t *testing.T) {
	e := newEnv(t)
	dr := e.upload(t, "v", "a", 2)
	if _, err := e.q.Submit(Request{Upload: dr.ID, Visitor: "v"}); !errors.Is(err, ErrBadKey) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.d.Get(dr.ID, "v"); err != nil {
		t.Fatal("draft consumed")
	}
}

func TestQueueFull(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < maxActive; i++ {
		e.submit(t, "v", "n", string(rune('A'+i)))
	}
	dr := e.upload(t, "v", "n", 2)
	if _, err := e.q.Submit(Request{Upload: dr.ID, Key: "extra", Visitor: "v"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.d.Get(dr.ID, "v"); err != nil {
		t.Fatal("draft consumed when queue full")
	}
}

func TestConcurrentSameKeySubmit(t *testing.T) {
	e := newEnv(t)
	dr := e.upload(t, "v", "a", 2)
	ids := make(chan string, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := e.q.Submit(Request{Upload: dr.ID, Key: "k", Visitor: "v"})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- j.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("different jobs %s %s", first, id)
		}
	}
	if n := len(e.q.List("v", true)); n != 1 {
		t.Fatalf("%d jobs", n)
	}
}

func TestExpiryAtExactlyTTL(t *testing.T) {
	e := newEnv(t)
	dr := e.upload(t, "v", "a", 2)
	e.c.t = e.c.t.Add(15*time.Minute - time.Nanosecond)
	if _, err := e.d.Get(dr.ID, "v"); err != nil {
		t.Fatal(err)
	}
	e.c.t = e.c.t.Add(time.Nanosecond)
	if _, err := e.d.Get(dr.ID, "v"); !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
}

func TestDraftsPurge(t *testing.T) {
	e := newEnv(t)
	dr := e.upload(t, "v", "a", 2)
	e.d.Purge()
	if exists(dr.PDF) {
		t.Fatal("file kept")
	}
	if _, err := e.d.Get(dr.ID, "v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}
