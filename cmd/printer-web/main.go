// Command printer-web serves a phone-friendly print page for one printer.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/kipavy/printer-web/internal/backend"
	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/queue"
	"github.com/kipavy/printer-web/internal/web"
	webassets "github.com/kipavy/printer-web/web"
)

// shutdownGrace bounds how long in-flight HTTP requests may finish.
const shutdownGrace = 3 * time.Second

func main() { os.Exit(run()) }

// tempMarker marks a directory created by printer-web, so a restart only
// ever empties a directory it made itself.
const tempMarker = ".printer-web"

// prepareTempDir returns base/printer-web-<uid>, emptied and recreated with
// mode 0700. An existing directory is only removed when it is a real
// directory (not a symlink) holding the marker; anything else is an error.
func prepareTempDir(base string) (string, error) {
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("temporary directory %q is not absolute", base)
	}
	dir := filepath.Join(base, fmt.Sprintf("printer-web-%d", os.Getuid()))
	if fi, err := os.Lstat(dir); err == nil {
		if !fi.IsDir() {
			return "", fmt.Errorf("%s exists and is not a directory", dir)
		}
		if _, err := os.Lstat(filepath.Join(dir, tempMarker)); err != nil {
			return "", fmt.Errorf("%s exists but was not created by printer-web; remove it", dir)
		}
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("cannot clean %s: %w", dir, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Mkdir (not MkdirAll) fails if someone else created it in the meantime.
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, tempMarker), nil, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

func run() int {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "printer-web: configuration error:", err)
		return 2
	}
	// A fixed directory, emptied at start: files left by a crash or a kill
	// are deleted on the next start.
	dir, err := prepareTempDir(os.TempDir())
	if err != nil {
		log.Printf("temp dir: %v", err)
		return 1
	}
	defer os.RemoveAll(dir)
	b, err := backend.New(cfg, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "printer-web: configuration error:", err)
		return 2
	}

	drafts := queue.NewDrafts(dir, cfg.DraftTTL, nil)
	q := queue.NewQueue(b, drafts, dir, nil)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           web.New(cfg, b, drafts, q, dir, webassets.FS()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-t.C:
				drafts.Sweep()
			}
		}
	}()
	go func() { defer wg.Done(); q.Run(workerCtx) }()

	sig, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	pub := cfg.PublicURL
	if pub == "" {
		pub = "(from request host)"
	}
	log.Printf("printer-web listening on %s, public URL %s", cfg.ListenAddr, pub)

	code := 0
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
			code = 1
		}
	case <-sig.Done():
		log.Print("shutting down")
	}
	// No new Submit after Shutdown; then let the worker drain. The grace is
	// short so the whole stop (plus at most 5 s for a child process to exit)
	// stays under Docker's default 10 s.
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	_ = srv.Shutdown(sctx)
	cancel()
	stopWorker()
	wg.Wait()
	drafts.Purge()
	_ = os.RemoveAll(dir)
	return code
}
