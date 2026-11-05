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
	"sync"
	"syscall"
	"time"

	"github.com/kipavy/printer-web/internal/backend"
	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/queue"
	"github.com/kipavy/printer-web/internal/web"
	webassets "github.com/kipavy/printer-web/web"
)

func main() { os.Exit(run()) }

func run() int {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "printer-web: configuration error:", err)
		return 2
	}
	b, err := backend.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "printer-web: configuration error:", err)
		return 2
	}
	dir, err := os.MkdirTemp("", "printer-web-")
	if err != nil {
		log.Printf("cannot create temp dir: %v", err)
		return 1
	}
	defer os.RemoveAll(dir)

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
	// No new Submit after Shutdown; then let the worker drain.
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = srv.Shutdown(sctx)
	cancel()
	stopWorker()
	wg.Wait()
	drafts.Purge()
	_ = os.RemoveAll(dir)
	return code
}
