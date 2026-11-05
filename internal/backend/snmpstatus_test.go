package backend

import (
	"context"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kipavy/printer-web/internal/config"
	"github.com/kipavy/printer-web/internal/snmp"
	"github.com/kipavy/printer-web/internal/snmp/snmptest"
)

func sv(s string) snmp.Value { return snmp.Value{Type: snmp.TypeOctetString, Bytes: []byte(s)} }
func iv(n int64) snmp.Value  { return snmp.Value{Type: snmp.TypeInteger, Int: n} }

// ricohVars is what a Ricoh SP C240DN answers: standard levels unknown,
// percentages in the private MIB.
func ricohVars() map[string]snmp.Value {
	m := map[string]snmp.Value{
		oidErrorState + ".1":            {Type: snmp.TypeOctetString, Bytes: []byte{0}},
		oidDeviceStatus + ".1":          iv(2),
		oidDeviceType + ".1":            {Type: snmp.TypeOID, OID: oidDevicePrinter},
		"1.3.6.1.2.1.43.18.1.1.8.1.127": sv("Ready"),
	}
	for i, n := range []string{"Cyan Cartridge", "Magenta Cartridge", "Yellow Cartridge", "Black Cartridge", "Waste Cartridge"} {
		k := ".1." + string(rune('1'+i))
		m[oidSupplyDesc+k] = sv(n)
		m[oidSupplyMax+k] = iv(-2)
		m[oidSupplyLevel+k] = iv(-3)
	}
	for i, n := range []string{"Cyan", "Magenta", "Yellow", "Black"} {
		k := "." + string(rune('1'+i))
		m["1.3.6.1.4.1.367.3.2.1.2.24.1.1.2"+k] = sv(n)
		m[oidRicohTonerName+k] = sv(n + " Toner")
		m[oidRicohTonerPct+k] = iv(100)
	}
	return m
}

func testReader(a *snmptest.Agent) *snmpReader {
	r := newSNMPReader(a.Addr, "public")
	r.client.Timeout = 300 * time.Millisecond
	return r
}

func TestSNMPRicohPrivateMIB(t *testing.T) {
	a := snmptest.New(t, ricohVars())
	st := testReader(a).merge(ctx(t), Status{State: StateReady})
	want := []Supply{
		{Name: "Cyan Toner", Color: "cyan", Level: 100},
		{Name: "Magenta Toner", Color: "magenta", Level: 100},
		{Name: "Yellow Toner", Color: "yellow", Level: 100},
		{Name: "Black Toner", Color: "black", Level: 100},
	}
	if !reflect.DeepEqual(st.Supplies, want) || st.State != StateReady || len(st.Reasons) != 0 {
		t.Fatalf("got %+v", st)
	}
}

func TestSNMPRicohNearEmpty(t *testing.T) {
	v := ricohVars()
	v[oidRicohTonerPct+".4"] = iv(-3)
	a := snmptest.New(t, v)
	st := testReader(a).merge(ctx(t), Status{State: StateReady})
	if len(st.Supplies) != 4 || st.Supplies[3].Level != -1 || !slices.Contains(st.Reasons, "toner-low") || st.State != StateReady {
		t.Fatalf("got %+v", st)
	}
}

func TestSNMPStandardLevels(t *testing.T) {
	v := map[string]snmp.Value{
		oidSupplyDesc + ".1.1":     sv("Toner A"),
		oidSupplyDesc + ".1.2":     sv("Toner B"),
		oidSupplyDesc + ".1.3":     sv("Drum Unit"),
		oidSupplyDesc + ".1.4":     sv("Imaging thing"),
		oidSupplyType + ".1.1":     iv(3),
		oidSupplyType + ".1.2":     iv(21),
		oidSupplyType + ".1.3":     iv(9), // opc: hidden
		oidSupplyType + ".1.4":     iv(4), // wasteToner: hidden
		oidSupplyMax + ".1.1":      iv(8000),
		oidSupplyMax + ".1.2":      iv(200),
		oidSupplyLevel + ".1.1":    iv(2000),
		oidSupplyLevel + ".1.2":    iv(-3),
		oidSupplyColorant + ".1.1": iv(2),
		oidSupplyColorant + ".1.2": iv(1),
		oidColorantValue + ".1.1":  sv("black"),
		oidColorantValue + ".1.2":  sv("magenta"),
		// Must not be used: a standard level is known.
		oidRicohTonerName + ".1": sv("Cyan Toner"),
		oidRicohTonerPct + ".1":  iv(50),
	}
	a := snmptest.New(t, v)
	st := testReader(a).merge(ctx(t), Status{State: StateReady})
	// Toner A: colorant index 2 (magenta), 2000/8000. Toner B: level unknown.
	want := []Supply{{Name: "Toner A", Color: "magenta", Level: 25}, {Name: "Toner B", Color: "black", Level: -1}}
	if !reflect.DeepEqual(st.Supplies, want) {
		t.Fatalf("got %+v, want %+v", st.Supplies, want)
	}
	// No error state object: no reasons, no error.
	if len(st.Reasons) != 0 || st.State != StateReady {
		t.Fatalf("got %+v", st)
	}
}

func TestSNMPErrorStateReasons(t *testing.T) {
	cases := []struct {
		bytes []byte
		want  []string
		state State
	}{
		{[]byte{0x00}, nil, StateReady},
		{[]byte{0x80}, []string{"media-low"}, StateReady},
		{[]byte{0x40}, []string{"media-empty"}, StateStopped},
		{[]byte{0x20, 0x00}, []string{"toner-low"}, StateReady},
		{[]byte{0x0c}, []string{"door-open", "media-jam"}, StateStopped},
		{[]byte{0x01, 0xa0}, []string{"input-tray-missing", "marker-supply-missing"}, StateReady}, // serviceRequested: no key
		{[]byte{0x40, 0x04}, []string{"media-empty", "media-needed"}, StateStopped},               // noPaper + inputTrayEmpty
		{[]byte{0x00, 0x04}, []string{"media-needed"}, StateReady},                                // inputTrayEmpty alone: not blocking
		{[]byte{0x00, 0x08}, []string{"output-area-full"}, StateReady},
		{[]byte{0x00, 0x52}, nil, StateReady}, // outputTrayMissing, outputNearFull, overduePreventMaint: no key
	}
	for _, c := range cases {
		v := ricohVars()
		v[oidErrorState+".1"] = snmp.Value{Type: snmp.TypeOctetString, Bytes: c.bytes}
		a := snmptest.New(t, v)
		st := testReader(a).merge(ctx(t), Status{State: StateReady})
		if !slices.Equal(st.Reasons, c.want) || st.State != c.state || len(st.Supplies) != 4 {
			t.Errorf("% x: got %+v, want %v %s", c.bytes, st, c.want, c.state)
		}
	}
	// Printing stays printing; existing reasons are kept once.
	v := ricohVars()
	v[oidErrorState+".1"] = snmp.Value{Type: snmp.TypeOctetString, Bytes: []byte{0x40}}
	v[oidDeviceStatus+".1"] = iv(5)
	a := snmptest.New(t, v)
	r := testReader(a)
	st := r.merge(ctx(t), Status{State: StatePrinting, Reasons: []string{"media-empty"}})
	if st.State != StatePrinting || !slices.Equal(st.Reasons, []string{"media-empty"}) {
		t.Fatalf("got %+v", st)
	}
}

func TestSNMPDeviceDown(t *testing.T) {
	v := ricohVars()
	v[oidDeviceStatus+".1"] = iv(5)
	a := snmptest.New(t, v)
	if st := testReader(a).merge(ctx(t), Status{State: StateReady}); st.State != StateStopped {
		t.Fatalf("got %+v", st)
	}
}

func TestSNMPUnreachable(t *testing.T) {
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := l.LocalAddr().String()
	l.Close()
	r := newSNMPReader(addr, "public")
	r.client.Timeout = 200 * time.Millisecond
	st := r.merge(ctx(t), Status{State: StateReady})
	if st.State != StateReady || len(st.Supplies) != 0 || len(st.Reasons) != 0 {
		t.Fatalf("got %+v", st)
	}

	// A silent agent: same, within the SNMP timeout.
	a := snmptest.New(t, ricohVars())
	a.Silent.Store(true)
	r = newSNMPReader(a.Addr, "public")
	start := time.Now()
	st = r.merge(ctx(t), Status{State: StateReady})
	if st.State != StateReady || len(st.Supplies) != 0 {
		t.Fatalf("got %+v", st)
	}
	if d := time.Since(start); d > snmpTimeout+time.Second {
		t.Fatalf("took %v", d)
	}
	// Unreachable stays unreachable and SNMP is not even asked.
	n := a.Requests.Load()
	if st := r.merge(ctx(t), Status{State: StateUnreachable}); st.State != StateUnreachable || a.Requests.Load() != n {
		t.Fatalf("got %+v", st)
	}
}

func TestSNMPCached(t *testing.T) {
	a := snmptest.New(t, ricohVars())
	r := testReader(a)
	r.merge(ctx(t), Status{State: StateReady})
	n := a.Requests.Load()
	st := r.merge(ctx(t), Status{State: StateReady})
	if a.Requests.Load() != n || len(st.Supplies) != 4 {
		t.Fatalf("not cached: %d -> %d", n, a.Requests.Load())
	}
	st.Supplies[0].Name = "changed"
	if r.merge(ctx(t), Status{State: StateReady}).Supplies[0].Name == "changed" {
		t.Fatal("cached slice shared")
	}
}

func TestSNMPInBackends(t *testing.T) {
	a := snmptest.New(t, ricohVars())
	// command, no probe
	cb, err := newCommand(config.Config{Backend: "command", Command: []string{os.Args[0]}, SNMP: a.Addr, SNMPCommunity: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := cb.Status(ctx(t)); err != nil || len(st.Supplies) != 4 {
		t.Fatalf("command: %+v %v", st, err)
	}
	// raw with a live socket
	addr, _ := listen(t)
	cfg := rawConfig(addr, "ps")
	cfg.SNMP, cfg.SNMPCommunity = a.Addr, "public"
	rb := newRaw(cfg)
	if st, err := rb.Status(ctx(t)); err != nil || st.State != StateReady || len(st.Supplies) != 4 {
		t.Fatalf("raw: %+v %v", st, err)
	}
}

func TestSNMPForIPPWithoutLevels(t *testing.T) {
	a := snmptest.New(t, ricohVars())
	b, _ := newTestIPP(t, nil)
	b.snmp = testReader(a)
	st, err := b.Status(ctx(t))
	if err != nil || len(st.Supplies) != 4 || st.Supplies[0].Level != 100 {
		t.Fatalf("got %+v %v", st, err)
	}
}

func TestSNMPClassAndClamp(t *testing.T) {
	v := map[string]snmp.Value{
		oidSupplyDesc + ".1.1":  sv("Toner C"),
		oidSupplyType + ".1.1":  iv(1), // other: kept
		oidSupplyClass + ".1.1": iv(3),
		oidSupplyMax + ".1.1":   iv(100),
		oidSupplyLevel + ".1.1": iv(150), // above max: clamped
		oidSupplyDesc + ".1.2":  sv("Collector"),
		oidSupplyType + ".1.2":  iv(2), // unknown, but a receptacle
		oidSupplyClass + ".1.2": iv(4),
		oidSupplyDesc + ".1.3":  sv("Waste-free toner"),
		oidSupplyClass + ".1.3": iv(3), // class wins over the name
		oidSupplyMax + ".1.3":   iv(200),
		oidSupplyLevel + ".1.3": iv(50),
	}
	a := snmptest.New(t, v)
	st := testReader(a).merge(ctx(t), Status{State: StateReady})
	want := []Supply{{Name: "Toner C", Level: 100}, {Name: "Waste-free toner", Level: 25}}
	if !reflect.DeepEqual(st.Supplies, want) {
		t.Fatalf("got %+v", st.Supplies)
	}
}

func TestSNMPRicohEmpty(t *testing.T) {
	v := ricohVars()
	v[oidRicohTonerPct+".2"] = iv(0)
	a := snmptest.New(t, v)
	st := testReader(a).merge(ctx(t), Status{State: StateReady})
	if st.Supplies[1].Level != 0 || !slices.Equal(st.Reasons, []string{"toner-empty"}) {
		t.Fatalf("got %+v", st)
	}
}

func TestSNMPPrinterRowNotOne(t *testing.T) {
	v := ricohVars()
	delete(v, oidErrorState+".1")
	delete(v, oidDeviceStatus+".1")
	delete(v, oidDeviceType+".1")
	v[oidDeviceType+".1"] = snmp.Value{Type: snmp.TypeOID, OID: "1.3.6.1.2.1.25.3.1.3"} // processor
	v[oidDeviceStatus+".1"] = iv(5)
	v[oidDeviceType+".3"] = snmp.Value{Type: snmp.TypeOID, OID: oidDevicePrinter}
	v[oidDeviceStatus+".3"] = iv(2)
	v[oidErrorState+".3"] = snmp.Value{Type: snmp.TypeOctetString, Bytes: []byte{0x04}}
	a := snmptest.New(t, v)
	st := testReader(a).merge(ctx(t), Status{State: StateReady})
	if !slices.Equal(st.Reasons, []string{"media-jam"}) || st.State != StateStopped {
		t.Fatalf("got %+v", st)
	}
	// No printer table: the device of type printer gives the status.
	delete(v, oidErrorState+".3")
	v[oidDeviceStatus+".3"] = iv(5)
	a = snmptest.New(t, v)
	st = testReader(a).merge(ctx(t), Status{State: StateReady})
	if len(st.Reasons) != 0 || st.State != StateStopped {
		t.Fatalf("got %+v", st)
	}
}

func TestSNMPSlowAgent(t *testing.T) {
	a := snmptest.New(t, ricohVars())
	a.Delay.Store(int64(60 * time.Millisecond))
	r := newSNMPReader(a.Addr, "public") // real timeouts
	start := time.Now()
	st := r.merge(ctx(t), Status{State: StateReady})
	if len(st.Supplies) != 4 || st.Supplies[0].Level != 100 {
		t.Fatalf("got %+v after %v, %d requests", st, time.Since(start), a.Requests.Load())
	}
	if n := a.Requests.Load(); n > 20 {
		t.Fatalf("%d requests", n)
	}
}

func TestSNMPPartialOnSupplyFailure(t *testing.T) {
	v := ricohVars()
	v[oidErrorState+".1"] = snmp.Value{Type: snmp.TypeOctetString, Bytes: []byte{0x40}}
	a := snmptest.New(t, v)
	// Answer the printer state, then go silent once the supplies are asked.
	a.SetResponder(func(req *snmp.Packet) *snmp.Packet {
		if strings.HasPrefix(req.Vars[0].OID, oidSupplyDesc) {
			a.Silent.Store(true)
		}
		return nil
	})
	r := testReader(a)
	c, cancel := context.WithTimeout(ctx(t), 10*time.Second)
	defer cancel()
	st := r.merge(c, Status{State: StateReady})
	if !slices.Equal(st.Reasons, []string{"media-empty"}) || st.State != StateStopped || len(st.Supplies) != 0 {
		t.Fatalf("got %+v", st)
	}
}
