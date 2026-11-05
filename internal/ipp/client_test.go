package ipp_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/kipavy/printer-web/internal/ipp"
	"github.com/kipavy/printer-web/internal/ipp/ipptest"
)

func TestServerURL(t *testing.T) {
	s := ipptest.NewServer(t, nil)
	if !strings.HasPrefix(s.URL, "ipp://127.0.0.1:") || !strings.HasSuffix(s.URL, "/ipp/print") {
		t.Fatalf("URL = %q", s.URL)
	}
}

func TestGetPrinterAttributes(t *testing.T) {
	s := ipptest.NewServer(t, map[string][]any{
		"printer-name":                 {"Office"},
		"printer-make-and-model":       {"Acme 1000"},
		"printer-state":                {3},
		"sides-supported":              {"one-sided", "two-sided-long-edge"},
		"copies-supported":             {ipp.Range{Lo: 1, Hi: 99}},
		"printer-resolution-supported": {ipp.Resolution{X: 600, Y: 600, Units: 3}},
		"color-supported":              {true},
	})
	c := ipp.NewClient(s.URL, false)
	got, err := c.GetPrinterAttributes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]any{
		"printer-name":                 {"Office"},
		"printer-make-and-model":       {"Acme 1000"},
		"printer-state":                {int32(3)},
		"sides-supported":              {"one-sided", "two-sided-long-edge"},
		"copies-supported":             {ipp.Range{Lo: 1, Hi: 99}},
		"printer-resolution-supported": {ipp.Resolution{X: 600, Y: 600, Units: 3}},
		"color-supported":              {true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}

	got, err = c.GetPrinterAttributes(context.Background(), "printer-name", "sides-supported")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["printer-name"][0] != "Office" || len(got["sides-supported"]) != 2 {
		t.Fatalf("filtered: %#v", got)
	}
	if n := s.OpCount(ipp.OpGetPrinterAttributes); n != 2 {
		t.Fatalf("OpCount = %d", n)
	}
}

func TestDefaultAttributes(t *testing.T) {
	s := ipptest.NewServer(t, nil)
	got, err := ipp.NewClient(s.URL, false).GetPrinterAttributes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"printer-name", "document-format-supported", "sides-supported",
		"print-color-mode-supported", "media-supported", "copies-supported"} {
		if len(got[name]) == 0 {
			t.Errorf("default attrs lack %s", name)
		}
	}
}

// onlyReader hides Len() so the client must stream with chunked encoding.
type onlyReader struct{ io.Reader }

func TestPrintJobDeliversBytesAndAttributes(t *testing.T) {
	doc := make([]byte, 300<<10)
	rand.New(rand.NewSource(2)).Read(doc)
	for name, r := range map[string]func() io.Reader{
		"sized":    func() io.Reader { return bytes.NewReader(doc) },
		"streamed": func() io.Reader { return onlyReader{bytes.NewReader(doc)} },
	} {
		t.Run(name, func(t *testing.T) {
			s := ipptest.NewServer(t, nil)
			c := ipp.NewClient(s.URL, false)
			id, err := c.PrintJob(context.Background(), r(), "application/pdf", "report.pdf", []ipp.Attr{
				{Name: "copies", Tag: ipp.TagInteger, Values: []any{2}},
				{Name: "sides", Tag: ipp.TagKeyword, Values: []any{"two-sided-long-edge"}},
				{Name: "print-color-mode", Tag: ipp.TagKeyword, Values: []any{"monochrome"}},
				{Name: "media", Tag: ipp.TagKeyword, Values: []any{"iso_a4_210x297mm"}},
				{Name: "orientation-requested", Tag: ipp.TagEnum, Values: []any{int32(4)}},
				{Name: "print-quality", Tag: ipp.TagEnum, Values: []any{int32(5)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			jobs := s.Jobs()
			if len(jobs) != 1 || jobs[0].ID != id || id != 1 {
				t.Fatalf("id=%d jobs=%+v", id, jobs)
			}
			j := jobs[0]
			if !bytes.Equal(j.Document, doc) {
				t.Fatalf("document differs: %d bytes vs %d", len(j.Document), len(doc))
			}
			want := map[string][]any{
				"attributes-charset":          {"utf-8"},
				"attributes-natural-language": {"en"},
				"printer-uri":                 {s.URL},
				"requesting-user-name":        {"printer-web"},
				"job-name":                    {"report.pdf"},
				"document-format":             {"application/pdf"},
				"copies":                      {int32(2)},
				"sides":                       {"two-sided-long-edge"},
				"print-color-mode":            {"monochrome"},
				"media":                       {"iso_a4_210x297mm"},
				"orientation-requested":       {int32(4)},
				"print-quality":               {int32(5)},
			}
			if !reflect.DeepEqual(j.Attrs, want) {
				t.Fatalf("attrs:\n got %#v\nwant %#v", j.Attrs, want)
			}
			if _, ok := j.Attrs["page-ranges"]; ok {
				t.Fatal("page-ranges must not be sent")
			}
		})
	}
}

func TestJobStatesPolled(t *testing.T) {
	s := ipptest.NewServer(t, nil)
	c := ipp.NewClient(s.URL, false)
	ctx := context.Background()
	id, err := c.PrintJob(ctx, strings.NewReader("hello"), "application/octet-stream", "a.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int32{ipp.JobProcessing, ipp.JobCompleted, ipp.JobCompleted} {
		a, err := c.GetJobAttributes(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := a["job-state"]; len(got) != 1 || got[0] != want {
			t.Fatalf("poll %d: job-state = %v, want %d", i+1, got, want)
		}
		if a["job-id"][0] != int32(id) || len(a["job-state-reasons"]) == 0 {
			t.Fatalf("poll %d: %#v", i+1, a)
		}
	}
	if s.OpCount(ipp.OpGetJobAttributes) != 3 {
		t.Fatal("OpCount")
	}
}

func TestScriptedJobStates(t *testing.T) {
	s := ipptest.NewServer(t, nil)
	s.SetJobStates(ipp.JobPending, ipp.JobPending, ipp.JobAborted)
	c := ipp.NewClient(s.URL, false)
	ctx := context.Background()
	id, err := c.PrintJob(ctx, strings.NewReader("x"), "application/pdf", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Job(id); j.State != ipp.JobPending {
		t.Fatalf("initial state %d", j.State)
	}
	var got []any
	for i := 0; i < 3; i++ {
		a, err := c.GetJobAttributes(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, a["job-state"][0])
	}
	if !reflect.DeepEqual(got, []any{int32(ipp.JobPending), int32(ipp.JobAborted), int32(ipp.JobAborted)}) {
		t.Fatalf("states %v", got)
	}
	s.SetJobState(id, ipp.JobProcessingStopped)
	a, err := c.GetJobAttributes(ctx, id)
	if err != nil || a["job-state"][0] != int32(ipp.JobProcessingStopped) {
		t.Fatalf("override: %v %v", a, err)
	}
	if _, ok := s.Jobs()[0].Attrs["job-name"]; ok {
		t.Fatal("empty job name must be omitted")
	}
}

func TestCancelJob(t *testing.T) {
	s := ipptest.NewServer(t, nil)
	c := ipp.NewClient(s.URL, false)
	ctx := context.Background()
	id, err := c.PrintJob(ctx, strings.NewReader("x"), "application/pdf", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CancelJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	a, err := c.GetJobAttributes(ctx, id)
	if err != nil || a["job-state"][0] != int32(ipp.JobCanceled) {
		t.Fatalf("after cancel: %v %v", a, err)
	}
	var se *ipp.StatusError
	if err := c.CancelJob(ctx, id); !errors.As(err, &se) || se.Code != ipp.StatusNotPossible {
		t.Fatalf("second cancel: %v", err)
	}
	if err := c.CancelJob(ctx, 99); !errors.As(err, &se) || se.Code != ipp.StatusNotFound {
		t.Fatalf("unknown job: %v", err)
	}
	if _, err := c.GetJobAttributes(ctx, 99); !errors.As(err, &se) || se.Code != ipp.StatusNotFound {
		t.Fatalf("unknown job attrs: %v", err)
	}
}

func TestErrorStatus(t *testing.T) {
	s := ipptest.NewServer(t, nil)
	c := ipp.NewClient(s.URL, false)
	ctx := context.Background()
	s.FailOpN(ipp.OpPrintJob, ipp.StatusBusy, 1)
	_, err := c.PrintJob(ctx, strings.NewReader("x"), "application/pdf", "x", nil)
	var se *ipp.StatusError
	if !errors.As(err, &se) || se.Code != 0x0507 || se.Op != ipp.OpPrintJob {
		t.Fatalf("want busy, got %v", err)
	}
	if !strings.Contains(err.Error(), "server-error-busy") {
		t.Fatalf("error text %q", err)
	}
	if len(s.Jobs()) != 0 {
		t.Fatal("failed job must not be recorded")
	}
	// The failure was one-shot.
	if _, err := c.PrintJob(ctx, strings.NewReader("x"), "application/pdf", "x", nil); err != nil {
		t.Fatal(err)
	}

	s.FailOp(ipp.OpGetPrinterAttributes, ipp.StatusInternalError)
	for i := 0; i < 2; i++ {
		if _, err := c.GetPrinterAttributes(ctx); !errors.As(err, &se) || se.Code != 0x0500 {
			t.Fatalf("want internal error, got %v", err)
		}
	}
	s.FailOp(ipp.OpGetPrinterAttributes, 0)
	if _, err := c.GetPrinterAttributes(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedDocumentFormat(t *testing.T) {
	s := ipptest.NewServer(t, map[string][]any{"document-format-supported": {"application/pdf"}})
	c := ipp.NewClient(s.URL, false)
	_, err := c.PrintJob(context.Background(), strings.NewReader("x"), "image/jpeg", "x", nil)
	var se *ipp.StatusError
	if !errors.As(err, &se) || se.Code != ipp.StatusDocumentFormatNotSupported {
		t.Fatalf("got %v", err)
	}
}

func TestHTTPErrorAndContext(t *testing.T) {
	s := ipptest.NewServer(t, nil)
	bad := ipp.NewClient(strings.Replace(s.URL, "/ipp/print", "/nope", 1), false)
	if _, err := bad.GetPrinterAttributes(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want HTTP 404 error, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ipp.NewClient(s.URL, false).GetPrinterAttributes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestIPPS(t *testing.T) {
	s := ipptest.NewTLSServer(t, nil)
	if !strings.HasPrefix(s.URL, "ipps://127.0.0.1:") {
		t.Fatalf("URL %q", s.URL)
	}
	if _, err := ipp.NewClient(s.URL, true).GetPrinterAttributes(context.Background()); err != nil {
		t.Fatalf("insecure TLS: %v", err)
	}
	if _, err := ipp.NewClient(s.URL, false).GetPrinterAttributes(context.Background()); err == nil {
		t.Fatal("self-signed cert must be rejected without insecureTLS")
	}
}
