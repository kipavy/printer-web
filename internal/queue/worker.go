package queue

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"time"

	"github.com/kipavy/printer-web/internal/backend"
)

const (
	statusTimeout = 15 * time.Second
	submitTimeout = 10 * time.Minute
	trackTimeout  = 15 * time.Second
)

// Run serves the queue one job at a time until ctx is cancelled.
func (q *Queue) Run(ctx context.Context) {
	defer q.drain()
	for {
		if ctx.Err() != nil {
			return
		}
		j := q.next()
		if j == nil {
			select {
			case <-ctx.Done():
				return
			case <-q.notify:
			}
			continue
		}
		q.process(ctx, j)
	}
}

// next marks the first queued job as preparing and returns it.
func (q *Queue) next() *job {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.order {
		if j.State == "queued" {
			j.State = "preparing"
			return j
		}
	}
	return nil
}

// end deletes the job's files and then moves it to the history.
func (q *Queue) end(j *job, state, errKey string) {
	_ = os.Remove(j.pdf)
	if j.prep != "" {
		_ = os.Remove(j.prep)
	}
	q.mu.Lock()
	q.finish(j, state, errKey)
	q.mu.Unlock()
}

// isNetErr reports a network-level failure. context.DeadlineExceeded also
// satisfies net.Error (Timeout() is true), so a Submit or Status timeout is
// treated as the printer being unreachable.
func isNetErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}

// resolveOrientation replaces "auto" (or empty) with the document's own
// orientation when it is known, so backends that cannot see the page
// geometry (such as a command) get portrait or landscape.
func resolveOrientation(o backend.Options, orient string) backend.Options {
	if (o.Orientation == "" || o.Orientation == "auto") && orient != "" {
		o.Orientation = orient
		o.AutoOrientation = true
	}
	return o
}

// fail ends j with errKey and logs why (never the file name).
func (q *Queue) fail(j *job, state, errKey string, err error) {
	log.Printf("job %s: %s (%s): %v", j.ID, state, errKey, err)
	q.end(j, state, errKey)
}

func (q *Queue) process(ctx context.Context, j *job) {
	q.mu.Lock()
	prepare, poll, trackMax := q.prepare, q.poll, q.trackMax
	q.mu.Unlock()

	// Is the printer there at all?
	sctx, cancel := context.WithTimeout(ctx, statusTimeout)
	st, err := q.b.Status(sctx)
	cancel()
	if ctx.Err() != nil {
		q.fail(j, "canceled", "server_stopped", ctx.Err())
		return
	}
	if err != nil || st.State == backend.StateUnreachable {
		if err == nil {
			err = errors.New("printer state is unreachable")
		}
		q.fail(j, "failed", "printer_unreachable", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, statusTimeout)
	caps, err := q.b.Caps(cctx)
	cancel()
	// If the capabilities cannot be read we cannot tell whether the job can
	// be tracked, so the job is treated as non-tracking and ends "sent".
	canTrack := err == nil && caps.CanTrack
	if err != nil {
		log.Printf("job %s: cannot read printer capabilities, not tracking: %v", j.ID, err)
	}

	prep, err := prepare(ctx, j.pdf, j.pages, q.tmp)
	if err != nil {
		if ctx.Err() != nil {
			q.fail(j, "canceled", "server_stopped", err)
			return
		}
		q.fail(j, "failed", "conversion_failed", err)
		return
	}
	if ctx.Err() != nil {
		_ = os.Remove(prep)
		q.fail(j, "canceled", "server_stopped", ctx.Err())
		return
	}
	q.mu.Lock()
	j.prep, j.State = prep, "sending"
	opts, name := resolveOrientation(j.opts, j.orient), j.Name
	q.mu.Unlock()

	subCtx, cancel := context.WithTimeout(ctx, submitTimeout)
	h, err := q.b.Submit(subCtx, prep, name, opts)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			// Shutting down mid-send: the printer may already have the job.
			log.Printf("job %s: shutting down while sending, ended as sent: %v", j.ID, err)
			q.end(j, "sent", "")
			return
		}
		key := "printer_error"
		if isNetErr(err) {
			key = "printer_unreachable"
		}
		q.fail(j, "failed", key, err)
		return
	}
	q.mu.Lock()
	j.handle, j.State = h, "printing"
	q.mu.Unlock()
	if !canTrack {
		q.end(j, "sent", "")
		return
	}
	q.track(ctx, j, h, poll, trackMax)
}

func (q *Queue) track(ctx context.Context, j *job, h backend.Handle, poll, trackMax time.Duration) {
	start := time.Now()
	for {
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			q.end(j, "sent", "") // shutting down; the printer has the job
			return
		case <-j.cancel:
			t.Stop()
			q.end(j, "canceled", "")
			return
		case <-t.C:
		}
		q.mu.Lock()
		canceled := j.State == "canceled"
		q.mu.Unlock()
		if canceled {
			q.end(j, "canceled", "")
			return
		}
		tctx, cancel := context.WithTimeout(ctx, trackTimeout)
		st, err := q.b.Track(tctx, h)
		cancel()
		if err == nil {
			switch st {
			case backend.JobDone:
				q.end(j, "done", "")
				return
			case backend.JobFailed:
				q.fail(j, "failed", "printer_error", errors.New("the printer aborted the job"))
				return
			case backend.JobCanceled:
				q.end(j, "canceled", "")
				return
			}
		} // transient errors are retried
		if time.Since(start) >= trackMax {
			log.Printf("job %s: no final state after %v, ended as sent (last error: %v)", j.ID, trackMax, err)
			q.end(j, "sent", "")
			return
		}
	}
}
