package backend

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/docs"
)

const (
	dialTimeout   = 10 * time.Second
	statusTimeout = 3 * time.Second
)

// probe checks that a TCP address accepts connections (cached) and reports
// "printing" while a job is being sent: such printers take one connection
// at a time.
type probe struct {
	addr  string
	cache *cache
	busy  atomic.Int32 // Submit calls in flight
	state stateLog
}

func newProbe(addr string) *probe { return &probe{addr: addr, cache: newCache(cacheTTL)} }

func (p *probe) dial(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", p.addr)
}

func (p *probe) status(ctx context.Context) (Status, error) {
	if p.busy.Load() > 0 {
		return Status{State: StatePrinting}, nil
	}
	_, err := p.cache.do(ctx, "status", func() (any, error) {
		c, err := p.dial(ctx, statusTimeout)
		if err != nil {
			return nil, err
		}
		return nil, c.Close()
	})
	if err != nil {
		if ctx.Err() != nil {
			return Status{}, ctx.Err()
		}
		p.state.note(err)
		return Status{State: StateUnreachable}, nil
	}
	p.state.note(nil)
	return Status{State: StateReady}, nil
}

// rawBackend streams PostScript or PCL to a TCP socket (port 9100).
type rawBackend struct {
	*probe
	cfg     config.Config
	convert converter
	tmp     string
}

func newRaw(cfg config.Config) *rawBackend {
	addr := cfg.PrinterURI
	if u, err := url.Parse(cfg.PrinterURI); err == nil && u.Host != "" {
		addr = u.Host
	}
	return &rawBackend{probe: newProbe(addr), cfg: cfg, convert: docs.Convert, tmp: os.TempDir()}
}

// rawDuplexOK reports whether duplex can be requested in that language.
func rawDuplexOK(lang string) bool { return lang == "ps" || lang == "pcl" }

func (b *rawBackend) Caps(context.Context) (Caps, error) {
	c := declaredCaps(b.cfg)
	c.Duplex = c.Duplex && rawDuplexOK(b.cfg.RawLanguage)
	return c, nil
}

func declaredCaps(cfg config.Config) Caps {
	return Caps{
		Color: cfg.Declared.Color, Duplex: cfg.Declared.Duplex,
		Papers: append([]string(nil), cfg.Declared.Papers...), Trays: append([]string(nil), cfg.Declared.Trays...),
		Model: cfg.Model, PPM: cfg.PPM, DPI: cfg.DPI,
	}
}

// Status reports "printing" while a job is being sent, otherwise checks that
// the socket accepts connections.
func (b *rawBackend) Status(ctx context.Context) (Status, error) { return b.status(ctx) }

// duplex returns the PostScript header or the PCL-XL Ghostscript flags that
// select one- or two-sided printing. Nothing when duplex is not declared.
func (b *rawBackend) duplex(sides string) (psHeader []byte, gsFlags []string) {
	if !b.cfg.Declared.Duplex || !rawDuplexOK(b.cfg.RawLanguage) {
		return nil, nil
	}
	on := sides == "long" || sides == "short"
	tumble := sides == "short"
	if b.cfg.RawLanguage == "pcl" {
		if !on {
			return nil, []string{"-dDuplex=false"}
		}
		return nil, []string{"-dDuplex=true", "-dTumble=" + strconv.FormatBool(tumble)}
	}
	if !on {
		return []byte("%!PS\n<< /Duplex false >> setpagedevice\n"), nil
	}
	return []byte(fmt.Sprintf("%%!PS\n<< /Duplex true /Tumble %t >> setpagedevice\n", tumble)), nil
}

func (b *rawBackend) Submit(ctx context.Context, pdf, name string, o Options) (Handle, error) {
	b.busy.Add(1)
	defer b.busy.Add(-1)
	format := "application/postscript"
	if b.cfg.RawLanguage == "pcl" {
		format = "application/vnd.hp-pcl"
	}
	header, flags := b.duplex(o.Sides)
	paper := sheetFor(o.Paper, b.cfg.Declared.Papers)
	ctx, cancel := context.WithTimeout(ctx, submitTimeout)
	defer cancel()
	err := convertAndRun(ctx, b.convert, b.tmp, pdf, format, o.Color, paper, flags, func(path string, _ int64) error {
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
		fail := func(err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for i := 0; i < clampCopies(o.Copies); i++ {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if _, err := conn.Write(header); err != nil {
				return fail(err)
			}
			if _, err := io.Copy(conn, f); err != nil {
				return fail(err)
			}
		}
		return nil
	})
	return Handle{}, err
}

func (b *rawBackend) Track(context.Context, Handle) (JobState, error) { return JobSent, nil }

func (b *rawBackend) Cancel(context.Context, Handle) error { return ErrNotSupported }
