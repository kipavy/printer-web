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

// pwgPapers maps friendly paper names to PWG media names.
var pwgPapers = map[string]string{
	"A3":     "iso_a3_297x420mm",
	"A4":     "iso_a4_210x297mm",
	"A5":     "iso_a5_148x210mm",
	"A6":     "iso_a6_105x148mm",
	"B5":     "iso_b5_176x250mm",
	"Letter": "na_letter_8.5x11in",
	"Legal":  "na_legal_8.5x14in",
}

func (b *ippBackend) Caps(ctx context.Context) (Caps, error) {
	if v, ok := b.cache.get("caps"); ok {
		return v.(Caps), nil
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	a, err := b.client.GetPrinterAttributes(ctx, "color-supported", "sides-supported", "media-supported",
		"media-source-supported", "document-format-supported", "print-quality-supported",
		"printer-make-and-model", "pages-per-minute", "pages-per-minute-color", "printer-resolution-supported")
	if err != nil {
		return Caps{}, err
	}
	c := Caps{CanTrack: true, CanCancel: true, Model: b.cfg.Model}
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
	c.Quality = len(a["print-quality-supported"]) > 0
	for _, m := range strs(a["media-supported"]) {
		for name, pwg := range pwgPapers {
			if m == pwg && !slices.Contains(c.Papers, name) {
				c.Papers = append(c.Papers, name)
			}
		}
	}
	slices.Sort(c.Papers)
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
	b.cache.put("caps", c)
	return c, nil
}

func (b *ippBackend) Status(ctx context.Context) (Status, error) {
	if v, ok := b.cache.get("status"); ok {
		return v.(Status), nil
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	a, err := b.client.GetPrinterAttributes(ctx, "printer-state", "printer-state-reasons",
		"marker-names", "marker-colors", "marker-levels")
	if err != nil {
		return Status{State: StateUnreachable}, nil
	}
	st := Status{State: StateReady}
	switch intOr(a["printer-state"]) {
	case 4:
		st.State = StatePrinting
	case 5:
		st.State = StateStopped
	}
	for _, r := range strs(a["printer-state-reasons"]) {
		for _, suf := range []string{"-warning", "-error", "-report"} {
			r = strings.TrimSuffix(r, suf)
		}
		if r != "none" && r != "" && !slices.Contains(st.Reasons, r) {
			st.Reasons = append(st.Reasons, r)
		}
	}
	names, colors, levels := strs(a["marker-names"]), strs(a["marker-colors"]), a["marker-levels"]
	for i, n := range names {
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
	b.cache.put("status", st)
	return st, nil
}

// pickFormat chooses the best supported document format.
func pickFormat(supported []string) (string, error) {
	if len(supported) == 0 {
		return "application/pdf", nil
	}
	for _, f := range formatOrder {
		if slices.Contains(supported, f) {
			return f, nil
		}
	}
	return "", errors.New("printer accepts none of PDF, PWG raster, URF or PostScript")
}

func (b *ippBackend) Submit(ctx context.Context, pdf, name string, o Options) (Handle, error) {
	c, err := b.Caps(ctx)
	if err != nil {
		return Handle{}, err
	}
	format, err := pickFormat(c.Formats)
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
		id, err = b.client.PrintJob(ctx, f, size, format, name, jobAttrs(o))
		return err
	})
	if err != nil {
		return Handle{}, err
	}
	return Handle{ID: id}, nil
}

// jobAttrs maps Options to IPP job template attributes.
func jobAttrs(o Options) []ipp.Attr {
	a := []ipp.Attr{{Name: "copies", Tag: ipp.TagInteger, Values: []any{max(o.Copies, 1)}}}
	kw := func(name, v string) {
		a = append(a, ipp.Attr{Name: name, Tag: ipp.TagKeyword, Values: []any{v}})
	}
	switch o.Sides {
	case "one":
		kw("sides", "one-sided")
	case "long":
		kw("sides", "two-sided-long-edge")
	case "short":
		kw("sides", "two-sided-short-edge")
	}
	if o.Color {
		kw("print-color-mode", "color")
	} else {
		kw("print-color-mode", "monochrome")
	}
	if o.Paper != "" {
		if pwg, ok := pwgPapers[o.Paper]; ok {
			kw("media", pwg)
		} else {
			kw("media", o.Paper)
		}
	}
	switch o.Orientation {
	case "portrait":
		a = append(a, ipp.Attr{Name: "orientation-requested", Tag: ipp.TagEnum, Values: []any{3}})
	case "landscape":
		a = append(a, ipp.Attr{Name: "orientation-requested", Tag: ipp.TagEnum, Values: []any{4}})
	}
	if q := map[string]int{"fast": 3, "standard": 4, "fine": 5}[o.Quality]; q != 0 {
		a = append(a, ipp.Attr{Name: "print-quality", Tag: ipp.TagEnum, Values: []any{q}})
	}
	if o.Tray != "" {
		kw("media-source", o.Tray)
	}
	return a
}

func (b *ippBackend) Track(ctx context.Context, h Handle) (JobState, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	a, err := b.client.GetJobAttributes(ctx, h.ID)
	if err != nil {
		return "", err
	}
	switch s := intOr(a["job-state"]); s {
	case ipp.JobProcessing, ipp.JobProcessingStopped:
		return JobPrinting, nil
	case ipp.JobCompleted:
		return JobDone, nil
	case ipp.JobCanceled:
		return JobCanceled, nil
	case ipp.JobAborted:
		return JobFailed, nil
	case ipp.JobPending, ipp.JobPendingHeld:
		return JobSent, nil
	default:
		return "", fmt.Errorf("unknown job-state %d", s)
	}
}

func (b *ippBackend) Cancel(ctx context.Context, h Handle) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return b.client.CancelJob(ctx, h.ID)
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
