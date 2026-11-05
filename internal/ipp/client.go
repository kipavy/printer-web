package ipp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Status codes.
const (
	StatusOK                         uint16 = 0x0000
	StatusBadRequest                 uint16 = 0x0400
	StatusNotFound                   uint16 = 0x0406
	StatusNotPossible                uint16 = 0x0404
	StatusDocumentFormatNotSupported uint16 = 0x040A
	StatusInternalError              uint16 = 0x0500
	StatusOperationNotSupported      uint16 = 0x0501
	StatusBusy                       uint16 = 0x0507
)

var statusNames = map[uint16]string{
	0x0000: "successful-ok",
	0x0001: "successful-ok-ignored-or-substituted-attributes",
	0x0002: "successful-ok-conflicting-attributes",
	0x0400: "client-error-bad-request",
	0x0401: "client-error-forbidden",
	0x0402: "client-error-not-authenticated",
	0x0403: "client-error-not-authorized",
	0x0404: "client-error-not-possible",
	0x0405: "client-error-timeout",
	0x0406: "client-error-not-found",
	0x0407: "client-error-gone",
	0x0408: "client-error-request-entity-too-large",
	0x0409: "client-error-request-value-too-long",
	0x040A: "client-error-document-format-not-supported",
	0x040B: "client-error-attributes-or-values-not-supported",
	0x040C: "client-error-uri-scheme-not-supported",
	0x040D: "client-error-charset-not-supported",
	0x040E: "client-error-conflicting-attributes",
	0x040F: "client-error-compression-not-supported",
	0x0410: "client-error-compression-error",
	0x0411: "client-error-document-format-error",
	0x0412: "client-error-document-access-error",
	0x0500: "server-error-internal-error",
	0x0501: "server-error-operation-not-supported",
	0x0502: "server-error-service-unavailable",
	0x0503: "server-error-version-not-supported",
	0x0504: "server-error-device-error",
	0x0505: "server-error-temporary-error",
	0x0506: "server-error-not-accepting-jobs",
	0x0507: "server-error-busy",
	0x0508: "server-error-job-canceled",
	0x0509: "server-error-multiple-document-jobs-not-supported",
}

// StatusName returns the RFC 8011 keyword of an IPP status code.
func StatusName(code uint16) string {
	if s, ok := statusNames[code]; ok {
		return s
	}
	switch code >> 8 {
	case 0x00:
		return fmt.Sprintf("successful-0x%04x", code)
	case 0x04:
		return fmt.Sprintf("client-error-0x%04x", code)
	case 0x05:
		return fmt.Sprintf("server-error-0x%04x", code)
	}
	return fmt.Sprintf("status-0x%04x", code)
}

// OpName returns a readable operation name.
func OpName(op uint16) string {
	switch op {
	case OpPrintJob:
		return "Print-Job"
	case OpCancelJob:
		return "Cancel-Job"
	case OpGetJobAttributes:
		return "Get-Job-Attributes"
	case OpGetPrinterAttributes:
		return "Get-Printer-Attributes"
	}
	return fmt.Sprintf("operation-0x%04x", op)
}

// StatusError is an IPP response with an error status (>= 0x0400).
type StatusError struct {
	Op      uint16
	Code    uint16
	Message string // status-message, if the printer sent one
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("ipp: %s: %s (0x%04x)", OpName(e.Op), StatusName(e.Code), e.Code)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// HTTPURL maps ipp://host[:port]/path to http://host:631/path and ipps:// to
// https:// (default port 631 too). http(s) URLs pass through unchanged.
func HTTPURL(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("ipp: bad printer URI: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("ipp: printer URI %q has no host", uri)
	}
	switch u.Scheme {
	case "http", "https":
		return u.String(), nil
	case "ipp":
		u.Scheme = "http"
	case "ipps":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("ipp: unsupported URI scheme %q", u.Scheme)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "631")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// Client talks to one printer.
type Client struct {
	URI  string // ipp://, ipps://, http:// or https://
	HTTP *http.Client
}

// NewClient returns a client with a 30 s timeout for attribute calls.
// insecureTLS skips certificate checks (printers use self-signed certs).
func NewClient(uri string, insecureTLS bool) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // user opt-in
	}
	return &Client{URI: uri, HTTP: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}

var requestID atomic.Uint32

// printerURI is the printer-uri attribute: always ipp:// or ipps://.
func (c *Client) printerURI() string {
	switch {
	case strings.HasPrefix(c.URI, "http://"):
		return "ipp://" + strings.TrimPrefix(c.URI, "http://")
	case strings.HasPrefix(c.URI, "https://"):
		return "ipps://" + strings.TrimPrefix(c.URI, "https://")
	}
	return c.URI
}

func (c *Client) request(op uint16, extra ...Attr) *Message {
	attrs := []Attr{
		{Name: "attributes-charset", Tag: TagCharset, Values: []any{"utf-8"}},
		{Name: "attributes-natural-language", Tag: TagLanguage, Values: []any{"en"}},
		{Name: "printer-uri", Tag: TagURI, Values: []any{c.printerURI()}},
		{Name: "requesting-user-name", Tag: TagName, Values: []any{"printer-web"}},
	}
	return &Message{
		Version:   [2]byte{2, 0},
		Code:      op,
		RequestID: requestID.Add(1),
		Groups:    []Group{{Tag: TagOperation, Attrs: append(attrs, extra...)}},
	}
}

// do sends req (followed by doc, if any) and returns the decoded response.
func (c *Client) do(ctx context.Context, hc *http.Client, req *Message, doc io.Reader) (*Message, error) {
	op := OpName(req.Code)
	hdr, err := req.MarshalBinary()
	if err != nil {
		return nil, err
	}
	u, err := HTTPURL(c.URI)
	if err != nil {
		return nil, err
	}
	body := io.Reader(bytes.NewReader(hdr))
	size := int64(0)
	if doc != nil {
		body = io.MultiReader(body, doc)
		size = docSize(doc)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, fmt.Errorf("ipp: %s: %w", op, err)
	}
	if size >= 0 {
		hr.ContentLength = int64(len(hdr)) + size
	} // else unknown length: chunked
	hr.Header.Set("Content-Type", "application/ipp")
	resp, err := hc.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("ipp: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ipp: %s: HTTP %s", op, resp.Status)
	}
	m, _, err := Decode(io.LimitReader(resp.Body, maxMessageLen+1))
	if err != nil {
		return nil, fmt.Errorf("ipp: %s: bad response: %w", op, err)
	}
	if m.Code >= 0x0400 {
		msg := ""
		if v := attrsOf(m, TagOperation)["status-message"]; len(v) > 0 {
			msg, _ = v[0].(string)
		}
		return nil, &StatusError{Op: req.Code, Code: m.Code, Message: msg}
	}
	return m, nil
}

// docSize returns the remaining length of doc, or -1 if unknown.
func docSize(doc io.Reader) int64 {
	switch d := doc.(type) {
	case interface{ Len() int }:
		return int64(d.Len())
	case *os.File:
		fi, err := d.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			return -1
		}
		off, err := d.Seek(0, io.SeekCurrent)
		if err != nil || off > fi.Size() {
			return -1
		}
		return fi.Size() - off
	}
	return -1
}

// attrsOf flattens every group with the given tag into a map.
func attrsOf(m *Message, tag byte) map[string][]any {
	out := map[string][]any{}
	for _, g := range m.Groups {
		if g.Tag != tag {
			continue
		}
		for _, a := range g.Attrs {
			out[a.Name] = append(out[a.Name], a.Values...)
		}
	}
	return out
}

// GetPrinterAttributes fetches the named printer attributes (all if none).
func (c *Client) GetPrinterAttributes(ctx context.Context, names ...string) (map[string][]any, error) {
	var extra []Attr
	if len(names) > 0 {
		vals := make([]any, len(names))
		for i, n := range names {
			vals[i] = n
		}
		extra = append(extra, Attr{Name: "requested-attributes", Tag: TagKeyword, Values: vals})
	}
	m, err := c.do(ctx, c.httpClient(), c.request(OpGetPrinterAttributes, extra...), nil)
	if err != nil {
		return nil, err
	}
	return attrsOf(m, TagPrinter), nil
}

// PrintJob streams doc to the printer with the given job template attributes.
// The HTTP client timeout is not applied (large documents); use ctx instead.
func (c *Client) PrintJob(ctx context.Context, doc io.Reader, format, jobName string, attrs []Attr) (int, error) {
	if format == "" {
		format = "application/octet-stream"
	}
	var extra []Attr
	if jobName != "" {
		extra = append(extra, Attr{Name: "job-name", Tag: TagName, Values: []any{jobName}})
	}
	extra = append(extra, Attr{Name: "document-format", Tag: TagMimeType, Values: []any{format}})
	req := c.request(OpPrintJob, extra...)
	if len(attrs) > 0 {
		req.Groups = append(req.Groups, Group{Tag: TagJob, Attrs: attrs})
	}
	hc := *c.httpClient()
	hc.Timeout = 0
	m, err := c.do(ctx, &hc, req, doc)
	if err != nil {
		return 0, err
	}
	id, ok := firstInt(attrsOf(m, TagJob)["job-id"])
	if !ok {
		return 0, errors.New("ipp: Print-Job: response has no job-id")
	}
	return id, nil
}

// GetJobAttributes returns the job's attributes (job-state, job-state-reasons...).
func (c *Client) GetJobAttributes(ctx context.Context, jobID int) (map[string][]any, error) {
	m, err := c.do(ctx, c.httpClient(), c.request(OpGetJobAttributes, jobIDAttr(jobID)), nil)
	if err != nil {
		return nil, err
	}
	return attrsOf(m, TagJob), nil
}

// CancelJob cancels a job.
func (c *Client) CancelJob(ctx context.Context, jobID int) error {
	_, err := c.do(ctx, c.httpClient(), c.request(OpCancelJob, jobIDAttr(jobID)), nil)
	return err
}

func jobIDAttr(id int) Attr {
	return Attr{Name: "job-id", Tag: TagInteger, Values: []any{id}}
}

func firstInt(v []any) (int, bool) {
	if len(v) == 0 {
		return 0, false
	}
	n, ok := v[0].(int32)
	return int(n), ok
}
