package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/docs"
	"github.com/kipavy/printer-web/internal/ipp"
)

const (
	cacheTTL      = 10 * time.Second
	submitTimeout = 10 * time.Minute
	queryTimeout  = 10 * time.Second
)

// ippBackend drives an IPP printer or a CUPS queue (same protocol).
type ippBackend struct {
	cfg     config.Config
	client  *ipp.Client
	convert converter
	tmp     string
	cache   *cache
}

func newIPP(cfg config.Config) *ippBackend {
	return &ippBackend{
		cfg:     cfg,
		client:  ipp.NewClient(cfg.PrinterURI, cfg.TLSInsecure),
		convert: docs.Convert,
		tmp:     os.TempDir(),
		cache:   newCache(cacheTTL),
	}
}

// Document formats in order of preference.
var formatOrder = []string{"application/pdf", "image/pwg-raster", "image/urf", "application/postscript"}

// papers lists the friendly paper names in display order, with the prefix of
// their PWG media names and the canonical PWG name.
var papers = []struct{ name, prefix, pwg string }{
	{"A4", "iso_a4_", "iso_a4_210x297mm"},
	{"Letter", "na_letter_", "na_letter_8.5x11in"},
	{"Legal", "na_legal_", "na_legal_8.5x14in"},
	{"A5", "iso_a5_", "iso_a5_148x210mm"},
	{"A3", "iso_a3_", "iso_a3_297x420mm"},
	{"A6", "iso_a6_", "iso_a6_105x148mm"},
	{"B5", "iso_b5_", "iso_b5_176x250mm"},
}

// friendlyPaper maps a PWG media name to a friendly name ("" if unknown).
func friendlyPaper(media string) string {
	for _, p := range papers {
		if strings.HasPrefix(media, p.prefix) {
			return p.name
		}
	}
	return ""
}

func paperPWG(name string) string {
	for _, p := range papers {
		if p.name == name {
			return p.pwg
		}
	}
	return ""
}

// ippCaps are the capabilities plus the printer's own media names.
type ippCaps struct {
	Caps
	media map[string]string // friendly paper name -> media name to send
}

func (b *ippBackend) Caps(ctx context.Context) (Caps, error) {
	c, err := b.caps(ctx)
	if err != nil {
		return Caps{}, err
	}
	out := c.Caps
	out.Papers, out.Trays, out.Formats = slices.Clone(out.Papers), slices.Clone(out.Trays), slices.Clone(out.Formats)
	return out, nil
}

func (b *ippBackend) caps(ctx context.Context) (ippCaps, error) {
	v, err := b.cache.do(ctx, "caps", func() (any, error) { return b.fetchCaps(ctx) })
	if err != nil {
		return ippCaps{}, err
	}
	return v.(ippCaps), nil
}

func (b *ippBackend) fetchCaps(ctx context.Context) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	a, err := b.client.GetPrinterAttributes(ctx, "color-supported", "sides-supported", "media-supported",
		"media-default", "media-source-supported", "document-format-supported", "print-quality-supported",
		"printer-make-and-model", "pages-per-minute", "pages-per-minute-color", "printer-resolution-supported")
	if err != nil {
		return nil, err
	}
	c := ippCaps{Caps: Caps{CanTrack: true, CanCancel: true, Model: b.cfg.Model}, media: map[string]string{}}
	if v := strs(a["printer-make-and-model"]); len(v) > 0 {
		c.Model = v[0]
	}
	if v := a["color-supported"]; len(v) > 0 {
		c.Color, _ = v[0].(bool)
	}
	for _, s := range strs(a["sides-supported"]) {
		if strings.HasPrefix(s, "two-sided") {
			c.Duplex = true
		}
	}
	c.Quality = len(a["print-quality-supported"]) >= 2
	for _, m := range strs(a["media-supported"]) {
		if f := friendlyPaper(m); f != "" {
			if old, ok := c.media[f]; !ok || (m == paperPWG(f) && old != m) {
				c.media[f] = m
			}
		}
	}
	def := ""
	if v := strs(a["media-default"]); len(v) > 0 {
		def = friendlyPaper(v[0])
	}
	if _, ok := c.media[def]; ok {
		c.Papers = append(c.Papers, def)
	}
	for _, p := range papers {
		if _, ok := c.media[p.name]; ok && p.name != def {
			c.Papers = append(c.Papers, p.name)
		}
	}
	c.Trays = strs(a["media-source-supported"])
	c.Formats = strs(a["document-format-supported"])
	c.PPM = intOr(a["pages-per-minute"])
	c.PPMColor = intOr(a["pages-per-minute-color"])
	for _, v := range a["printer-resolution-supported"] {
		if r, ok := v.(ipp.Resolution); ok {
			dpi := int(max(r.X, r.Y))
			if r.Units == 4 { // dots per cm
				dpi = dpi * 254 / 100
			}
			c.DPI = max(c.DPI, dpi)
		}
	}
	if c.PPM == 0 {
		c.PPM = b.cfg.PPM
	}
	if c.DPI == 0 {
		c.DPI = b.cfg.DPI
	}
	return c, nil
}

func (b *ippBackend) Status(ctx context.Context) (Status, error) {
	v, err := b.cache.do(ctx, "status", func() (any, error) { return b.fetchStatus(ctx) })
	if err != nil {
		if ctx.Err() != nil {
			return Status{}, ctx.Err()
		}
		return Status{State: StateUnreachable}, nil
	}
	st := v.(Status)
	st.Reasons, st.Supplies = slices.Clone(st.Reasons), slices.Clone(st.Supplies)
	return st, nil
}

func (b *ippBackend) fetchStatus(ctx context.Context) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	a, err := b.client.GetPrinterAttributes(ctx, "printer-state", "printer-state-reasons",
		"printer-is-accepting-jobs", "marker-names", "marker-types", "marker-colors", "marker-levels")
	if err != nil {
		return nil, err
	}
	st := Status{State: StateReady}
	switch intOr(a["printer-state"]) {
	case 4:
		st.State = StatePrinting
	case 5:
		st.State = StateStopped
	}
	if v := a["printer-is-accepting-jobs"]; len(v) > 0 {
		if acc, ok := v[0].(bool); ok && !acc {
			st.State = StateStopped
		}
	}
	for _, r := range strs(a["printer-state-reasons"]) {
		for _, suf := range []string{"-warning", "-error", "-report"} {
			r = strings.TrimSuffix(r, suf)
		}
		if r != "none" && r != "" && !slices.Contains(st.Reasons, r) {
			st.Reasons = append(st.Reasons, r)
		}
	}
	names, types, colors, levels := strs(a["marker-names"]), strs(a["marker-types"]), strs(a["marker-colors"]), a["marker-levels"]
	for i, n := range names {
		if i < len(types) && strings.HasPrefix(types[i], "waste") {
			continue
		}
		s := Supply{Name: n, Level: -1}
		if i < len(colors) {
			s.Color = colors[i]
		}
		if i < len(levels) {
			if l, ok := levels[i].(int32); ok && l >= 0 && l <= 100 {
				s.Level = int(l)
			}
		}
		st.Supplies = append(st.Supplies, s)
	}
	return st, nil
}

// pickFormat chooses the best supported document format (case-insensitive).
func pickFormat(supported []string) (format, wire string, err error) {
	if len(supported) == 0 {
		return "application/pdf", "application/pdf", nil
	}
	for _, f := range formatOrder {
		if i := slices.IndexFunc(supported, func(s string) bool { return strings.EqualFold(s, f) }); i >= 0 {
			return f, supported[i], nil
		}
	}
	return "", "", errors.New("printer accepts none of PDF, PWG raster, URF or PostScript")
}

var errPaper = errors.New("paper not supported by the printer")

func (b *ippBackend) Submit(ctx context.Context, pdf, name string, o Options) (Handle, error) {
	c, err := b.caps(ctx)
	if err != nil {
		return Handle{}, err
	}
	format, wire, err := pickFormat(c.Formats)
	if err != nil {
		return Handle{}, err
	}
	attrs, err := jobAttrs(o, c)
	if err != nil {
		return Handle{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, submitTimeout)
	defer cancel()
	var id int
	err = convertAndRun(ctx, b.convert, b.tmp, pdf, format, o.Color, func(path string, size int64) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		id, err = b.client.PrintJob(ctx, f, size, wire, name, attrs)
		return err
	})
	if err != nil {
		return Handle{}, err
	}
	return Handle{ID: id}, nil
}

// jobAttrs maps Options to IPP job template attributes, only those the
// printer advertises.
func jobAttrs(o Options, c ippCaps) ([]ipp.Attr, error) {
	a := []ipp.Attr{{Name: "copies", Tag: ipp.TagInteger, Values: []any{clampCopies(o.Copies)}}}
	kw := func(name, v string) {
		a = append(a, ipp.Attr{Name: name, Tag: ipp.TagKeyword, Values: []any{v}})
	}
	switch {
	case o.Sides == "one":
		kw("sides", "one-sided")
	case o.Sides == "long" && c.Duplex:
		kw("sides", "two-sided-long-edge")
	case o.Sides == "short" && c.Duplex:
		kw("sides", "two-sided-short-edge")
	}
	if o.Color {
		kw("print-color-mode", "color")
	} else {
		kw("print-color-mode", "monochrome")
	}
	if o.Paper != "" {
		m, ok := c.media[o.Paper]
		if !ok {
			return nil, errPaper
		}
		kw("media", m)
	}
	switch o.Orientation {
	case "portrait":
		a = append(a, ipp.Attr{Name: "orientation-requested", Tag: ipp.TagEnum, Values: []any{3}})
	case "landscape":
		a = append(a, ipp.Attr{Name: "orientation-requested", Tag: ipp.TagEnum, Values: []any{4}})
	}
	if q := map[string]int{"fast": 3, "standard": 4, "fine": 5}[o.Quality]; q != 0 && c.Quality {
		a = append(a, ipp.Attr{Name: "print-quality", Tag: ipp.TagEnum, Values: []any{q}})
	}
	if o.Tray != "" && slices.Contains(c.Trays, o.Tray) {
		kw("media-source", o.Tray)
	}
	return a, nil
}

func (b *ippBackend) Track(ctx context.Context, h Handle) (JobState, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	a, err := b.client.GetJobAttributes(ctx, h.ID)
	if err != nil {
		// Printers purge finished jobs: not-found after a successful submit
		// means the job is done.
		var se *ipp.StatusError
		if errors.As(err, &se) && se.Code == ipp.StatusNotFound {
			return JobDone, nil
		}
		return "", err
	}
	switch s := intOr(a["job-state"]); s {
	case 0: // no job-state: the printer already forgot the job
		return JobDone, nil
	case ipp.JobCompleted:
		return JobDone, nil
	case ipp.JobCanceled:
		return JobCanceled, nil
	case ipp.JobAborted:
		return JobFailed, nil
	case ipp.JobPending, ipp.JobPendingHeld:
		return JobSent, nil
	case ipp.JobProcessing, ipp.JobProcessingStopped:
		return JobPrinting, nil
	default:
		return JobPrinting, nil
	}
}

func (b *ippBackend) Cancel(ctx context.Context, h Handle) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if err := b.client.CancelJob(ctx, h.ID); err != nil {
		return fmt.Errorf("cancel job: %w", err)
	}
	return nil
}

func strs(v []any) []string {
	var out []string
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intOr(v []any) int {
	if len(v) > 0 {
		if n, ok := v[0].(int32); ok {
			return int(n)
		}
	}
	return 0
}
