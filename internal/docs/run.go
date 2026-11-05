package docs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const maxStderr = 512

// run executes argv without a shell; replaced in tests.
var run = func(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > maxStderr {
			msg = msg[:maxStderr] + "..."
		}
		return nil, fmt.Errorf("%s: %w: %s", argv[0], err, msg)
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
	return name, nil
}

func removeQuiet(p string) { _ = os.Remove(p) }
