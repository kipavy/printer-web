package docs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxTail    = 512
	toolBudget = 2 * time.Minute
)

// tail returns the last n bytes of b as valid UTF-8, trimmed.
func tail(b []byte, n int) string {
	prefix := ""
	if len(b) > n {
		b = b[len(b)-n:]
		prefix = "..."
	}
	return prefix + strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}

// run executes argv without a shell; replaced in tests. Errors carry the tail
// of stderr and stdout (Ghostscript reports errors on stdout).
var run = func(ctx context.Context, argv []string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, toolBudget)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 5 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		base := fmt.Errorf("%s: %w", argv[0], err)
		if cerr := ctx.Err(); cerr != nil {
			base = fmt.Errorf("%s: %w: %v", argv[0], cerr, err)
		}
		msg := ""
		if s := tail(errb.Bytes(), maxTail); s != "" {
			msg += "; stderr: " + s
		}
		if s := tail(out.Bytes(), maxTail); s != "" {
			msg += "; stdout: " + s
		}
		if msg == "" {
			return nil, base
		}
		return nil, fmt.Errorf("%w%s", base, msg)
	}
	return out.Bytes(), nil
}

// gsBase are the flags every Ghostscript call starts with.
func gsBase() []string { return []string{"gs", "-dSAFER", "-dBATCH", "-dNOPAUSE", "-q"} }

func tempPath(tmp, pattern string) (string, error) {
	f, err := os.CreateTemp(tmp, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	return filepath.Abs(name)
}

func removeQuiet(p string) { _ = os.Remove(p) }
