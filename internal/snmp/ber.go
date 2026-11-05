// Package snmp is a minimal SNMPv1 client: Get and Walk (GetNext) over UDP.
package snmp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ASN.1 / SNMP tags.
const (
	tagInteger     = 0x02
	tagOctetString = 0x04
	tagNull        = 0x05
	tagOID         = 0x06
	tagSequence    = 0x30
)

// Type is the type of a variable value (its BER tag).
type Type byte

// Value types.
const (
	TypeInteger     Type = tagInteger
	TypeOctetString Type = tagOctetString
	TypeNull        Type = tagNull
	TypeOID         Type = tagOID
	TypeIPAddress   Type = 0x40
	TypeCounter32   Type = 0x41
	TypeGauge32     Type = 0x42
	TypeTimeTicks   Type = 0x43
	// SNMPv2 exceptions, accepted when decoding.
	TypeNoSuchObject   Type = 0x80
	TypeNoSuchInstance Type = 0x81
	TypeEndOfMibView   Type = 0x82
	// TypeUnknown marks an opaque value (see Value.Raw).
	TypeUnknown Type = 0xff
)

// PDU types.
const (
	PDUGetRequest     byte = 0xa0
	PDUGetNextRequest byte = 0xa1
	PDUGetResponse    byte = 0xa2
)

// Limits that keep a hostile packet from costing much.
const (
	maxOIDArcs   = 128
	maxVarBinds  = 512
	maxPacketLen = 65535
)

// Value is a decoded variable value. Int holds INTEGER, Counter32, Gauge32
// and TimeTicks; Bytes holds OCTET STRING and IpAddress; OID holds an OID.
// A value of an unsupported type, or a malformed one, is kept opaque: Type
// is TypeUnknown, Raw holds its whole TLV and Bytes its content.
type Value struct {
	Type  Type
	Int   int64
	Bytes []byte
	OID   string
	Raw   []byte
}

// String returns the value as text.
func (v Value) String() string {
	switch v.Type {
	case TypeOctetString:
		return string(v.Bytes)
	case TypeIPAddress:
		if len(v.Bytes) == 4 {
			return fmt.Sprintf("%d.%d.%d.%d", v.Bytes[0], v.Bytes[1], v.Bytes[2], v.Bytes[3])
		}
		return ""
	case TypeOID:
		return v.OID
	case TypeInteger, TypeCounter32, TypeGauge32, TypeTimeTicks:
		return strconv.FormatInt(v.Int, 10)
	}
	return ""
}

// IsException reports whether the value is a v2 noSuchObject, noSuchInstance
// or endOfMibView.
func (v Value) IsException() bool {
	return v.Type == TypeNoSuchObject || v.Type == TypeNoSuchInstance || v.Type == TypeEndOfMibView
}

// Var is one variable binding.
type Var struct {
	OID   string
	Value Value
}

// Packet is an SNMP message with its PDU.
type Packet struct {
	Version     int64
	Community   string
	PDUType     byte
	RequestID   int32
	ErrorStatus int64
	ErrorIndex  int64
	Vars        []Var
}

var errShort = errors.New("snmp: truncated packet")

// ParseOID parses a dotted OID ("1.3.6.1", a leading dot is allowed).
func ParseOID(s string) ([]uint32, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), ".")
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > maxOIDArcs {
		return nil, fmt.Errorf("snmp: invalid OID %q", s)
	}
	out := make([]uint32, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("snmp: invalid OID %q", s)
		}
		out[i] = uint32(n)
	}
	if out[0] > 2 || (out[0] < 2 && out[1] >= 40) {
		return nil, fmt.Errorf("snmp: invalid OID %q", s)
	}
	return out, nil
}

func formatOID(arcs []uint32) string {
	var b strings.Builder
	for i, a := range arcs {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.FormatUint(uint64(a), 10))
	}
	return b.String()
}

// CompareOIDs orders two OIDs lexicographically by arc; invalid OIDs sort
// first.
func CompareOIDs(a, b string) int {
	x, _ := ParseOID(a)
	y, _ := ParseOID(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return len(x) - len(y)
}

// ---------- encoding ----------

func appendLength(b []byte, n int) []byte {
	switch {
	case n < 0x80:
		return append(b, byte(n))
	case n <= 0xff:
		return append(b, 0x81, byte(n))
	case n <= 0xffff:
		return append(b, 0x82, byte(n>>8), byte(n))
	}
	return append(b, 0x83, byte(n>>16), byte(n>>8), byte(n))
}

func appendTLV(b []byte, tag byte, content []byte) []byte {
	b = append(b, tag)
	b = appendLength(b, len(content))
	return append(b, content...)
}

// intBytes is the minimal two's complement form of n.
func intBytes(n int64) []byte {
	var out []byte
	for {
		out = append([]byte{byte(n)}, out...)
		if (n >= -128 && n <= 127) || len(out) == 8 {
			return out
		}
		n >>= 8
	}
}

// uintBytes is the unsigned form of n, with a leading 0 when the high bit
// is set.
func uintBytes(n uint32) []byte { return intBytes(int64(n)) }

func oidBytes(s string) ([]byte, error) {
	arcs, err := ParseOID(s)
	if err != nil {
		return nil, err
	}
	out := appendBase128(nil, uint64(arcs[0])*40+uint64(arcs[1]))
	for _, a := range arcs[2:] {
		out = appendBase128(out, uint64(a))
	}
	return out, nil
}

func appendBase128(b []byte, n uint64) []byte {
	var tmp [10]byte
	i := len(tmp) - 1
	tmp[i] = byte(n & 0x7f)
	for n >>= 7; n > 0; n >>= 7 {
		i--
		tmp[i] = byte(n&0x7f) | 0x80
	}
	return append(b, tmp[i:]...)
}

func encodeValue(v Value) ([]byte, error) {
	if v.Type == TypeUnknown {
		if _, _, rest, err := readTLV(v.Raw); err != nil || len(rest) != 0 {
			return nil, errors.New("snmp: opaque value is not one TLV")
		}
		return v.Raw, nil
	}
	switch v.Type {
	case TypeNull, 0:
		return []byte{tagNull, 0}, nil
	case TypeInteger:
		return appendTLV(nil, tagInteger, intBytes(v.Int)), nil
	case TypeOctetString, TypeIPAddress:
		return appendTLV(nil, byte(v.Type), v.Bytes), nil
	case TypeCounter32, TypeGauge32, TypeTimeTicks:
		return appendTLV(nil, byte(v.Type), uintBytes(uint32(v.Int))), nil
	case TypeOID:
		o, err := oidBytes(v.OID)
		if err != nil {
			return nil, err
		}
		return appendTLV(nil, tagOID, o), nil
	case TypeNoSuchObject, TypeNoSuchInstance, TypeEndOfMibView:
		return []byte{byte(v.Type), 0}, nil
	}
	return nil, fmt.Errorf("snmp: cannot encode type 0x%02x", byte(v.Type))
}

// Marshal encodes the packet.
func (p *Packet) Marshal() ([]byte, error) {
	var list []byte
	for _, vb := range p.Vars {
		o, err := oidBytes(vb.OID)
		if err != nil {
			return nil, err
		}
		val, err := encodeValue(vb.Value)
		if err != nil {
			return nil, err
		}
		list = appendTLV(list, tagSequence, append(appendTLV(nil, tagOID, o), val...))
	}
	var pdu []byte
	pdu = appendTLV(pdu, tagInteger, intBytes(int64(p.RequestID)))
	pdu = appendTLV(pdu, tagInteger, intBytes(p.ErrorStatus))
	pdu = appendTLV(pdu, tagInteger, intBytes(p.ErrorIndex))
	pdu = appendTLV(pdu, tagSequence, list)
	var msg []byte
	msg = appendTLV(msg, tagInteger, intBytes(p.Version))
	msg = appendTLV(msg, tagOctetString, []byte(p.Community))
	msg = appendTLV(msg, p.PDUType, pdu)
	out := appendTLV(nil, tagSequence, msg)
	if len(out) > maxPacketLen {
		return nil, errors.New("snmp: packet too large")
	}
	return out, nil
}

// ---------- decoding ----------

// readTLV splits b into one TLV and the rest. Only single-byte tags and
// definite lengths of up to 3 bytes are accepted.
func readTLV(b []byte) (tag byte, content, rest []byte, err error) {
	if len(b) < 2 {
		return 0, nil, nil, errShort
	}
	tag = b[0]
	i := 1
	if tag&0x1f == 0x1f { // multi-byte tag: skipped, tag is its first byte
		for i < len(b) && b[i]&0x80 != 0 {
			i++
		}
		i++
		if i > 5 {
			return 0, nil, nil, errors.New("snmp: tag too long")
		}
		if i >= len(b) {
			return 0, nil, nil, errShort
		}
	}
	n := int(b[i])
	i++
	if n&0x80 != 0 {
		k := n & 0x7f
		if k == 0 || k > 3 {
			return 0, nil, nil, errors.New("snmp: bad length")
		}
		if len(b) < i+k {
			return 0, nil, nil, errShort
		}
		n = 0
		for _, c := range b[i : i+k] {
			n = n<<8 | int(c)
		}
		i += k
	}
	if n > len(b)-i {
		return 0, nil, nil, errShort
	}
	return tag, b[i : i+n], b[i+n:], nil
}

func expect(b []byte, want byte) (content, rest []byte, err error) {
	tag, c, r, err := readTLV(b)
	if err != nil {
		return nil, nil, err
	}
	if tag != want {
		return nil, nil, fmt.Errorf("snmp: tag 0x%02x, want 0x%02x", tag, want)
	}
	return c, r, nil
}

func parseInt(c []byte) (int64, error) {
	if len(c) == 0 || len(c) > 8 {
		return 0, errors.New("snmp: bad integer")
	}
	n := int64(int8(c[0]))
	for _, x := range c[1:] {
		n = n<<8 | int64(x)
	}
	return n, nil
}

func parseUint32(c []byte) (int64, error) {
	if len(c) == 0 || len(c) > 5 || (len(c) == 5 && c[0] != 0) {
		return 0, errors.New("snmp: bad unsigned integer")
	}
	var n int64
	for _, x := range c {
		n = n<<8 | int64(x)
	}
	return n & 0xffffffff, nil
}

func parseOIDBytes(c []byte) (string, error) {
	if len(c) == 0 {
		return "", errors.New("snmp: empty OID")
	}
	var arcs []uint32
	var n uint64
	start := true
	for i, x := range c {
		if start && x == 0x80 {
			return "", errors.New("snmp: bad OID encoding")
		}
		n = n<<7 | uint64(x&0x7f)
		if n > 0xffffffff+80 {
			return "", errors.New("snmp: OID arc overflow")
		}
		start = false
		if x&0x80 != 0 {
			if i == len(c)-1 {
				return "", errors.New("snmp: truncated OID")
			}
			continue
		}
		if arcs == nil {
			switch {
			case n < 40:
				arcs = append(arcs, 0, uint32(n))
			case n < 80:
				arcs = append(arcs, 1, uint32(n-40))
			default:
				if n-80 > 0xffffffff {
					return "", errors.New("snmp: OID arc overflow")
				}
				arcs = append(arcs, 2, uint32(n-80))
			}
		} else {
			if n > 0xffffffff {
				return "", errors.New("snmp: OID arc overflow")
			}
			arcs = append(arcs, uint32(n))
		}
		if len(arcs) > maxOIDArcs {
			return "", errors.New("snmp: OID too long")
		}
		n, start = 0, true
	}
	return formatOID(arcs), nil
}

func decodeValue(tag byte, c []byte) (Value, error) {
	v := Value{Type: Type(tag)}
	var err error
	switch v.Type {
	case TypeInteger:
		v.Int, err = parseInt(c)
	case TypeOctetString:
		v.Bytes = append([]byte(nil), c...)
	case TypeIPAddress:
		if len(c) != 4 {
			return Value{}, errors.New("snmp: bad IpAddress")
		}
		v.Bytes = append([]byte(nil), c...)
	case TypeCounter32, TypeGauge32, TypeTimeTicks:
		v.Int, err = parseUint32(c)
	case TypeOID:
		v.OID, err = parseOIDBytes(c)
	case TypeNull, TypeNoSuchObject, TypeNoSuchInstance, TypeEndOfMibView:
		if len(c) != 0 {
			return Value{}, errors.New("snmp: bad null")
		}
	default:
		return Value{}, fmt.Errorf("snmp: unsupported type 0x%02x", tag)
	}
	return v, err
}

func readInt(b []byte) (int64, []byte, error) {
	c, rest, err := expect(b, tagInteger)
	if err != nil {
		return 0, nil, err
	}
	n, err := parseInt(c)
	return n, rest, err
}

// Unmarshal decodes a packet. It never panics on garbage.
func Unmarshal(b []byte) (*Packet, error) {
	if len(b) > maxPacketLen {
		return nil, errors.New("snmp: packet too large")
	}
	msg, _, err := expect(b, tagSequence)
	if err != nil {
		return nil, err
	}
	p := &Packet{}
	if p.Version, msg, err = readInt(msg); err != nil {
		return nil, err
	}
	comm, msg, err := expect(msg, tagOctetString)
	if err != nil {
		return nil, err
	}
	p.Community = string(comm)
	tag, pdu, _, err := readTLV(msg)
	if err != nil {
		return nil, err
	}
	switch tag {
	case PDUGetRequest, PDUGetNextRequest, PDUGetResponse:
	default:
		return nil, fmt.Errorf("snmp: unsupported PDU 0x%02x", tag)
	}
	p.PDUType = tag
	id, pdu, err := readInt(pdu)
	if err != nil {
		return nil, err
	}
	if id < -1<<31 || id > 1<<31-1 {
		return nil, errors.New("snmp: bad request ID")
	}
	p.RequestID = int32(id)
	if p.ErrorStatus, pdu, err = readInt(pdu); err != nil {
		return nil, err
	}
	if p.ErrorIndex, pdu, err = readInt(pdu); err != nil {
		return nil, err
	}
	list, _, err := expect(pdu, tagSequence)
	if err != nil {
		return nil, err
	}
	for len(list) > 0 {
		if len(p.Vars) >= maxVarBinds {
			return nil, errors.New("snmp: too many variables")
		}
		var vb []byte
		if vb, list, err = expect(list, tagSequence); err != nil {
			return nil, err
		}
		oc, vb, err := expect(vb, tagOID)
		if err != nil {
			return nil, err
		}
		oid, err := parseOIDBytes(oc)
		if err != nil {
			return nil, err
		}
		vt, vc, after, err := readTLV(vb)
		if err != nil {
			return nil, err
		}
		val, err := decodeValue(vt, vc)
		if err != nil { // unsupported or malformed: keep it opaque
			val = Value{Type: TypeUnknown, Bytes: append([]byte(nil), vc...),
				Raw: append([]byte(nil), vb[:len(vb)-len(after)]...)}
		}
		p.Vars = append(p.Vars, Var{OID: oid, Value: val})
	}
	return p, nil
}
