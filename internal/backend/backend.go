// Package backend sends print jobs to a printer through IPP, CUPS, a raw
// socket or an external command.
package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/docs"
)

// ErrNotSupported is returned by operations a backend cannot do.
var ErrNotSupported = errors.New("backend: operation not supported")

// State is the printer state.
type State string

// Printer states.
const (
	StateReady       State = "ready"
	StatePrinting    State = "printing"
	StateStopped     State = "stopped"
	StateUnreachable State = "unreachable"
)

// Supply is a consumable (toner, ink...). Level is 0-100, -1 if unknown.
type Supply struct {
	Name, Color string
	Level       int
}

// Status is the live printer status.
type Status struct {
	State    State
	Reasons  []string // "media-empty", "media-jam", "toner-low", "door-open"...
	Supplies []Supply
}

// Caps are the printer capabilities.
type Caps struct {
	Color, Duplex, Quality bool
	Papers, Trays          []string
	Formats                []string
	CanTrack, CanCancel    bool
	Model                  string
	PPM, PPMColor, DPI     int
}

// Options are the per-job print options.
type Options struct {
	Copies      int
	Sides       string // one | long | short
	Color       bool
	Paper       string
	Orientation string // auto | portrait | landscape
	Quality     string // fast | standard | fine
	Tray        string
}

// Handle identifies a submitted job (ID 0 when the backend cannot track it).
type Handle struct{ ID int }

// JobState is the progress of a submitted job.
type JobState string

// Job states.
const (
	JobSent     JobState = "sent"
	JobPrinting JobState = "printing"
	JobDone     JobState = "done"
	JobFailed   JobState = "failed"
	JobCanceled JobState = "canceled"
)

// Backend is a way to reach the printer.
type Backend interface {
	Caps(ctx context.Context) (Caps, error)
	Status(ctx context.Context) (Status, error)
	// Submit prints the PDF at path pdf. It never deletes pdf.
	Submit(ctx context.Context, pdf, name string, o Options) (Handle, error)
	// Track reports job progress; non-tracking backends return JobSent.
	Track(ctx context.Context, h Handle) (JobState, error)
	// Cancel cancels a job; ErrNotSupported if the backend cannot.
	Cancel(ctx context.Context, h Handle) error
}

// New builds the backend selected by cfg.Backend.
func New(cfg config.Config) (Backend, error) {
	switch cfg.Backend {
	case "ipp", "cups":
		return newIPP(cfg), nil
	case "raw":
		return newRaw(cfg), nil
	case "command":
		return newCommand(cfg)
	}
	return nil, fmt.Errorf("backend: unknown backend %q", cfg.Backend)
}

// converter renders the PDF into the printer's format and returns a file path.
type converter func(ctx context.Context, pdf, format string, color bool, tmp string) (string, error)

var _ converter = docs.Convert

// convertAndRun converts pdf, calls fn with the result and its exact size,
// then removes the converted file (never the input pdf).
func convertAndRun(ctx context.Context, conv converter, tmp, pdf, format string, color bool, fn func(path string, size int64) error) error {
	out, err := conv(ctx, pdf, format, color, tmp)
	if err != nil {
		return err
	}
	if !samePath(out, pdf) {
		defer removeQuiet(out)
	}
	st, err := os.Stat(out)
	if err != nil {
		return err
	}
	return fn(out, st.Size())
}

func removeQuiet(p string) { _ = os.Remove(p) }

func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && aa == bb
}

// clampCopies bounds the copy count to 1..99.
func clampCopies(n int) int { return min(max(n, 1), 99) }

const failTTL = 3 * time.Second

// cache holds one result per key. Concurrent callers for the same key share
// one fetch; successes live for ttl, failures for failTTL.
type cache struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]*cacheEntry
}

type cacheEntry struct {
	mu  sync.Mutex // held during a fetch
	ok  bool
	at  time.Time
	v   any
	err error
}

func newCache(ttl time.Duration) *cache {
	return &cache{ttl: ttl, now: time.Now, m: map[string]*cacheEntry{}}
}

// do returns the cached result for key or runs fetch. Errors caused by the
// caller's own cancelled ctx are not cached.
func (c *cache) do(ctx context.Context, key string, fetch func() (any, error)) (any, error) {
	c.mu.Lock()
	e := c.m[key]
	if e == nil {
		e = &cacheEntry{}
		c.m[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	ttl := c.ttl
	if e.err != nil {
		ttl = failTTL
	}
	if e.ok && c.now().Sub(e.at) < ttl {
		return e.v, e.err
	}
	v, err := fetch()
	if err != nil && ctx.Err() != nil {
		return v, err
	}
	e.ok, e.at, e.v, e.err = true, c.now(), v, err
	return v, err
}
