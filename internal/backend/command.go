package backend

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kipavy/printer-web/internal/config"
)

const (
	stderrTail = 500
	waitDelay  = 5 * time.Second
)

// safeValue limits values that reach the command line unquoted.
var safeValue = regexp.MustCompile(`^[A-Za-z0-9._+x-]*$`)

// commandBackend runs an external program (argv, never a shell).
type commandBackend struct {
	cfg     config.Config
	timeout time.Duration
}

func newCommand(cfg config.Config) (*commandBackend, error) {
	for _, t := range cfg.Command {
		if t != "{mono_flag}" && t != "{landscape_flag}" &&
			(strings.Contains(t, "{mono_flag}") || strings.Contains(t, "{landscape_flag}")) {
			return nil, fmt.Errorf("PRINTER_COMMAND: %q must be a whole element, not part of %q", "{mono_flag}/{landscape_flag}", t)
		}
	}
	return &commandBackend{cfg: cfg, timeout: submitTimeout}, nil
}

func (b *commandBackend) Caps(context.Context) (Caps, error) {
	return declaredCaps(b.cfg), nil
}

func (b *commandBackend) Status(context.Context) (Status, error) {
	if len(b.cfg.Command) == 0 {
		return Status{State: StateUnreachable}, nil
	}
	if _, err := exec.LookPath(b.cfg.Command[0]); err != nil {
		return Status{State: StateUnreachable}, nil
	}
	return Status{State: StateReady}, nil
}

// validate rejects option values that could be read as command-line flags.
func validate(o Options) error {
	for k, v := range map[string]string{"paper": o.Paper, "tray": o.Tray, "quality": o.Quality} {
		if !safeValue.MatchString(v) || strings.HasPrefix(v, "-") {
			return fmt.Errorf("invalid %s %q", k, v)
		}
	}
	switch o.Sides {
	case "", "one", "long", "short":
	default:
		return fmt.Errorf("invalid sides %q", o.Sides)
	}
	switch o.Orientation {
	case "", "auto", "portrait", "landscape":
	default:
		return fmt.Errorf("invalid orientation %q", o.Orientation)
	}
	return nil
}

// buildArgv substitutes the tokens. Values are never re-expanded or split.
// Elements that end up empty are dropped (except the program itself).
func buildArgv(tmpl []string, pdf, name string, o Options) []string {
	if abs, err := filepath.Abs(pdf); err == nil {
		pdf = abs
	}
	name = strings.TrimLeft(name, "-")
	duplex := "none"
	if o.Sides == "long" || o.Sides == "short" {
		duplex = o.Sides
	}
	color := "mono"
	if o.Color {
		color = "color"
	}
	orient := o.Orientation
	if orient == "" {
		orient = "auto"
	}
	r := strings.NewReplacer(
		"{file}", pdf, "{name}", name, "{copies}", strconv.Itoa(clampCopies(o.Copies)),
		"{duplex}", duplex, "{color}", color, "{paper}", o.Paper,
		"{orientation}", orient, "{quality}", o.Quality, "{tray}", o.Tray,
	)
	out := make([]string, 0, len(tmpl))
	for i, t := range tmpl {
		switch t {
		case "{mono_flag}":
			if !o.Color {
				out = append(out, "--mono")
			}
		case "{landscape_flag}":
			if o.Orientation == "landscape" {
				out = append(out, "--landscape")
			}
		default:
			v := r.Replace(t)
			if v == "" && i > 0 {
				continue
			}
			out = append(out, v)
		}
	}
	return out
}

// tailWriter keeps only the last n bytes written to it.
type tailWriter struct {
	n   int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.n {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.n:]...)
	}
	return len(p), nil
}

func (b *commandBackend) Submit(ctx context.Context, pdf, name string, o Options) (Handle, error) {
	if err := validate(o); err != nil {
		return Handle{}, err
	}
	o = withDefaultPaper(o, b.cfg.Declared.Papers)
	argv := buildArgv(b.cfg.Command, pdf, name, o)
	if len(argv) == 0 || argv[0] == "" {
		return Handle{}, errors.New("empty print command")
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = waitDelay
	stderr := &tailWriter{n: stderrTail}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return Handle{}, fmt.Errorf("print command failed: %w: %s", err, strings.TrimSpace(string(stderr.buf)))
	}
	return Handle{}, nil
}

func (b *commandBackend) Track(context.Context, Handle) (JobState, error) { return JobSent, nil }

func (b *commandBackend) Cancel(context.Context, Handle) error { return ErrNotSupported }

// withDefaultPaper fills an empty paper with the first declared one, so a
// {paper} token never expands to nothing for API clients that omit it.
func withDefaultPaper(o Options, papers []string) Options {
	if o.Paper == "" && len(papers) > 0 {
		o.Paper = papers[0]
	}
	return o
}
