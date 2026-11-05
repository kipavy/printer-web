package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kipavy/printer-web/internal/config"
)

// commandBackend runs an external program (argv, never a shell).
type commandBackend struct{ cfg config.Config }

func newCommand(cfg config.Config) *commandBackend { return &commandBackend{cfg: cfg} }

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

// buildArgv substitutes the tokens. Values are never re-expanded or split.
func buildArgv(tmpl []string, pdf, name string, o Options) []string {
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
		"{file}", pdf, "{name}", name, "{copies}", strconv.Itoa(max(o.Copies, 1)),
		"{duplex}", duplex, "{color}", color, "{paper}", o.Paper,
		"{orientation}", orient, "{quality}", o.Quality, "{tray}", o.Tray,
	)
	out := make([]string, 0, len(tmpl))
	for _, t := range tmpl {
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
			out = append(out, r.Replace(t))
		}
	}
	return out
}

func (b *commandBackend) Submit(ctx context.Context, pdf, name string, o Options) (Handle, error) {
	argv := buildArgv(b.cfg.Command, pdf, name, o)
	if len(argv) == 0 {
		return Handle{}, errors.New("empty print command")
	}
	ctx, cancel := context.WithTimeout(ctx, submitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return Handle{}, fmt.Errorf("print command failed: %w: %s", err, tail(stderr.Bytes(), 500))
	}
	return Handle{}, nil
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}

func (b *commandBackend) Track(context.Context, Handle) (JobState, error) { return JobSent, nil }

func (b *commandBackend) Cancel(context.Context, Handle) error { return ErrNotSupported }
