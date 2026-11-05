package ipp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Decoding limits, so malformed input can't exhaust memory.
const (
	maxValueLen   = 64 << 10 // per attribute value (also bounded by the uint16 length)
	maxMessageLen = 4 << 20  // header + attributes, document excluded
	maxDepth      = 16       // collection nesting
)

// ErrTooLarge is returned when the attribute section exceeds 4 MiB.
var ErrTooLarge = errors.New("ipp: message too large")

var errTruncated = errors.New("ipp: truncated message")

type decoder struct {
	r *bufio.Reader
	n int // bytes consumed so far
}

func (d *decoder) full(p []byte) error {
	if d.n+len(p) > maxMessageLen {
		return ErrTooLarge
	}
	if _, err := io.ReadFull(d.r, p); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return errTruncated
		}
		return err
	}
	d.n += len(p)
	return nil
}

func (d *decoder) byte() (byte, error) {
	var b [1]byte
	err := d.full(b[:])
	return b[0], err
}

func (d *decoder) u16() (int, error) {
	var b [2]byte
	if err := d.full(b[:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(b[:])), nil
}

// entry reads name-length, name, value-length and value (the tag is already read).
func (d *decoder) entry() (string, []byte, error) {
	nl, err := d.u16()
	if err != nil {
		return "", nil, err
	}
	name := make([]byte, nl)
	if err := d.full(name); err != nil {
		return "", nil, err
	}
	vl, err := d.u16()
	if err != nil {
		return "", nil, err
	}
	if vl > maxValueLen {
		return "", nil, fmt.Errorf("ipp: value too long (%d bytes)", vl)
	}
	val := make([]byte, vl)
	if err := d.full(val); err != nil {
		return "", nil, err
	}
	return string(name), val, nil
}

// Decode reads one IPP message and returns it with the data that follows the
// end-of-attributes tag (the document of a Print-Job request). Callers should
// bound r when the trailing data is untrusted.
func Decode(r io.Reader) (*Message, []byte, error) {
	d := &decoder{r: bufio.NewReader(r)}
	var hdr [8]byte
	if err := d.full(hdr[:]); err != nil {
		return nil, nil, err
	}
	m := &Message{
		Version:   [2]byte{hdr[0], hdr[1]},
		Code:      binary.BigEndian.Uint16(hdr[2:4]),
		RequestID: binary.BigEndian.Uint32(hdr[4:8]),
	}
	for {
		tag, err := d.byte()
		if err != nil {
			return nil, nil, err
		}
		if tag <= 0x0F { // delimiter
			if tag == TagEnd {
				break
			}
			if tag == 0 {
				return nil, nil, errors.New("ipp: reserved delimiter tag 0x00")
			}
			m.Groups = append(m.Groups, Group{Tag: tag})
			continue
		}
		if len(m.Groups) == 0 {
			return nil, nil, errors.New("ipp: attribute outside of a group")
		}
		name, val, err := d.entry()
		if err != nil {
			return nil, nil, err
		}
		var v any
		switch tag {
		case TagBeginCollection:
			if err := d.skipCollection(); err != nil {
				return nil, nil, err
			}
		case TagEndCollection, TagMemberName:
			return nil, nil, fmt.Errorf("ipp: unexpected collection tag %#x", tag)
		default:
			if v, err = decodeValue(tag, val); err != nil {
				return nil, nil, fmt.Errorf("ipp: attribute %q: %w", name, err)
			}
		}
		g := &m.Groups[len(m.Groups)-1]
		if name == "" {
			if len(g.Attrs) == 0 {
				return nil, nil, errors.New("ipp: additional value without attribute")
			}
			last := &g.Attrs[len(g.Attrs)-1]
			last.Values = append(last.Values, v)
			continue
		}
		g.Attrs = append(g.Attrs, Attr{Name: name, Tag: tag, Values: []any{v}})
	}
	doc, err := io.ReadAll(d.r)
	if err != nil {
		return nil, nil, err
	}
	return m, doc, nil
}

// skipCollection consumes members up to the matching endCollection.
func (d *decoder) skipCollection() error {
	depth := 1
	for depth > 0 {
		tag, err := d.byte()
		if err != nil {
			return err
		}
		if tag <= 0x0F {
			return errors.New("ipp: delimiter inside a collection")
		}
		if _, _, err := d.entry(); err != nil {
			return err
		}
		switch tag {
		case TagBeginCollection:
			depth++
			if depth > maxDepth {
				return errors.New("ipp: collections nested too deeply")
			}
		case TagEndCollection:
			depth--
		}
	}
	return nil
}

func decodeValue(tag byte, b []byte) (any, error) {
	want := func(n int) error {
		if len(b) != n {
			return fmt.Errorf("tag %#x: want %d bytes, got %d", tag, n, len(b))
		}
		return nil
	}
	switch {
	case tag <= 0x1F: // out-of-band
		return nil, nil
	case tag == TagInteger || tag == TagEnum:
		if err := want(4); err != nil {
			return nil, err
		}
		return int32(binary.BigEndian.Uint32(b)), nil
	case tag == TagBoolean:
		if err := want(1); err != nil {
			return nil, err
		}
		return b[0] != 0, nil
	case tag == TagResolution:
		if err := want(9); err != nil {
			return nil, err
		}
		return Resolution{
			X:     int32(binary.BigEndian.Uint32(b[0:4])),
			Y:     int32(binary.BigEndian.Uint32(b[4:8])),
			Units: int8(b[8]),
		}, nil
	case tag == TagRange:
		if err := want(8); err != nil {
			return nil, err
		}
		return Range{
			Lo: int32(binary.BigEndian.Uint32(b[0:4])),
			Hi: int32(binary.BigEndian.Uint32(b[4:8])),
		}, nil
	case tag >= 0x40 && tag <= 0x5F: // character-string types
		return string(b), nil
	default: // octetString, dateTime, *WithLanguage, unknown: raw bytes
		return b, nil
	}
}
