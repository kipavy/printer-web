// Package snmptest provides a fake SNMPv1 agent on 127.0.0.1 for tests.
package snmptest

import (
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kipavy/printer-web/internal/snmp"
)

// Agent serves a fixed set of variables over UDP.
type Agent struct {
	Addr string // 127.0.0.1:port

	conn *net.UDPConn
	mu   sync.Mutex
	vars []snmp.Var // sorted by OID

	// Silent makes the agent read requests and never answer.
	Silent atomic.Bool
	// WrongIDFirst makes the agent send a garbage packet and a response
	// with the wrong request ID before each real response.
	WrongIDFirst atomic.Bool
	// Requests counts the requests received.
	Requests atomic.Int32
	// Delay is waited before each answer (nanoseconds).
	Delay atomic.Int64

	respond func(req *snmp.Packet) *snmp.Packet // guarded by mu
}

// SetResponder replaces the default answer; f returns nil to fall back to
// it.
func (a *Agent) SetResponder(f func(req *snmp.Packet) *snmp.Packet) {
	a.mu.Lock()
	a.respond = f
	a.mu.Unlock()
}

// New starts an agent serving vars; it stops when the test ends.
func New(t testing.TB, vars map[string]snmp.Value) *Agent {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Addr: conn.LocalAddr().String(), conn: conn}
	a.Set(vars)
	done := make(chan struct{})
	go func() { defer close(done); a.serve() }()
	t.Cleanup(func() { _ = conn.Close(); <-done })
	return a
}

// Set replaces the served variables.
func (a *Agent) Set(vars map[string]snmp.Value) {
	vs := make([]snmp.Var, 0, len(vars))
	for o, v := range vars {
		vs = append(vs, snmp.Var{OID: strings.TrimPrefix(o, "."), Value: v})
	}
	sort.Slice(vs, func(i, j int) bool { return snmp.CompareOIDs(vs[i].OID, vs[j].OID) < 0 })
	a.mu.Lock()
	a.vars = vs
	a.mu.Unlock()
}

func (a *Agent) serve() {
	buf := make([]byte, 65535)
	for {
		n, from, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		a.Requests.Add(1)
		req, err := snmp.Unmarshal(buf[:n])
		if err != nil || a.Silent.Load() {
			continue
		}
		if d := a.Delay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		a.mu.Lock()
		f := a.respond
		a.mu.Unlock()
		var resp *snmp.Packet
		if f != nil {
			resp = f(req)
		}
		if resp == nil {
			resp = a.answer(req)
		}
		if a.WrongIDFirst.Load() {
			_, _ = a.conn.WriteToUDP([]byte{0x30, 0x03, 0x02, 0x01}, from)
			bad := *resp
			bad.RequestID++
			bad.Vars = []snmp.Var{{OID: "1.3.6.1.9.9", Value: snmp.Value{Type: snmp.TypeOctetString, Bytes: []byte("wrong")}}}
			if b, err := bad.Marshal(); err == nil {
				_, _ = a.conn.WriteToUDP(b, from)
			}
		}
		if b, err := resp.Marshal(); err == nil {
			_, _ = a.conn.WriteToUDP(b, from)
		}
	}
}

func (a *Agent) answer(req *snmp.Packet) *snmp.Packet {
	a.mu.Lock()
	defer a.mu.Unlock()
	resp := &snmp.Packet{Version: req.Version, Community: req.Community, PDUType: snmp.PDUGetResponse, RequestID: req.RequestID}
	for i, rv := range req.Vars {
		var found *snmp.Var
		for j := range a.vars {
			v := &a.vars[j]
			if (req.PDUType == snmp.PDUGetRequest && v.OID == rv.OID) ||
				(req.PDUType == snmp.PDUGetNextRequest && snmp.CompareOIDs(v.OID, rv.OID) > 0) {
				found = v
				break
			}
		}
		if found == nil { // SNMPv1: noSuchName for the whole request
			resp.ErrorStatus, resp.ErrorIndex = 2, int64(i+1)
			resp.Vars = req.Vars
			return resp
		}
		resp.Vars = append(resp.Vars, *found)
	}
	return resp
}
