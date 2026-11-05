// Package ipp is a small IPP/2.0 codec (RFC 8010) and client (RFC 8011).
package ipp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// Delimiter (group) tags.
const (
	TagOperation        byte = 0x01
	TagJob              byte = 0x02
	TagEnd              byte = 0x03
	TagPrinter          byte = 0x04
	TagUnsupportedGroup byte = 0x05
)

// Value tags.
const (
	TagUnsupportedValue byte = 0x10 // out-of-band
	TagUnknown          byte = 0x12 // out-of-band
	TagNoValue          byte = 0x13 // out-of-band
	TagInteger          byte = 0x21
	TagBoolean          byte = 0x22
	TagEnum             byte = 0x23
	TagOctetString      byte = 0x30
	TagDateTime         byte = 0x31
	TagResolution       byte = 0x32
	TagRange            byte = 0x33
	TagBeginCollection  byte = 0x34
	TagTextLang         byte = 0x35
	TagNameLang         byte = 0x36
	TagEndCollection    byte = 0x37
	TagText             byte = 0x41
	TagName             byte = 0x42
	TagKeyword          byte = 0x44
	TagURI              byte = 0x45
	TagURIScheme        byte = 0x46
	TagCharset          byte = 0x47
	TagLanguage         byte = 0x48
	TagMimeType         byte = 0x49
	TagMemberName       byte = 0x4A
	TagExtension        byte = 0x7F
)

// Operation ids.
const (
	OpPrintJob             uint16 = 0x0002
	OpCancelJob            uint16 = 0x0008
	OpGetJobAttributes     uint16 = 0x0009
	OpGetPrinterAttributes uint16 = 0x000B
)

// Job states (job-state enum).
const (
	JobPending           = 3
	JobPendingHeld       = 4
	JobProcessing        = 5
	JobProcessingStopped = 6
	JobCanceled          = 7
	JobAborted           = 8
	JobCompleted         = 9
)

// Range is a rangeOfInteger value.
type Range struct{ Lo, Hi int32 }

// Resolution is a resolution value; Units is 3 (dots per inch) or 4 (dots per cm).
type Resolution struct {
	X, Y  int32
	Units int8
}

// Attr is one attribute. Values hold string, int32, bool, []byte, Range,
// Resolution or nil (out-of-band values and decoded collections).
// Encoding also accepts int for integer/enum and string for octetString.
type Attr struct {
	Name   string
	Tag    byte
	Values []any
}

// Group is an attribute group (operation, job, printer...).
type Group struct {
	Tag   byte
	Attrs []Attr
}

// Message is an IPP request or response. Code is the operation id or status code.
type Message struct {
	Version   [2]byte
	Code      uint16
	RequestID uint32
	Groups    []Group
}

// Encode returns the wire form, or nil if an attribute is invalid
// (use MarshalBinary to get the error).
func (m *Message) Encode() []byte {
	b, err := m.MarshalBinary()
	if err != nil {
		return nil
	}
	return b
}

// MarshalBinary returns the wire form, ending with the end-of-attributes tag.
func (m *Message) MarshalBinary() ([]byte, error) {
	var b bytes.Buffer
	b.Write(m.Version[:])
	b.Write(binary.BigEndian.AppendUint16(nil, m.Code))
	b.Write(binary.BigEndian.AppendUint32(nil, m.RequestID))
	for _, g := range m.Groups {
		if g.Tag == 0 || g.Tag == TagEnd || g.Tag > 0x0F {
			return nil, fmt.Errorf("ipp: invalid group tag %#x", g.Tag)
		}
		b.WriteByte(g.Tag)
		for _, a := range g.Attrs {
			if err := writeAttr(&b, a); err != nil {
				return nil, err
			}
		}
	}
	b.WriteByte(TagEnd)
	return b.Bytes(), nil
}

func writeAttr(b *bytes.Buffer, a Attr) error {
	if a.Name == "" || len(a.Name) > math.MaxUint16 {
		return fmt.Errorf("ipp: invalid attribute name %q", a.Name)
	}
	if len(a.Values) == 0 {
		return fmt.Errorf("ipp: attribute %s has no value", a.Name)
	}
	for i, v := range a.Values {
		val, err := encodeValue(a.Tag, v)
		if err != nil {
			return fmt.Errorf("ipp: attribute %s: %w", a.Name, err)
		}
		if len(val) > math.MaxUint16 {
			return fmt.Errorf("ipp: attribute %s: value too long", a.Name)
		}
		name := a.Name
		if i > 0 {
			name = "" // additional value of a 1setOf
		}
		b.WriteByte(a.Tag)
		b.Write(binary.BigEndian.AppendUint16(nil, uint16(len(name))))
		b.WriteString(name)
		b.Write(binary.BigEndian.AppendUint16(nil, uint16(len(val))))
		b.Write(val)
	}
	return nil
}

func encodeValue(tag byte, v any) ([]byte, error) {
	switch {
	case tag <= 0x0F || tag == TagExtension:
		return nil, fmt.Errorf("invalid value tag %#x", tag)
	case tag <= 0x1F: // out-of-band
		if v != nil {
			return nil, fmt.Errorf("out-of-band tag %#x takes no value", tag)
		}
		return nil, nil
	case tag == TagBeginCollection || tag == TagEndCollection || tag == TagMemberName:
		return nil, fmt.Errorf("collections are not supported")
	}
	switch tag {
	case TagInteger, TagEnum:
		var n int64
		switch x := v.(type) {
		case int32:
			n = int64(x)
		case int:
			n = int64(x)
		default:
			return nil, fmt.Errorf("want int32, got %T", v)
		}
		if n < math.MinInt32 || n > math.MaxInt32 {
			return nil, fmt.Errorf("integer %d out of range", n)
		}
		return binary.BigEndian.AppendUint32(nil, uint32(int32(n))), nil
	case TagBoolean:
		x, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("want bool, got %T", v)
		}
		if x {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case TagResolution:
		x, ok := v.(Resolution)
		if !ok {
			return nil, fmt.Errorf("want Resolution, got %T", v)
		}
		out := binary.BigEndian.AppendUint32(nil, uint32(x.X))
		out = binary.BigEndian.AppendUint32(out, uint32(x.Y))
		return append(out, byte(x.Units)), nil
	case TagRange:
		x, ok := v.(Range)
		if !ok {
			return nil, fmt.Errorf("want Range, got %T", v)
		}
		out := binary.BigEndian.AppendUint32(nil, uint32(x.Lo))
		return binary.BigEndian.AppendUint32(out, uint32(x.Hi)), nil
	}
	switch x := v.(type) {
	case []byte:
		return x, nil
	case string:
		if tag >= 0x40 && tag <= 0x5F || tag == TagOctetString {
			return []byte(x), nil
		}
	}
	return nil, fmt.Errorf("unsupported value %T for tag %#x", v, tag)
}
