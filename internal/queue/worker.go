package queue

import (
	"context"
	"errors"
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

func isNetErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}

func (q *Queue) process(ctx context.Context, j *job) {
	q.mu.Lock()
	prepare, poll, trackMax := q.prepare, q.poll, q.trackMax
	q.mu.Unlock()

	// Is the printer there at all?
	sctx, cancel := context.WithTimeout(ctx, statusTimeout)
	st, err := q.b.Status(sctx)
	cancel()
	if err != nil || st.State == backend.StateUnreachable {
		q.end(j, "failed", "printer_unreachable")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, statusTimeout)
	caps, err := q.b.Caps(cctx)
	cancel()
	canTrack := err == nil && caps.CanTrack

	prep, err := prepare(ctx, j.pdf, j.pages, q.tmp)
	if err != nil {
		q.end(j, "failed", "conversion_failed")
		return
	}
	q.mu.Lock()
	j.prep, j.State = prep, "sending"
	opts, name := j.opts, j.Name
	q.mu.Unlock()

	subCtx, cancel := context.WithTimeout(ctx, submitTimeout)
	h, err := q.b.Submit(subCtx, prep, name, opts)
	cancel()
	if err != nil {
		key := "printer_error"
		if isNetErr(err) {
			key = "printer_unreachable"
		}
		q.end(j, "failed", key)
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
			q.end(j, "failed", "printer_error")
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
				q.end(j, "failed", "printer_error")
				return
			case backend.JobCanceled:
				q.end(j, "canceled", "")
				return
			}
		} // transient errors are retried
		if time.Since(start) >= trackMax {
			q.end(j, "sent", "")
			return
		}
	}
}
