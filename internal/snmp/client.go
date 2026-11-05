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

// Session is one UDP socket to the agent, reused for several requests.
type Session struct {
	c    *Client
	conn net.Conn
	buf  []byte
}

// Open creates a session; Close it when done.
func (c *Client) Open(ctx context.Context) (*Session, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", c.addr())
	if err != nil {
		return nil, err
	}
	return &Session{c: c, conn: conn, buf: make([]byte, maxPacketLen)}, nil
}

// Close closes the socket.
func (s *Session) Close() error { return s.conn.Close() }

// exchange sends one request and waits for the response with the same
// request ID and community, retrying on timeout. Garbage and mismatched
// packets are ignored.
func (s *Session) exchange(ctx context.Context, pduType byte, oids []string) (*Packet, error) {
	c := s.c
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
	stop := context.AfterFunc(ctx, func() { _ = s.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	for attempt := 0; attempt <= retries; attempt++ {
		dl := time.Now().Add(timeout)
		if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
			dl = cd
		}
		if err := s.conn.SetDeadline(dl); err != nil {
			return nil, err
		}
		// After SetDeadline: a cancel that raced with it is not missed.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := s.conn.Write(b); err != nil {
			return nil, err
		}
		for {
			n, err := s.conn.Read(s.buf)
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
			p, err := Unmarshal(s.buf[:n])
			if err != nil || p.PDUType != PDUGetResponse || p.RequestID != id || p.Version != 0 || p.Community != req.Community {
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

func normalize(oids []string) ([]string, error) {
	out := make([]string, len(oids))
	for i, o := range oids {
		arcs, err := ParseOID(o)
		if err != nil {
			return nil, err
		}
		out[i] = formatOID(arcs)
	}
	return out, nil
}

// Get reads the given OIDs. The result is keyed by OID (without a leading
// dot). Values that are v2 exceptions are left out.
func (s *Session) Get(ctx context.Context, oids ...string) (map[string]Value, error) {
	norm, err := normalize(oids)
	if err != nil || len(norm) == 0 {
		return map[string]Value{}, err
	}
	p, err := s.exchange(ctx, PDUGetRequest, norm)
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
// subtree, at the end of the MIB, when the agent does not move forward, or
// after MaxWalk variables.
func (s *Session) Walk(ctx context.Context, root string) ([]Var, error) {
	norm, err := normalize([]string{root})
	if err != nil {
		return nil, err
	}
	m, err := s.WalkColumns(ctx, norm[0])
	return m[norm[0]], err
}

// WalkColumns walks several subtrees together: each GetNext carries one
// variable per subtree not yet finished, so a table costs about rows+1
// requests. The result is keyed by root as given (leading dot removed).
// MaxWalk bounds the total number of variables.
func (s *Session) WalkColumns(ctx context.Context, roots ...string) (map[string][]Var, error) {
	norm, err := normalize(roots)
	if err != nil {
		return nil, err
	}
	out := map[string][]Var{}
	type column struct{ root, cur string }
	var active []column
	for _, r := range norm {
		if _, dup := out[r]; !dup {
			out[r] = nil
			active = append(active, column{r, r})
		}
	}
	total := 0
	for len(active) > 0 && total < MaxWalk {
		oids := make([]string, len(active))
		for i, col := range active {
			oids[i] = col.cur
		}
		p, err := s.exchange(ctx, PDUGetNextRequest, oids)
		if err != nil {
			// SNMPv1 noSuchName: that variable ran off the end of the MIB.
			var se *Error
			if errors.As(err, &se) && se.Status == 2 && se.Index >= 1 && se.Index <= int64(len(active)) {
				active = append(active[:se.Index-1], active[se.Index:]...)
				continue
			}
			return out, err
		}
		if len(p.Vars) != len(active) {
			return out, errors.New("snmp: bad GetNext response")
		}
		next := active[:0]
		for i, col := range active {
			v := p.Vars[i]
			if v.Value.IsException() || !strings.HasPrefix(v.OID, col.root+".") ||
				CompareOIDs(v.OID, col.cur) <= 0 || total >= MaxWalk {
				continue // column done
			}
			out[col.root] = append(out[col.root], v)
			total++
			next = append(next, column{col.root, v.OID})
		}
		active = next
	}
	return out, nil
}

// Get opens a session, reads the OIDs and closes it.
func (c *Client) Get(ctx context.Context, oids ...string) (map[string]Value, error) {
	s, err := c.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Get(ctx, oids...)
}

// Walk opens a session, walks root and closes it.
func (c *Client) Walk(ctx context.Context, root string) ([]Var, error) {
	s, err := c.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Walk(ctx, root)
}
