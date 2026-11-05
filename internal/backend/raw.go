package backend

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/docs"
)

const (
	dialTimeout   = 10 * time.Second
	statusTimeout = 3 * time.Second
)

// rawBackend streams PostScript or PCL to a TCP socket (port 9100).
type rawBackend struct {
	cfg     config.Config
	addr    string
	convert converter
	tmp     string
	cache   *cache
	busy    atomic.Int32 // Submit calls in flight
}

func newRaw(cfg config.Config) *rawBackend {
	addr := cfg.PrinterURI
	if u, err := url.Parse(cfg.PrinterURI); err == nil && u.Host != "" {
		addr = u.Host
	}
	return &rawBackend{cfg: cfg, addr: addr, convert: docs.Convert, tmp: os.TempDir(), cache: newCache(cacheTTL)}
}

func (b *rawBackend) dial(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", b.addr)
}

func (b *rawBackend) Caps(context.Context) (Caps, error) {
	return declaredCaps(b.cfg), nil
}

func declaredCaps(cfg config.Config) Caps {
	return Caps{
		Color: cfg.Declared.Color, Duplex: cfg.Declared.Duplex,
		Papers: append([]string(nil), cfg.Declared.Papers...), Trays: append([]string(nil), cfg.Declared.Trays...),
		Model: cfg.Model, PPM: cfg.PPM, DPI: cfg.DPI,
	}
}

// Status reports "printing" while a job is being sent (the printer accepts one
// connection at a time), otherwise checks that the socket accepts connections.
func (b *rawBackend) Status(ctx context.Context) (Status, error) {
	if b.busy.Load() > 0 {
		return Status{State: StatePrinting}, nil
	}
	_, err := b.cache.do(ctx, "status", func() (any, error) {
		c, err := b.dial(ctx, statusTimeout)
		if err != nil {
			return nil, err
		}
		return nil, c.Close()
	})
	if err != nil {
		if ctx.Err() != nil {
			return Status{}, ctx.Err()
		}
		return Status{State: StateUnreachable}, nil
	}
	return Status{State: StateReady}, nil
}

func (b *rawBackend) Submit(ctx context.Context, pdf, name string, o Options) (Handle, error) {
	b.busy.Add(1)
	defer b.busy.Add(-1)
	format := "application/postscript"
	if b.cfg.RawLanguage == "pcl" {
		format = "application/vnd.hp-pcl"
	}
	ctx, cancel := context.WithTimeout(ctx, submitTimeout)
	defer cancel()
	err := convertAndRun(ctx, b.convert, b.tmp, pdf, format, o.Color, func(path string, _ int64) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		conn, err := b.dial(ctx, dialTimeout)
		if err != nil {
			return err
		}
		defer conn.Close()
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		}
		// Unblock a stuck write when ctx is cancelled.
		stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
		defer stop()
		for i := 0; i < clampCopies(o.Copies); i++ {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if _, err := io.Copy(conn, f); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
		return nil
	})
	return Handle{}, err
}

func (b *rawBackend) Track(context.Context, Handle) (JobState, error) { return JobSent, nil }

func (b *rawBackend) Cancel(context.Context, Handle) error { return ErrNotSupported }
