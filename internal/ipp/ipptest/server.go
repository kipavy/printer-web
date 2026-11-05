// Package ipptest provides a scriptable fake IPP printer for tests.
package ipptest

import (
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/kipavy/printer-web/internal/ipp"
)

// Job is a job received by the fake printer.
type Job struct {
	ID       int
	Attrs    map[string][]any // operation + job attributes of the Print-Job request
	Document []byte
	State    int // current job-state

	// HTTP framing of the Print-Job request: Content-Length (-1 if absent)
	// and whether it was sent with Transfer-Encoding: chunked.
	ContentLength int64
	Chunked       bool

	script   []int
	polls    int
	override bool // state fixed by Cancel-Job or SetJobState
}

type failure struct {
	status uint16
	left   int // < 0: forever
}

// Server is a fake IPP printer on 127.0.0.1.
type Server struct {
	URL     string // ipp://127.0.0.1:port/ipp/print (ipps:// for TLS)
	HTTPURL string // http(s)://127.0.0.1:port/ipp/print

	srv    *httptest.Server
	mu     sync.Mutex
	attrs  []ipp.Attr
	script []int
	fail   map[uint16]*failure
	ops    map[uint16]int
	jobs   []*Job
}

// NewServer starts a fake printer answering with attrs (DefaultAttributes if
// nil). Tags are inferred from the Go type and attribute name; use SetAttrs
// for exact tags. It is closed when the test ends.
func NewServer(t testing.TB, attrs map[string][]any) *Server {
	return start(t, attrs, false)
}

// NewTLSServer is NewServer over HTTPS with a self-signed certificate.
func NewTLSServer(t testing.TB, attrs map[string][]any) *Server {
	return start(t, attrs, true)
}

func start(t testing.TB, attrs map[string][]any, tls bool) *Server {
	t.Helper()
	if attrs == nil {
		attrs = DefaultAttributes()
	}
	s := &Server{
		script: []int{ipp.JobPending, ipp.JobProcessing, ipp.JobCompleted},
		fail:   map[uint16]*failure{},
		ops:    map[uint16]int{},
	}
	names := make([]string, 0, len(attrs))
	for n := range attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s.SetAttr(n, attrs[n]...)
	}
	h := http.HandlerFunc(s.handle)
	scheme := "ipp"
	if tls {
		s.srv = httptest.NewTLSServer(h)
		scheme = "ipps"
	} else {
		s.srv = httptest.NewServer(h)
	}
	t.Cleanup(s.srv.Close)
	s.HTTPURL = s.srv.URL + "/ipp/print"
	s.URL = scheme + "://" + s.srv.Listener.Addr().String() + "/ipp/print"
	return s
}

// Close stops the server (also done automatically at test end).
func (s *Server) Close() { s.srv.Close() }

// DefaultAttributes is a plausible A4 duplex color printer.
func DefaultAttributes() map[string][]any {
	return map[string][]any{
		"printer-name":                    {"Fake Printer"},
		"printer-make-and-model":          {"Fake Printer 1000"},
		"printer-info":                    {"Fake Printer"},
		"printer-state":                   {ipp.JobPending}, // 3 = idle
		"printer-state-reasons":           {"none"},
		"printer-is-accepting-jobs":       {true},
		"ipp-versions-supported":          {"1.1", "2.0"},
		"operations-supported":            {int32(ipp.OpPrintJob), int32(ipp.OpCancelJob), int32(ipp.OpGetJobAttributes), int32(ipp.OpGetPrinterAttributes)},
		"document-format-supported":       {"application/octet-stream", "application/pdf", "image/jpeg", "image/urf", "image/pwg-raster"},
		"document-format-default":         {"application/octet-stream"},
		"color-supported":                 {true},
		"copies-supported":                {ipp.Range{Lo: 1, Hi: 99}},
		"copies-default":                  {1},
		"sides-supported":                 {"one-sided", "two-sided-long-edge", "two-sided-short-edge"},
		"sides-default":                   {"one-sided"},
		"print-color-mode-supported":      {"auto", "monochrome", "color"},
		"print-color-mode-default":        {"auto"},
		"media-supported":                 {"iso_a4_210x297mm", "iso_a5_148x210mm", "na_letter_8.5x11in"},
		"media-default":                   {"iso_a4_210x297mm"},
		"media-ready":                     {"iso_a4_210x297mm"},
		"orientation-requested-supported": {3, 4},
		"orientation-requested-default":   {3},
		"print-quality-supported":         {3, 4, 5},
		"print-quality-default":           {4},
		"printer-resolution-supported":    {ipp.Resolution{X: 300, Y: 300, Units: 3}, ipp.Resolution{X: 600, Y: 600, Units: 3}},
		"printer-resolution-default":      {ipp.Resolution{X: 600, Y: 600, Units: 3}},
	}
}

// SetAttr adds or replaces a printer attribute, inferring its tag.
// No values means no-value.
func (s *Server) SetAttr(name string, values ...any) {
	if len(values) == 0 {
		values = []any{nil}
	}
	s.SetAttrs(ipp.Attr{Name: name, Tag: inferTag(name, values[0]), Values: normalize(values)})
}

// SetAttrs adds or replaces printer attributes with explicit tags.
func (s *Server) SetAttrs(attrs ...ipp.Attr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range attrs {
		i := slices.IndexFunc(s.attrs, func(x ipp.Attr) bool { return x.Name == a.Name })
		if i >= 0 {
			s.attrs[i] = a
		} else {
			s.attrs = append(s.attrs, a)
		}
	}
}

// DeleteAttr removes a printer attribute.
func (s *Server) DeleteAttr(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs = slices.DeleteFunc(s.attrs, func(x ipp.Attr) bool { return x.Name == name })
}

// SetJobStates scripts the job-state of jobs created afterwards: the Print-Job
// response reports states[0] and the Nth Get-Job-Attributes reports
// states[min(N, len-1)]. Default: pending, processing, completed.
func (s *Server) SetJobStates(states ...int) {
	if len(states) == 0 {
		panic("ipptest: SetJobStates needs at least one state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = slices.Clone(states)
}

// SetJobState forces a job's state; the script no longer advances it.
func (s *Server) SetJobState(id, state int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.job(id); j != nil {
		j.State, j.override = state, true
	}
}

// FailOp makes every request for op answer status (0 clears).
func (s *Server) FailOp(op, status uint16) { s.FailOpN(op, status, -1) }

// FailOpN makes the next n requests for op answer status (n < 0: forever).
func (s *Server) FailOpN(op, status uint16, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status == 0 || n == 0 {
		delete(s.fail, op)
		return
	}
	s.fail[op] = &failure{status: status, left: n}
}

// OpCount returns how many requests for op were received.
func (s *Server) OpCount(op uint16) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ops[op]
}

// Jobs returns copies of the jobs received so far, oldest first.
func (s *Server) Jobs() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, len(s.jobs))
	for i, j := range s.jobs {
		out[i] = j.copy()
	}
	return out
}

// Job returns a copy of one job.
func (s *Server) Job(id int) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.job(id); j != nil {
		return j.copy(), true
	}
	return Job{}, false
}

func (j *Job) copy() Job {
	c := Job{ID: j.ID, State: j.State, ContentLength: j.ContentLength, Chunked: j.Chunked, Document: slices.Clone(j.Document), Attrs: map[string][]any{}}
	for k, v := range j.Attrs {
		c.Attrs[k] = slices.Clone(v)
	}
	return c
}

func (s *Server) job(id int) *Job {
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ipp/print" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/ipp" {
		http.Error(w, "want application/ipp", http.StatusBadRequest)
		return
	}
	req, doc, err := ipp.Decode(http.MaxBytesReader(w, r.Body, 256<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	resp := s.answer(req, doc, r)
	s.mu.Unlock()
	b, err := resp.MarshalBinary()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/ipp")
	_, _ = w.Write(b)
}

// answer builds the response; s.mu is held.
func (s *Server) answer(req *ipp.Message, doc []byte, r *http.Request) *ipp.Message {
	s.ops[req.Code]++
	resp := &ipp.Message{Version: req.Version, RequestID: req.RequestID}
	status := func(code uint16, msg string, groups ...ipp.Group) *ipp.Message {
		resp.Code = code
		op := []ipp.Attr{
			{Name: "attributes-charset", Tag: ipp.TagCharset, Values: []any{"utf-8"}},
			{Name: "attributes-natural-language", Tag: ipp.TagLanguage, Values: []any{"en"}},
		}
		if msg != "" {
			op = append(op, ipp.Attr{Name: "status-message", Tag: ipp.TagText, Values: []any{msg}})
		}
		resp.Groups = append([]ipp.Group{{Tag: ipp.TagOperation, Attrs: op}}, groups...)
		return resp
	}

	opAttrs := flatten(req, ipp.TagOperation)
	for _, n := range []string{"attributes-charset", "attributes-natural-language", "printer-uri"} {
		if len(opAttrs[n]) == 0 {
			return status(ipp.StatusBadRequest, "missing "+n)
		}
	}
	if f := s.fail[req.Code]; f != nil {
		if f.left > 0 {
			if f.left--; f.left == 0 {
				delete(s.fail, req.Code)
			}
		}
		return status(f.status, "scripted failure")
	}

	switch req.Code {
	case ipp.OpGetPrinterAttributes:
		return status(ipp.StatusOK, "", ipp.Group{Tag: ipp.TagPrinter, Attrs: s.requested(opAttrs["requested-attributes"])})

	case ipp.OpPrintJob:
		if f := strOf(opAttrs["document-format"]); f != "" && !s.formatSupported(f) {
			return status(ipp.StatusDocumentFormatNotSupported, "unsupported document-format "+f)
		}
		attrs := opAttrs
		for k, v := range flatten(req, ipp.TagJob) {
			attrs[k] = v
		}
		j := &Job{ID: len(s.jobs) + 1, Attrs: attrs, Document: doc, script: slices.Clone(s.script)}
		j.ContentLength = r.ContentLength
		j.Chunked = slices.Contains(r.TransferEncoding, "chunked")
		j.State = j.script[0]
		s.jobs = append(s.jobs, j)
		return status(ipp.StatusOK, "", s.jobGroup(j))

	case ipp.OpGetJobAttributes, ipp.OpCancelJob:
		id, ok := intOf(opAttrs["job-id"])
		if !ok {
			return status(ipp.StatusBadRequest, "missing job-id")
		}
		j := s.job(id)
		if j == nil {
			return status(ipp.StatusNotFound, fmt.Sprintf("job %d not found", id))
		}
		if req.Code == ipp.OpCancelJob {
			if j.State >= ipp.JobCanceled {
				return status(ipp.StatusNotPossible, "job already finished")
			}
			j.State, j.override = ipp.JobCanceled, true
			return status(ipp.StatusOK, "")
		}
		j.polls++
		if !j.override {
			j.State = j.script[min(j.polls, len(j.script)-1)]
		}
		return status(ipp.StatusOK, "", s.jobGroup(j))
	}
	return status(ipp.StatusOperationNotSupported, "")
}

func (s *Server) jobGroup(j *Job) ipp.Group {
	attrs := []ipp.Attr{
		{Name: "job-id", Tag: ipp.TagInteger, Values: []any{j.ID}},
		{Name: "job-uri", Tag: ipp.TagURI, Values: []any{fmt.Sprintf("%s/jobs/%d", s.URL, j.ID)}},
		{Name: "job-state", Tag: ipp.TagEnum, Values: []any{j.State}},
		{Name: "job-state-reasons", Tag: ipp.TagKeyword, Values: []any{stateReason(j.State)}},
	}
	if n := strOf(j.Attrs["job-name"]); n != "" {
		attrs = append(attrs, ipp.Attr{Name: "job-name", Tag: ipp.TagName, Values: []any{n}})
	}
	return ipp.Group{Tag: ipp.TagJob, Attrs: attrs}
}

func stateReason(state int) string {
	switch state {
	case ipp.JobProcessing:
		return "job-printing"
	case ipp.JobProcessingStopped:
		return "printer-stopped"
	case ipp.JobCanceled:
		return "job-canceled-by-user"
	case ipp.JobAborted:
		return "aborted-by-system"
	case ipp.JobCompleted:
		return "job-completed-successfully"
	}
	return "none"
}

// requested filters printer attributes by requested-attributes.
func (s *Server) requested(names []any) []ipp.Attr {
	want := map[string]bool{}
	for _, n := range names {
		if str, ok := n.(string); ok {
			want[str] = true
		}
	}
	if len(want) == 0 || want["all"] || want["printer-description"] || want["job-template"] {
		return slices.Clone(s.attrs)
	}
	var out []ipp.Attr
	for _, a := range s.attrs {
		if want[a.Name] {
			out = append(out, a)
		}
	}
	return out
}

func (s *Server) formatSupported(f string) bool {
	for _, a := range s.attrs {
		if a.Name == "document-format-supported" {
			return slices.Contains(a.Values, any(f))
		}
	}
	return true // not advertised: accept anything
}

func flatten(m *ipp.Message, tag byte) map[string][]any {
	out := map[string][]any{}
	for _, g := range m.Groups {
		if g.Tag == tag {
			for _, a := range g.Attrs {
				out[a.Name] = append(out[a.Name], a.Values...)
			}
		}
	}
	return out
}

func strOf(v []any) string {
	if len(v) > 0 {
		s, _ := v[0].(string)
		return s
	}
	return ""
}

func intOf(v []any) (int, bool) {
	if len(v) > 0 {
		n, ok := v[0].(int32)
		return int(n), ok
	}
	return 0, false
}

// normalize converts int to int32 so encoded values decode identically.
func normalize(values []any) []any {
	out := make([]any, len(values))
	for i, v := range values {
		if n, ok := v.(int); ok {
			v = int32(n)
		}
		out[i] = v
	}
	return out
}

func inferTag(name string, v any) byte {
	switch v.(type) {
	case nil:
		return ipp.TagNoValue
	case bool:
		return ipp.TagBoolean
	case ipp.Range:
		return ipp.TagRange
	case ipp.Resolution:
		return ipp.TagResolution
	case []byte:
		return ipp.TagOctetString
	case int, int32:
		for _, p := range []string{"printer-state", "job-state", "operations-supported", "finishings", "orientation-requested", "print-quality", "printer-type"} {
			if name == p || strings.HasPrefix(name, p+"-") {
				if name != "printer-state-change-time" && name != "printer-state-reasons" {
					return ipp.TagEnum
				}
			}
		}
		return ipp.TagInteger
	}
	switch {
	case name == "printer-name" || name == "job-name" || name == "requesting-user-name":
		return ipp.TagName
	case name == "printer-make-and-model" || name == "printer-info" || name == "printer-location" ||
		name == "printer-state-message" || name == "status-message" || name == "printer-message-from-operator":
		return ipp.TagText
	case strings.HasSuffix(name, "-uri") || strings.HasSuffix(name, "-uri-supported") ||
		name == "printer-more-info" || name == "printer-icons":
		return ipp.TagURI
	case strings.HasPrefix(name, "document-format"):
		return ipp.TagMimeType
	case name == "charset-configured" || name == "charset-supported":
		return ipp.TagCharset
	case name == "natural-language-configured" || name == "generated-natural-language-supported":
		return ipp.TagLanguage
	}
	return ipp.TagKeyword
}
