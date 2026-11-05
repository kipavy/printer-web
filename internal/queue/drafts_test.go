package queue

import (
	"errors"
	"os"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestDrafts(t *testing.T) (*Drafts, *clock) {
	c := &clock{t: time.Unix(1000, 0)}
	return NewDrafts(t.TempDir(), 15*time.Minute, c.now), c
}

func addFile(t *testing.T, d *Drafts, visitor string) Draft {
	t.Helper()
	f, err := os.CreateTemp(d.dir, "up-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	return d.Add(Draft{Visitor: visitor, Name: "a.pdf", PDF: f.Name(), Pages: 3})
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestDraftAddGet(t *testing.T) {
	d, _ := newTestDrafts(t)
	dr := addFile(t, d, "v1")
	if len(dr.ID) != 32 {
		t.Fatalf("id %q", dr.ID)
	}
	got, err := d.Get(dr.ID, "v1")
	if err != nil || got.Name != "a.pdf" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := d.Get(dr.ID, "v2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want forbidden, got %v", err)
	}
	if _, err := d.Get("nope", "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want notfound, got %v", err)
	}
}

func TestDraftTakeOnce(t *testing.T) {
	d, _ := newTestDrafts(t)
	dr := addFile(t, d, "v1")
	if _, err := d.Take(dr.ID, "v2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	got, err := d.Take(dr.ID, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !exists(got.PDF) {
		t.Fatal("take must keep the file")
	}
	if _, err := d.Take(dr.ID, "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second take: %v", err)
	}
}

func TestDraftExpiryAndSweep(t *testing.T) {
	d, c := newTestDrafts(t)
	dr := addFile(t, d, "v1")
	c.t = c.t.Add(14 * time.Minute)
	if _, err := d.Get(dr.ID, "v1"); err != nil {
		t.Fatal(err)
	}
	d.Sweep()
	if !exists(dr.PDF) {
		t.Fatal("not expired yet")
	}
	c.t = c.t.Add(2 * time.Minute)
	if _, err := d.Get(dr.ID, "v1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if _, err := d.Take(dr.ID, "v1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v", err)
	}
	d.Sweep()
	if exists(dr.PDF) {
		t.Fatal("file should be removed by Sweep")
	}
	if _, err := d.Get(dr.ID, "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDraftDelete(t *testing.T) {
	d, _ := newTestDrafts(t)
	dr := addFile(t, d, "v1")
	if err := d.Delete(dr.ID, "v2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	if !exists(dr.PDF) {
		t.Fatal("other visitor must not delete")
	}
	if err := d.Delete(dr.ID, "v1"); err != nil {
		t.Fatal(err)
	}
	if exists(dr.PDF) {
		t.Fatal("file should be removed")
	}
	if err := d.Delete(dr.ID, "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}
