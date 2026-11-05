package ipp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// raw builds IPP bytes by hand so tests don't depend on the encoder.
type raw struct{ bytes.Buffer }

func (b *raw) header(major, minor byte, code uint16, id uint32) *raw {
	b.WriteByte(major)
	b.WriteByte(minor)
	b.Write(binary.BigEndian.AppendUint16(nil, code))
	b.Write(binary.BigEndian.AppendUint32(nil, id))
	return b
}

func (b *raw) group(tag byte) *raw { b.WriteByte(tag); return b }

func (b *raw) attr(tag byte, name string, val []byte) *raw {
	b.WriteByte(tag)
	b.Write(binary.BigEndian.AppendUint16(nil, uint16(len(name))))
	b.WriteString(name)
	b.Write(binary.BigEndian.AppendUint16(nil, uint16(len(val))))
	b.Write(val)
	return b
}

func i32(v int32) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }

func TestEncodeGetPrinterAttributesRequest(t *testing.T) {
	m := &Message{
		Version:   [2]byte{2, 0},
		Code:      OpGetPrinterAttributes,
		RequestID: 7,
		Groups: []Group{{Tag: TagOperation, Attrs: []Attr{
			{Name: "attributes-charset", Tag: TagCharset, Values: []any{"utf-8"}},
			{Name: "attributes-natural-language", Tag: TagLanguage, Values: []any{"en"}},
			{Name: "printer-uri", Tag: TagURI, Values: []any{"ipp://printer.local/ipp/print"}},
			{Name: "requesting-user-name", Tag: TagName, Values: []any{"printer-web"}},
			{Name: "requested-attributes", Tag: TagKeyword, Values: []any{"printer-name", "sides-supported"}},
		}}},
	}
	want := new(raw).header(2, 0, 0x000B, 7).group(0x01).
		attr(0x47, "attributes-charset", []byte("utf-8")).
		attr(0x48, "attributes-natural-language", []byte("en")).
		attr(0x45, "printer-uri", []byte("ipp://printer.local/ipp/print")).
		attr(0x42, "requesting-user-name", []byte("printer-web")).
		attr(0x44, "requested-attributes", []byte("printer-name")).
		attr(0x44, "", []byte("sides-supported")).
		group(0x03)
	got := m.Encode()
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("encode mismatch:\n got %x\nwant %x", got, want.Bytes())
	}
	back, doc, err := Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) != 0 {
		t.Fatalf("unexpected doc %q", doc)
	}
	if !reflect.DeepEqual(back, m) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", back, m)
	}
}

// cannedResponse is a Get-Printer-Attributes response with collections.
func cannedResponse() []byte {
	b := new(raw).header(2, 0, 0x0000, 42).group(0x01).
		attr(0x47, "attributes-charset", []byte("utf-8")).
		attr(0x48, "attributes-natural-language", []byte("en")).
		group(0x04).
		attr(0x42, "printer-name", []byte("Office")).
		attr(0x44, "sides-supported", []byte("one-sided")).
		attr(0x44, "", []byte("two-sided-long-edge")).
		attr(0x44, "", []byte("two-sided-short-edge")).
		attr(0x33, "copies-supported", append(i32(1), i32(99)...)).
		attr(0x32, "printer-resolution-supported", append(append(i32(300), i32(300)...), 3)).
		attr(0x32, "", append(append(i32(600), i32(1200)...), 3)).
		// media-col-default { media-size { x-dimension, y-dimension }, media-type }
		attr(0x34, "media-col-default", nil).
		attr(0x4A, "", []byte("media-size")).
		attr(0x34, "", nil).
		attr(0x4A, "", []byte("x-dimension")).
		attr(0x21, "", i32(21000)).
		attr(0x4A, "", []byte("y-dimension")).
		attr(0x21, "", i32(29700)).
		attr(0x37, "", nil).
		attr(0x4A, "", []byte("media-type")).
		attr(0x44, "", []byte("stationery")).
		attr(0x37, "", nil).
		// 1setOf collection: two empty collections
		attr(0x34, "media-col-ready", nil).
		attr(0x37, "", nil).
		attr(0x34, "", nil).
		attr(0x37, "", nil).
		attr(0x23, "printer-state", i32(3)).
		attr(0x22, "printer-is-accepting-jobs", []byte{1}).
		attr(0x13, "printer-alert", nil).
		attr(0x31, "printer-current-time", []byte{0x07, 0xEA, 11, 5, 10, 0, 0, 0, '+', 1, 0}).
		attr(0x41, "printer-info", []byte("Second floor")).
		attr(0x49, "document-format-supported", []byte("application/pdf")).
		attr(0x30, "printer-firmware-version", []byte{1, 2}).
		group(0x03)
	return b.Bytes()
}

func TestDecodeCannedResponse(t *testing.T) {
	m, doc, err := Decode(bytes.NewReader(cannedResponse()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != [2]byte{2, 0} || m.Code != 0 || m.RequestID != 42 || len(doc) != 0 {
		t.Fatalf("bad header: %+v doc=%q", m, doc)
	}
	if len(m.Groups) != 2 || m.Groups[0].Tag != TagOperation || m.Groups[1].Tag != TagPrinter {
		t.Fatalf("bad groups: %+v", m.Groups)
	}
	got := map[string]Attr{}
	for _, a := range m.Groups[1].Attrs {
		got[a.Name] = a
	}
	check := func(name string, tag byte, want ...any) {
		t.Helper()
		a, ok := got[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if a.Tag != tag || !reflect.DeepEqual(a.Values, want) {
			t.Fatalf("%s: got tag %#x %#v, want tag %#x %#v", name, a.Tag, a.Values, tag, want)
		}
	}
	check("printer-name", TagName, "Office")
	check("sides-supported", TagKeyword, "one-sided", "two-sided-long-edge", "two-sided-short-edge")
	check("copies-supported", TagRange, Range{Lo: 1, Hi: 99})
	check("printer-resolution-supported", TagResolution,
		Resolution{X: 300, Y: 300, Units: 3}, Resolution{X: 600, Y: 1200, Units: 3})
	check("media-col-default", TagBeginCollection, nil)
	check("media-col-ready", TagBeginCollection, nil, nil)
	check("printer-state", TagEnum, int32(3))
	check("printer-is-accepting-jobs", TagBoolean, true)
	check("printer-alert", TagNoValue, nil)
	check("printer-current-time", TagDateTime, []byte{0x07, 0xEA, 11, 5, 10, 0, 0, 0, '+', 1, 0})
	check("printer-info", TagText, "Second floor")
	check("document-format-supported", TagMimeType, "application/pdf")
	check("printer-firmware-version", TagOctetString, []byte{1, 2})
	if len(m.Groups[1].Attrs) != 13 {
		t.Fatalf("want 13 attrs, got %d", len(m.Groups[1].Attrs))
	}
}

func TestRoundTripAllTypes(t *testing.T) {
	m := &Message{
		Version:   [2]byte{2, 0},
		Code:      OpPrintJob,
		RequestID: 1,
		Groups: []Group{
			{Tag: TagOperation, Attrs: []Attr{
				{Name: "attributes-charset", Tag: TagCharset, Values: []any{"utf-8"}},
				{Name: "attributes-natural-language", Tag: TagLanguage, Values: []any{"en"}},
				{Name: "document-format", Tag: TagMimeType, Values: []any{"application/pdf"}},
			}},
			{Tag: TagJob, Attrs: []Attr{
				{Name: "copies", Tag: TagInteger, Values: []any{int32(2)}},
				{Name: "orientation-requested", Tag: TagEnum, Values: []any{int32(4)}},
				{Name: "fit", Tag: TagBoolean, Values: []any{false}},
				{Name: "sides", Tag: TagKeyword, Values: []any{"two-sided-long-edge"}},
				{Name: "page-ranges", Tag: TagRange, Values: []any{Range{1, 3}, Range{5, 5}}},
				{Name: "printer-resolution", Tag: TagResolution, Values: []any{Resolution{600, 600, 3}}},
				{Name: "job-name", Tag: TagName, Values: []any{"Fête.pdf"}},
				{Name: "opaque", Tag: TagOctetString, Values: []any{[]byte{0, 1, 2}}},
				{Name: "when", Tag: TagDateTime, Values: []any{[]byte{7, 234, 1, 1, 0, 0, 0, 0, '+', 0, 0}}},
				{Name: "scheme", Tag: TagURIScheme, Values: []any{"ipp"}},
				{Name: "info", Tag: TagText, Values: []any{"hello"}},
				{Name: "gone", Tag: TagUnknown, Values: []any{nil}},
				{Name: "unsup", Tag: TagUnsupportedValue, Values: []any{nil}},
			}},
		},
	}
	enc, err := m.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.4 data")
	back, doc, err := Decode(io.MultiReader(bytes.NewReader(enc), bytes.NewReader(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, payload) {
		t.Fatalf("doc = %q", doc)
	}
	// Encode accepts int for integers; decode always gives int32.
	if !reflect.DeepEqual(back, m) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", back, m)
	}
}

func TestEncodeAcceptsInt(t *testing.T) {
	m := &Message{Version: [2]byte{2, 0}, Groups: []Group{{Tag: TagJob, Attrs: []Attr{
		{Name: "copies", Tag: TagInteger, Values: []any{3}},
	}}}}
	back, _, err := Decode(bytes.NewReader(m.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if v := back.Groups[0].Attrs[0].Values[0]; v != int32(3) {
		t.Fatalf("got %#v", v)
	}
}

func TestMarshalErrors(t *testing.T) {
	cases := map[string]Attr{
		"string as integer":  {Name: "copies", Tag: TagInteger, Values: []any{"2"}},
		"int overflow":       {Name: "copies", Tag: TagInteger, Values: []any{1 << 40}},
		"int as keyword":     {Name: "sides", Tag: TagKeyword, Values: []any{2}},
		"no values":          {Name: "sides", Tag: TagKeyword},
		"empty name":         {Name: "", Tag: TagKeyword, Values: []any{"x"}},
		"collection":         {Name: "media-col", Tag: TagBeginCollection, Values: []any{nil}},
		"value too long":     {Name: "x", Tag: TagText, Values: []any{strings.Repeat("a", 70000)}},
		"value for no-value": {Name: "x", Tag: TagNoValue, Values: []any{"a"}},
	}
	for name, a := range cases {
		m := &Message{Groups: []Group{{Tag: TagJob, Attrs: []Attr{a}}}}
		if _, err := m.MarshalBinary(); err == nil {
			t.Errorf("%s: want error", name)
		}
		if b := m.Encode(); b != nil {
			t.Errorf("%s: Encode should return nil on error", name)
		}
	}
	bad := &Message{Groups: []Group{{Tag: 0x20}}}
	if _, err := bad.MarshalBinary(); err == nil {
		t.Error("bad group tag: want error")
	}
}

func TestDecodeTruncated(t *testing.T) {
	full := cannedResponse()
	for n := 0; n < len(full); n++ {
		if _, _, err := Decode(bytes.NewReader(full[:n])); err == nil {
			t.Fatalf("prefix %d/%d: want error", n, len(full))
		}
	}
}

func TestDecodeMalformed(t *testing.T) {
	hdr := func() *raw { return new(raw).header(2, 0, 0, 1) }
	cases := map[string][]byte{
		"attr before group":    hdr().attr(0x44, "x", []byte("y")).group(0x03).Bytes(),
		"extra value no attr":  hdr().group(0x04).attr(0x44, "", []byte("y")).group(0x03).Bytes(),
		"bad integer length":   hdr().group(0x04).attr(0x21, "x", []byte{1, 2}).group(0x03).Bytes(),
		"bad boolean length":   hdr().group(0x04).attr(0x22, "x", nil).group(0x03).Bytes(),
		"bad range length":     hdr().group(0x04).attr(0x33, "x", i32(1)).group(0x03).Bytes(),
		"bad resolution":       hdr().group(0x04).attr(0x32, "x", i32(1)).group(0x03).Bytes(),
		"stray end collection": hdr().group(0x04).attr(0x37, "x", nil).group(0x03).Bytes(),
		"stray member name":    hdr().group(0x04).attr(0x4A, "x", []byte("m")).group(0x03).Bytes(),
		"reserved delimiter":   hdr().group(0x00).group(0x03).Bytes(),
		"delimiter in collection": hdr().group(0x04).attr(0x34, "c", nil).
			group(0x03).Bytes(),
		"unterminated collection": hdr().group(0x04).attr(0x34, "c", nil).
			attr(0x4A, "", []byte("m")).Bytes(),
	}
	for name, b := range cases {
		if _, _, err := Decode(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func nested(depth int) []byte {
	b := new(raw).header(2, 0, 0, 1).group(0x04).attr(0x34, "c", nil)
	for i := 1; i < depth; i++ {
		b.attr(0x4A, "", []byte("m")).attr(0x34, "", nil)
	}
	for i := 0; i < depth; i++ {
		b.attr(0x37, "", nil)
	}
	return b.group(0x03).Bytes()
}

func TestDecodeCollectionDepth(t *testing.T) {
	if _, _, err := Decode(bytes.NewReader(nested(16))); err != nil {
		t.Fatalf("depth 16: %v", err)
	}
	if _, _, err := Decode(bytes.NewReader(nested(17))); err == nil {
		t.Fatal("depth 17: want error")
	}
}

// endless yields a valid header then attributes forever.
type endless struct {
	pre []byte
	rec []byte
	off int
}

func (e *endless) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(e.pre) > 0 {
			c := copy(p[n:], e.pre)
			e.pre = e.pre[c:]
			n += c
			continue
		}
		c := copy(p[n:], e.rec[e.off:])
		e.off = (e.off + c) % len(e.rec)
		n += c
	}
	return n, nil
}

func TestDecodeMessageSizeCap(t *testing.T) {
	r := &endless{
		pre: new(raw).header(2, 0, 0, 1).group(0x04).Bytes(),
		rec: new(raw).attr(0x41, "x", bytes.Repeat([]byte("a"), 60000)).Bytes(),
	}
	_, _, err := Decode(r)
	if err == nil || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestDecodeRandomFlipsNeverPanic(t *testing.T) {
	base := cannedResponse()
	rng := rand.New(rand.NewSource(1))
	buf := make([]byte, len(base))
	for i := 0; i < 20000; i++ {
		copy(buf, base)
		for f := rng.Intn(4) + 1; f > 0; f-- {
			buf[rng.Intn(len(buf))] = byte(rng.Intn(256))
		}
		n := len(buf)
		if rng.Intn(4) == 0 {
			n = rng.Intn(len(buf) + 1)
		}
		_, _, _ = Decode(bytes.NewReader(buf[:n]))
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(cannedResponse())
	f.Add(nested(16))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, _, err := Decode(bytes.NewReader(b))
		if err != nil {
			return
		}
		// Whatever decodes must not panic when re-encoded.
		_, _ = m.MarshalBinary()
	})
}

func TestHTTPURL(t *testing.T) {
	cases := map[string]string{
		"ipp://printer.local/ipp/print":      "http://printer.local:631/ipp/print",
		"ipp://192.168.1.50:8631/ipp/print":  "http://192.168.1.50:8631/ipp/print",
		"ipps://printer.local/ipp/print":     "https://printer.local:631/ipp/print",
		"ipps://printer.local:443/ipp/print": "https://printer.local:443/ipp/print",
		"ipp://[fe80::1]/ipp/print":          "http://[fe80::1]:631/ipp/print",
		"ipp://printer.local":                "http://printer.local:631/",
		"http://printer.local:631/ipp/print": "http://printer.local:631/ipp/print",
		"https://printer.local/ipp/print":    "https://printer.local/ipp/print",
	}
	for in, want := range cases {
		got, err := HTTPURL(in)
		if err != nil || got != want {
			t.Errorf("HTTPURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "lpd://printer.local/q", "ipp:///ipp/print", "::bad"} {
		if _, err := HTTPURL(bad); err == nil {
			t.Errorf("HTTPURL(%q): want error", bad)
		}
	}
}

func TestStatusName(t *testing.T) {
	if StatusName(0x0507) != "server-error-busy" || StatusName(0x040A) != "client-error-document-format-not-supported" {
		t.Fatal("bad names")
	}
	if StatusName(0x04FF) != "client-error-0x04ff" || StatusName(0x05FF) != "server-error-0x05ff" {
		t.Fatalf("bad fallback: %s %s", StatusName(0x04FF), StatusName(0x05FF))
	}
}
