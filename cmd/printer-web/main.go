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

func run() int {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "printer-web: configuration error:", err)
		return 2
	}
	// A fixed directory, emptied at start: files left by a crash or a kill
	// are deleted on the next start.
	dir := filepath.Join(os.TempDir(), "printer-web")
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("cannot clean temp dir %s: %v", dir, err)
		return 1
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("cannot create temp dir %s: %v", dir, err)
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
