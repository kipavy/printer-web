package snmp

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultPort is the SNMP agent port.
const DefaultPort = "161"

// MaxWalk bounds the number of variables one Walk returns.
const MaxWalk = 500

// Defaults for Client.
const (
	DefaultTimeout = time.Second // per attempt
	DefaultRetries = 1
)

// ErrTimeout is returned when the agent does not answer.
var ErrTimeout = errors.New("snmp: no answer")

// Error is an error status sent by the agent (2 is noSuchName).
type Error struct {
	Status, Index int64
}

func (e *Error) Error() string {
	return fmt.Sprintf("snmp: agent error status %d (index %d)", e.Status, e.Index)
}

// Client talks SNMPv1 to one agent.
type Client struct {
	Addr      string        // host:port, or host (port 161)
	Community string        // "public" when empty
	Timeout   time.Duration // per attempt; DefaultTimeout when 0
	Retries   int           // extra attempts after a timeout

	nextID atomic.Int32
}

// NewClient returns a client with the default timeout and one retry.
func NewClient(addr, community string) *Client {
	c := &Client{Addr: addr, Community: community, Timeout: DefaultTimeout, Retries: DefaultRetries}
	c.nextID.Store(rand.Int32N(1 << 30))
	return c
}

func (c *Client) addr() string {
	if _, _, err := net.SplitHostPort(c.Addr); err == nil {
		return c.Addr
	}
	return net.JoinHostPort(strings.Trim(c.Addr, "[]"), DefaultPort)
}

func (c *Client) community() string {
	if c.Community == "" {
		return "public"
	}
	return c.Community
}

func (c *Client) id() int32 {
	id := c.nextID.Add(1) & 0x7fffffff
	if id == 0 {
		id = c.nextID.Add(1) & 0x7fffffff
	}
	return id
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "udp", c.addr())
}

// exchange sends one request and waits for the response with the same
// request ID, retrying once on timeout. Garbage and mismatched packets are
// ignored.
func (c *Client) exchange(ctx context.Context, conn net.Conn, pduType byte, oids []string) (*Packet, error) {
	id := c.id()
	req := &Packet{Version: 0, Community: c.community(), PDUType: pduType, RequestID: id}
	for _, o := range oids {
		req.Vars = append(req.Vars, Var{OID: o, Value: Value{Type: TypeNull}})
	}
	b, err := req.Marshal()
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	retries := max(c.Retries, 0)
	// Wake a blocked read when ctx ends.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	buf := make([]byte, maxPacketLen)
	for attempt := 0; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dl := time.Now().Add(timeout)
		if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
			dl = cd
		}
		if err := conn.SetDeadline(dl); err != nil {
			return nil, err
		}
		if _, err := conn.Write(b); err != nil {
			return nil, err
		}
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break // next attempt
				}
				return nil, err
			}
			p, err := Unmarshal(buf[:n])
			if err != nil || p.PDUType != PDUGetResponse || p.RequestID != id || p.Version != 0 {
				continue
			}
			if p.ErrorStatus != 0 {
				return nil, &Error{Status: p.ErrorStatus, Index: p.ErrorIndex}
			}
			return p, nil
		}
	}
	return nil, ErrTimeout
}

// Get reads the given OIDs. The result is keyed by OID (without a leading
// dot). Values that are v2 exceptions are left out.
func (c *Client) Get(ctx context.Context, oids ...string) (map[string]Value, error) {
	if len(oids) == 0 {
		return map[string]Value{}, nil
	}
	norm := make([]string, len(oids))
	for i, o := range oids {
		arcs, err := ParseOID(o)
		if err != nil {
			return nil, err
		}
		norm[i] = formatOID(arcs)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	p, err := c.exchange(ctx, conn, PDUGetRequest, norm)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Value, len(p.Vars))
	for _, v := range p.Vars {
		if !v.Value.IsException() {
			out[v.OID] = v.Value
		}
	}
	return out, nil
}

// Walk reads the subtree under root with GetNext. It stops at the end of the
// subtree, at the end of the MIB, or after MaxWalk variables.
func (c *Client) Walk(ctx context.Context, root string) ([]Var, error) {
	arcs, err := ParseOID(root)
	if err != nil {
		return nil, err
	}
	root = formatOID(arcs)
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out []Var
	cur := root
	for len(out) < MaxWalk {
		p, err := c.exchange(ctx, conn, PDUGetNextRequest, []string{cur})
		if err != nil {
			var se *Error
			if errors.As(err, &se) && se.Status == 2 { // noSuchName: end of MIB
				return out, nil
			}
			return out, err
		}
		if len(p.Vars) != 1 {
			return out, errors.New("snmp: bad GetNext response")
		}
		v := p.Vars[0]
		if v.Value.IsException() || !strings.HasPrefix(v.OID, root+".") {
			return out, nil
		}
		if CompareOIDs(v.OID, cur) <= 0 { // agent going backwards: stop
			return out, nil
		}
		out = append(out, v)
		cur = v.OID
	}
	return out, nil
}
