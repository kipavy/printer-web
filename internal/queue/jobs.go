package queue

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/kipavy/printer-web/internal/backend"
	"github.com/kipavy/printer-web/internal/docs"
)

const (
	historyMax = 20
	maxCopies  = 99
)

// Request asks to print an upload.
type Request struct {
	Upload, Key, Visitor, Pages string
	Opts                        backend.Options
}

// Job is a print job as shown to clients.
type Job struct {
	ID, Visitor, Name string
	Sheets, Pages     int
	// State is queued|preparing|sending|printing|sent|done|failed|canceled.
	State    string
	Error    string // message key such as printer_unreachable
	Position int    // 1-based among active jobs, 0 when finished
	Created  time.Time
	Finished time.Time
}

// View is a job as seen by one visitor; Name is "" when masked.
type View struct {
	Job
	Mine bool
}

type job struct {
	Job
	key    string // idempotency map key
	pdf    string // draft file, owned by the job
	prep   string // Prepare output, once created
	pages  []int
	opts   backend.Options
	handle backend.Handle
}

// Queue is a serial FIFO print queue served by one worker (Run).
type Queue struct {
	b      backend.Backend
	drafts *Drafts
	tmp    string
	now    func() time.Time

	notify chan struct{}

	mu       sync.Mutex
	jobs     map[string]*job // active and history, by ID
	order    []*job          // active jobs, FIFO
	history  []*job          // finished, newest first
	keys     map[string]string
	prepare  func(ctx context.Context, pdf string, pages []int, tmp string) (string, error)
	poll     time.Duration
	trackMax time.Duration
}

// NewQueue creates a queue; call Run to start its worker.
func NewQueue(b backend.Backend, d *Drafts, tmp string, now func() time.Time) *Queue {
	if now == nil {
		now = time.Now
	}
	return &Queue{
		b: b, drafts: d, tmp: tmp, now: now,
		notify:   make(chan struct{}, 1),
		jobs:     map[string]*job{},
		keys:     map[string]string{},
		prepare:  docs.Prepare,
		poll:     2 * time.Second,
		trackMax: 30 * time.Minute,
	}
}

// Sheets is the number of paper sheets: ceil(pages/2) per copy when duplex.
func Sheets(pages, copies int, sides string) int {
	per := pages
	if sides == "long" || sides == "short" {
		per = (pages + 1) / 2
	}
	return per * copies
}

func isActive(s string) bool {
	return s == "queued" || s == "preparing" || s == "sending" || s == "printing"
}

// snapshot returns the job with its position; the lock must be held.
func (q *Queue) snapshot(j *job) Job {
	out := j.Job
	out.Position = 0
	if isActive(j.State) {
		n := 0
		for _, o := range q.order {
			if o.State == "canceled" {
				continue
			}
			n++
			if o == j {
				out.Position = n
				break
			}
		}
	}
	return out
}

// Submit validates the request and queues a job. The same Key returns the
// same Job; an upload that was already taken gives ErrNotFound.
func (q *Queue) Submit(r Request) (Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	mapKey := r.Visitor + "\x00" + r.Key
	if id, ok := q.keys[mapKey]; ok {
		if j := q.jobs[id]; j != nil {
			return q.snapshot(j), nil
		}
	}
	dr, err := q.drafts.Get(r.Upload, r.Visitor)
	switch err {
	case nil:
	case ErrForbidden:
		return Job{}, ErrNotFound // do not reveal other visitors' uploads
	default:
		return Job{}, err
	}
	pages, err := docs.ParsePages(r.Pages, dr.Pages)
	if err != nil {
		return Job{}, ErrBadPages
	}
	if _, err := q.drafts.Take(r.Upload, r.Visitor); err != nil {
		return Job{}, ErrNotFound
	}
	r.Opts.Copies = min(max(r.Opts.Copies, 1), maxCopies)
	j := &job{
		Job: Job{
			ID: randomID(), Visitor: r.Visitor, Name: dr.Name, State: "queued",
			Pages: len(pages), Sheets: Sheets(len(pages), r.Opts.Copies, r.Opts.Sides),
			Created: q.now(),
		},
		key: mapKey, pdf: dr.PDF, pages: pages, opts: r.Opts,
	}
	q.jobs[j.ID] = j
	q.keys[mapKey] = j.ID
	q.order = append(q.order, j)
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return q.snapshot(j), nil
}

func (q *Queue) view(j *job, visitor string, showNames bool) View {
	v := View{Job: q.snapshot(j), Mine: j.Visitor == visitor}
	if !v.Mine && !showNames {
		v.Name = ""
	}
	v.Visitor = "" // never expose visitor ids
	return v
}

// List returns active jobs in order.
func (q *Queue) List(visitor string, showNames bool) []View {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []View{}
	for _, j := range q.order {
		if j.State == "canceled" {
			continue
		}
		out = append(out, q.view(j, visitor, showNames))
	}
	return out
}

// History returns the last finished jobs, newest first.
func (q *Queue) History(visitor string, showNames bool) []View {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []View{}
	for _, j := range q.history {
		out = append(out, q.view(j, visitor, showNames))
	}
	return out
}

// Cancel cancels the visitor's own queued job, or printing job when the
// backend can cancel.
func (q *Queue) Cancel(id, visitor string) error {
	q.mu.Lock()
	j := q.jobs[id]
	switch {
	case j == nil:
		q.mu.Unlock()
		return ErrNotFound
	case j.Visitor != visitor:
		q.mu.Unlock()
		return ErrForbidden
	case j.State == "queued":
		q.removeActive(j)
		delete(q.jobs, id)
		delete(q.keys, j.key)
		q.mu.Unlock()
		_ = os.Remove(j.pdf)
		return nil
	case j.State != "printing":
		q.mu.Unlock()
		return ErrNotActive
	}
	h := j.handle
	q.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	caps, err := q.b.Caps(ctx)
	if err != nil {
		return err
	}
	if !caps.CanCancel {
		return ErrNotActive
	}
	if err := q.b.Cancel(ctx, h); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if j.State == "printing" {
		j.State = "canceled"
		j.Finished = q.now()
	}
	return nil
}

// removeActive drops j from the active order; the lock must be held.
func (q *Queue) removeActive(j *job) {
	for i, o := range q.order {
		if o == j {
			q.order = append(q.order[:i], q.order[i+1:]...)
			return
		}
	}
}

// finish moves j to the history; the lock must be held. A job already
// canceled keeps that state.
func (q *Queue) finish(j *job, state, errKey string) {
	if j.State != "canceled" {
		j.State, j.Error = state, errKey
		j.Finished = q.now()
	}
	q.removeActive(j)
	q.history = append([]*job{j}, q.history...)
	for len(q.history) > historyMax {
		old := q.history[len(q.history)-1]
		q.history = q.history[:len(q.history)-1]
		delete(q.jobs, old.ID)
		delete(q.keys, old.key)
	}
}
