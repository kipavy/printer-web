package snmp_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/kipavy/printer-web/internal/snmp"
	"github.com/kipavy/printer-web/internal/snmp/snmptest"
)

func str(s string) snmp.Value { return snmp.Value{Type: snmp.TypeOctetString, Bytes: []byte(s)} }
func num(n int64) snmp.Value  { return snmp.Value{Type: snmp.TypeInteger, Int: n} }

func TestRoundTrip(t *testing.T) {
	p := &snmp.Packet{
		Version: 0, Community: "public", PDUType: snmp.PDUGetResponse, RequestID: 123456,
		Vars: []snmp.Var{
			{OID: "1.3.6.1.2.1.1.1.0", Value: str("Ricoh")},
			{OID: "1.3.6.1.2.1.43.11.1.1.9.1.1", Value: num(-3)},
			{OID: "1.3.6.1.2.1.43.11.1.1.9.1.2", Value: num(100000)},
			{OID: "1.3.6.1.2.1.1.2.0", Value: snmp.Value{Type: snmp.TypeOID, OID: "1.3.6.1.4.1.367.1.1"}},
			{OID: "1.3.6.1.2.1.1.3.0", Value: snmp.Value{Type: snmp.TypeTimeTicks, Int: 4294967295}},
			{OID: "1.3.6.1.2.1.2.2.1.10.1", Value: snmp.Value{Type: snmp.TypeCounter32, Int: 128}},
			{OID: "1.3.6.1.2.1.2.2.1.5.1", Value: snmp.Value{Type: snmp.TypeGauge32, Int: 1000000000}},
			{OID: "1.3.6.1.2.1.4.20.1.1.1", Value: snmp.Value{Type: snmp.TypeIPAddress, Bytes: []byte{192, 168, 1, 50}}},
			{OID: "1.3.6.1.2.1.1.9.0", Value: snmp.Value{Type: snmp.TypeNull}},
			{OID: "2.999.4294967295", Value: str(string(make([]byte, 300)))},
		},
	}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	q, err := snmp.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, q) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", q, p)
	}
	if q.Vars[7].Value.String() != "192.168.1.50" || q.Vars[0].Value.String() != "Ricoh" || q.Vars[1].Value.String() != "-3" {
		t.Fatal("String()")
	}
}

func TestGetRequestBytes(t *testing.T) {
	p := &snmp.Packet{Community: "public", PDUType: snmp.PDUGetRequest, RequestID: 1,
		Vars: []snmp.Var{{OID: "1.3.6.1.2.1.1.1.0"}}}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x30, 0x26, // message
		0x02, 0x01, 0x00, // version 1 (0)
		0x04, 0x06, 'p', 'u', 'b', 'l', 'i', 'c',
		0xa0, 0x19, // GetRequest
		0x02, 0x01, 0x01, // request-id 1
		0x02, 0x01, 0x00, // error-status
		0x02, 0x01, 0x00, // error-index
		0x30, 0x0e, 0x30, 0x0c,
		0x06, 0x08, 0x2b, 0x06, 0x01, 0x02, 0x01, 0x01, 0x01, 0x00,
		0x05, 0x00,
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("got  % x\nwant % x", b, want)
	}
	// Multi-byte arcs and negative/large integers.
	p = &snmp.Packet{Community: "", PDUType: snmp.PDUGetNextRequest, RequestID: 300,
		Vars: []snmp.Var{{OID: "1.3.6.1.4.1.367"}}}
	b, _ = p.Marshal()
	// 367 = 2*128 + 111: 0x82 0x6f.
	want = []byte{0x30, 0x20, 0x02, 0x01, 0x00, 0x04, 0x00, 0xa1, 0x19,
		0x02, 0x02, 0x01, 0x2c, 0x02, 0x01, 0x00, 0x02, 0x01, 0x00,
		0x30, 0x0d, 0x30, 0x0b, 0x06, 0x07, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x6f, 0x05, 0x00}
	if !bytes.Equal(b, want) {
		t.Fatalf("got  % x\nwant % x", b, want)
	}
}

func TestUnmarshalGarbage(t *testing.T) {
	for _, b := range [][]byte{
		nil, {0x30}, {0x30, 0x80}, {0x30, 0x84, 0xff, 0xff, 0xff, 0xff}, {0x30, 0x05, 0x02, 0x01},
		{0x30, 0x03, 0x02, 0x01, 0x00},
		{0x30, 0x0b, 0x02, 0x01, 0x00, 0x04, 0x00, 0xa2, 0x04, 0x02, 0x09, 0, 0},
	} {
		if _, err := snmp.Unmarshal(b); err == nil {
			t.Errorf("% x: no error", b)
		}
	}
}

func FuzzUnmarshal(f *testing.F) {
	p := &snmp.Packet{Community: "public", PDUType: snmp.PDUGetResponse, RequestID: 7,
		Vars: []snmp.Var{{OID: "1.3.6.1.2.1.25.3.5.1.2.1", Value: snmp.Value{Type: snmp.TypeOctetString, Bytes: []byte{0}}},
			{OID: "1.3.6.1.2.1.43.11.1.1.9.1.1", Value: num(-3)}}}
	b, _ := p.Marshal()
	f.Add(b)
	f.Add([]byte{0x30, 0x03, 0x02, 0x01, 0x00})
	f.Add([]byte{0x30, 0x82, 0x00, 0x05, 0x02, 0x01, 0x00, 0x04, 0x00})
	f.Add([]byte{0x06, 0x03, 0x88, 0x80, 0x80})
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := snmp.Unmarshal(b)
		if err != nil {
			return
		}
		// What decodes must re-encode and decode the same.
		out, err := p.Marshal()
		if err != nil {
			return
		}
		q, err := snmp.Unmarshal(out)
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		if !reflect.DeepEqual(p, q) {
			t.Fatalf("re-decode differs:\n%+v\n%+v", p, q)
		}
	})
}

var table = map[string]snmp.Value{
	"1.3.6.1.2.1.1.1.0":           str("Fake printer"),
	"1.3.6.1.2.1.43.11.1.1.6.1.1": str("Cyan Cartridge"),
	"1.3.6.1.2.1.43.11.1.1.6.1.2": str("Black Cartridge"),
	"1.3.6.1.2.1.43.11.1.1.6.1.3": str("Waste Cartridge"),
	"1.3.6.1.2.1.43.11.1.1.8.1.1": num(-2),
	"1.3.6.1.2.1.43.11.1.1.8.1.2": num(-2),
	"1.3.6.1.2.1.43.11.1.1.8.1.3": num(-2),
	"1.3.6.1.2.1.43.12.1.1.4.1.1": str("cyan"),
}

func client(a *snmptest.Agent) *snmp.Client {
	c := snmp.NewClient(a.Addr, "public")
	c.Timeout = 300 * time.Millisecond
	return c
}

func TestGet(t *testing.T) {
	a := snmptest.New(t, table)
	got, err := client(a).Get(context.Background(), "1.3.6.1.2.1.1.1.0", ".1.3.6.1.2.1.43.11.1.1.8.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got["1.3.6.1.2.1.1.1.0"].String() != "Fake printer" || got["1.3.6.1.2.1.43.11.1.1.8.1.1"].Int != -2 {
		t.Fatalf("got %+v", got)
	}
	_, err = client(a).Get(context.Background(), "1.3.6.1.2.1.1.5.0")
	var se *snmp.Error
	if !errors.As(err, &se) || se.Status != 2 {
		t.Fatalf("missing OID: %v", err)
	}
}

func TestWalk(t *testing.T) {
	a := snmptest.New(t, table)
	vs, err := client(a).Walk(context.Background(), "1.3.6.1.2.1.43.11.1.1.6")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 || vs[0].Value.String() != "Cyan Cartridge" || vs[2].OID != "1.3.6.1.2.1.43.11.1.1.6.1.3" {
		t.Fatalf("got %+v", vs)
	}
	// The last subtree in the table ends at the end of the MIB (noSuchName).
	vs, err = client(a).Walk(context.Background(), "1.3.6.1.2.1.43.12")
	if err != nil || len(vs) != 1 || vs[0].Value.String() != "cyan" {
		t.Fatalf("end of MIB: %+v %v", vs, err)
	}
	vs, err = client(a).Walk(context.Background(), "1.3.6.1.4.1.367")
	if err != nil || len(vs) != 0 {
		t.Fatalf("empty subtree: %+v %v", vs, err)
	}
}

func TestWalkCap(t *testing.T) {
	big := map[string]snmp.Value{}
	for i := range snmp.MaxWalk + 50 {
		big["1.3.6.1.4.1.9."+itoa(i+1)] = num(int64(i))
	}
	a := snmptest.New(t, big)
	vs, err := client(a).Walk(context.Background(), "1.3.6.1.4.1.9")
	if err != nil || len(vs) != snmp.MaxWalk {
		t.Fatalf("got %d vars, %v", len(vs), err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestTimeoutAndRetry(t *testing.T) {
	a := snmptest.New(t, table)
	a.Silent.Store(true)
	c := client(a)
	start := time.Now()
	_, err := c.Get(context.Background(), "1.3.6.1.2.1.1.1.0")
	if !errors.Is(err, snmp.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d < 500*time.Millisecond || d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	if n := a.Requests.Load(); n != 2 {
		t.Fatalf("requests = %d, want 2 (one retry)", n)
	}
	// The context deadline wins over the per-attempt timeout.
	c.Timeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := c.Get(ctx, "1.3.6.1.2.1.1.1.0"); err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("ctx deadline ignored: %v", d)
	}
}

func TestMismatchedIDIgnored(t *testing.T) {
	a := snmptest.New(t, table)
	a.WrongIDFirst.Store(true)
	got, err := client(a).Get(context.Background(), "1.3.6.1.2.1.1.1.0")
	if err != nil || got["1.3.6.1.2.1.1.1.0"].String() != "Fake printer" || len(got) != 1 {
		t.Fatalf("got %+v %v", got, err)
	}
	if n := a.Requests.Load(); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}

func TestClosedPort(t *testing.T) {
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := l.LocalAddr().String()
	l.Close()
	c := snmp.NewClient(addr, "")
	c.Timeout = 200 * time.Millisecond
	if _, err := c.Walk(context.Background(), "1.3.6.1"); err == nil {
		t.Fatal("no error")
	}
}

func TestParseOID(t *testing.T) {
	for _, bad := range []string{"", "1", "1.x", "3.1", "1.40", "1.3.-1", "1..3"} {
		if _, err := snmp.ParseOID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if snmp.CompareOIDs("1.3.6.1.2", "1.3.6.1.10") >= 0 || snmp.CompareOIDs("1.3.6", "1.3.6.1") >= 0 {
		t.Fatal("compare")
	}
}

func TestOpaqueValues(t *testing.T) {
	// A packet with an Opaque (0x44), a malformed INTEGER (9 bytes) and a
	// multi-byte tag, between two good values.
	good1 := []byte{0x30, 0x0c, 0x06, 0x08, 0x2b, 6, 1, 2, 1, 1, 1, 0, 0x04, 0x00}
	oid := []byte{0x06, 0x08, 0x2b, 6, 1, 2, 1, 1, 2, 0}
	opaque := append(append([]byte{0x30, byte(len(oid) + 4)}, oid...), 0x44, 0x02, 0xab, 0xcd)
	badInt := append(append([]byte{0x30, byte(len(oid) + 11)}, oid...), 0x02, 0x09, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	longTag := append(append([]byte{0x30, byte(len(oid) + 5)}, oid...), 0x5f, 0x81, 0x01, 0x01, 0x07)
	good2 := []byte{0x30, 0x0d, 0x06, 0x08, 0x2b, 6, 1, 2, 1, 1, 3, 0, 0x02, 0x01, 0x05}
	var list []byte
	for _, v := range [][]byte{good1, opaque, badInt, longTag, good2} {
		list = append(list, v...)
	}
	pdu := append([]byte{0x02, 0x01, 0x07, 0x02, 0x01, 0x00, 0x02, 0x01, 0x00, 0x30, byte(len(list))}, list...)
	msg := append([]byte{0x02, 0x01, 0x00, 0x04, 0x06, 'p', 'u', 'b', 'l', 'i', 'c', 0xa2, byte(len(pdu))}, pdu...)
	b := append([]byte{0x30, byte(len(msg))}, msg...)
	p, err := snmp.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Vars) != 5 || p.Vars[4].Value.Int != 5 || p.Vars[0].Value.Type != snmp.TypeOctetString {
		t.Fatalf("got %+v", p.Vars)
	}
	for i := 1; i <= 3; i++ {
		if p.Vars[i].Value.Type != snmp.TypeUnknown || len(p.Vars[i].Value.Raw) == 0 {
			t.Fatalf("var %d: %+v", i, p.Vars[i].Value)
		}
	}
	if out, err := p.Marshal(); err != nil || !bytes.Equal(out, b) {
		t.Fatalf("re-encode: % x %v", out, err)
	}

	// Over the wire: the response is used, not dropped as a timeout.
	a := snmptest.New(t, map[string]snmp.Value{
		"1.3.6.1.2.1.1.1.0": str("ok"),
		"1.3.6.1.2.1.1.2.0": {Type: snmp.TypeUnknown, Raw: []byte{0x44, 0x01, 0x00}},
	})
	got, err := client(a).Get(context.Background(), "1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.2.0")
	if err != nil || got["1.3.6.1.2.1.1.1.0"].String() != "ok" || got["1.3.6.1.2.1.1.2.0"].Type != snmp.TypeUnknown {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestWalkStops(t *testing.T) {
	a := snmptest.New(t, table)
	c := client(a)
	// endOfMibView after one row.
	a.SetResponder(func(req *snmp.Packet) *snmp.Packet {
		v := snmp.Value{Type: snmp.TypeEndOfMibView}
		oid := req.Vars[0].OID
		if oid == "1.3.6.1.2.1.43.11.1.1.6" {
			oid, v = oid+".1.1", str("first")
		}
		return &snmp.Packet{Community: req.Community, PDUType: snmp.PDUGetResponse, RequestID: req.RequestID,
			Vars: []snmp.Var{{OID: oid, Value: v}}}
	})
	vs, err := c.Walk(context.Background(), "1.3.6.1.2.1.43.11.1.1.6")
	if err != nil || len(vs) != 1 || vs[0].Value.String() != "first" {
		t.Fatalf("endOfMibView: %+v %v", vs, err)
	}
	// An agent that answers the same OID again: stop, no loop.
	a.SetResponder(func(req *snmp.Packet) *snmp.Packet {
		return &snmp.Packet{Community: req.Community, PDUType: snmp.PDUGetResponse, RequestID: req.RequestID,
			Vars: []snmp.Var{{OID: "1.3.6.1.2.1.43.11.1.1.6.1.1", Value: str("again")}}}
	})
	n := a.Requests.Load()
	vs, err = c.Walk(context.Background(), "1.3.6.1.2.1.43.11.1.1.6")
	if err != nil || len(vs) != 1 || a.Requests.Load()-n != 2 {
		t.Fatalf("non-increasing: %+v %v, %d requests", vs, err, a.Requests.Load()-n)
	}
}

func TestWalkColumns(t *testing.T) {
	a := snmptest.New(t, table)
	s, err := client(a).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.WalkColumns(context.Background(), "1.3.6.1.2.1.43.11.1.1.6", ".1.3.6.1.2.1.43.11.1.1.8", "1.3.6.1.2.1.43.12.1.1.4", "1.3.6.1.4.1.367")
	if err != nil {
		t.Fatal(err)
	}
	if len(m["1.3.6.1.2.1.43.11.1.1.6"]) != 3 || len(m["1.3.6.1.2.1.43.11.1.1.8"]) != 3 ||
		len(m["1.3.6.1.2.1.43.12.1.1.4"]) != 1 || len(m["1.3.6.1.4.1.367"]) != 0 {
		t.Fatalf("got %+v", m)
	}
	// 3 rows + 1, plus one resend for each of the two columns that run off
	// the end of the MIB (SNMPv1 noSuchName), all on one socket.
	if n := a.Requests.Load(); n != 6 {
		t.Fatalf("requests = %d", n)
	}
}

func TestCommunityMismatchIgnored(t *testing.T) {
	a := snmptest.New(t, table)
	a.SetResponder(func(req *snmp.Packet) *snmp.Packet {
		return &snmp.Packet{Community: "other", PDUType: snmp.PDUGetResponse, RequestID: req.RequestID,
			Vars: []snmp.Var{{OID: req.Vars[0].OID, Value: str("x")}}}
	})
	if _, err := client(a).Get(context.Background(), "1.3.6.1.2.1.1.1.0"); !errors.Is(err, snmp.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
}
