// Package queue holds upload drafts and the serial print queue.
package queue

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/kipavy/printer-web/internal/docs"
)

// Errors returned by drafts and the queue.
var (
	ErrNotFound  = errors.New("queue: not found")
	ErrExpired   = errors.New("queue: expired")
	ErrForbidden = errors.New("queue: forbidden")
	ErrBadPages  = errors.New("queue: invalid page selection")
	ErrNotActive = errors.New("queue: job cannot be canceled")
	ErrBadKey    = errors.New("queue: missing idempotency key")
	ErrQueueFull = errors.New("queue: too many jobs waiting")
	ErrPrinter   = errors.New("queue: printer error")
)

// Draft is an uploaded, not yet printed document. Its PDF file is owned by
// the store until Take hands it to the caller.
type Draft struct {
	ID, Visitor, Name string
	Kind              docs.Kind
	PDF               string
	Pages             int
	Coverage          []docs.Coverage
	// Orientation of the first page: "portrait", "landscape" or "" (unknown).
	Orientation string
	Created     time.Time
}

// Drafts is an in-memory store of drafts with expiry.
type Drafts struct {
	mu  sync.Mutex
	m   map[string]Draft
	dir string
	ttl time.Duration
	now func() time.Time
}

// NewDrafts creates a store; dir is the upload directory, now may be nil.
func NewDrafts(dir string, ttl time.Duration, now func() time.Time) *Drafts {
	if now == nil {
		now = time.Now
	}
	return &Drafts{m: map[string]Draft{}, dir: dir, ttl: ttl, now: now}
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Add stores the draft under a new random ID and stamps its creation time.
func (d *Drafts) Add(dr Draft) Draft {
	dr.ID = randomID()
	d.mu.Lock()
	defer d.mu.Unlock()
	dr.Created = d.now()
	d.m[dr.ID] = dr
	return dr
}

func (d *Drafts) expired(dr Draft) bool { return !d.now().Before(dr.Created.Add(d.ttl)) }

// lookup must be called with the lock held.
func (d *Drafts) lookup(id, visitor string) (Draft, error) {
	dr, ok := d.m[id]
	if !ok {
		return Draft{}, ErrNotFound
	}
	if dr.Visitor != visitor {
		return Draft{}, ErrForbidden
	}
	if d.expired(dr) {
		return Draft{}, ErrExpired
	}
	return dr, nil
}

// Get returns a draft owned by visitor.
func (d *Drafts) Get(id, visitor string) (Draft, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lookup(id, visitor)
}

// Take removes the draft and hands over ownership of its file.
func (d *Drafts) Take(id, visitor string) (Draft, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dr, err := d.lookup(id, visitor)
	if err != nil {
		return Draft{}, err
	}
	delete(d.m, id)
	return dr, nil
}

// Delete removes the draft and its file.
func (d *Drafts) Delete(id, visitor string) error {
	d.mu.Lock()
	dr, err := d.lookup(id, visitor)
	if errors.Is(err, ErrExpired) {
		err = nil // deleting an expired draft is fine
		dr = d.m[id]
	}
	if err == nil {
		delete(d.m, id)
	}
	d.mu.Unlock()
	if err != nil {
		return err
	}
	_ = os.Remove(dr.PDF)
	return nil
}

// Purge deletes every draft and its file (used at shutdown).
func (d *Drafts) Purge() {
	d.mu.Lock()
	files := make([]string, 0, len(d.m))
	for id, dr := range d.m {
		files = append(files, dr.PDF)
		delete(d.m, id)
	}
	d.mu.Unlock()
	for _, f := range files {
		_ = os.Remove(f)
	}
}

// Sweep deletes expired drafts and their files.
func (d *Drafts) Sweep() {
	var files []string
	d.mu.Lock()
	for id, dr := range d.m {
		if d.expired(dr) {
			files = append(files, dr.PDF)
			delete(d.m, id)
		}
	}
	d.mu.Unlock()
	for _, f := range files {
		_ = os.Remove(f)
	}
}
