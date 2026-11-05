package backend

import (
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kipavy/printer-web/internal/snmp"
)

// snmpTimeout bounds one whole SNMP read (all its requests).
const snmpTimeout = 2 * time.Second

// OIDs read over SNMP (RFC 3805 Printer-MIB, RFC 2790 Host Resources MIB and
// the Ricoh private MIB).
const (
	oidSupplyColorant = "1.3.6.1.2.1.43.11.1.1.3"  // prtMarkerSuppliesColorantIndex
	oidSupplyType     = "1.3.6.1.2.1.43.11.1.1.5"  // prtMarkerSuppliesType
	oidSupplyDesc     = "1.3.6.1.2.1.43.11.1.1.6"  // prtMarkerSuppliesDescription
	oidSupplyMax      = "1.3.6.1.2.1.43.11.1.1.8"  // prtMarkerSuppliesMaxCapacity
	oidSupplyLevel    = "1.3.6.1.2.1.43.11.1.1.9"  // prtMarkerSuppliesLevel
	oidColorantValue  = "1.3.6.1.2.1.43.12.1.1.4"  // prtMarkerColorantValue
	oidErrorState     = "1.3.6.1.2.1.25.3.5.1.2.1" // hrPrinterDetectedErrorState
	oidDeviceStatus   = "1.3.6.1.2.1.25.3.2.1.5.1" // hrDeviceStatus
	oidRicohTonerName = "1.3.6.1.4.1.367.3.2.1.2.24.1.1.3"
	oidRicohTonerPct  = "1.3.6.1.4.1.367.3.2.1.2.24.1.1.5"
)

// tonerTypes are the prtMarkerSuppliesType values shown as supplies:
// toner(3), ink(5), inkCartridge(6), inkRibbon(7), solidWax(12),
// ribbonWax(13), tonerCartridge(21). Waste, drums, fusers... are hidden.
var tonerTypes = []int64{3, 5, 6, 7, 12, 13, 21}

// errorBits maps hrPrinterDetectedErrorState bits (bit 0 is the high bit of
// the first byte) to printer-state-reasons keywords.
var errorBits = map[int]string{
	0:  "media-low",
	1:  "media-empty",
	2:  "toner-low",
	3:  "toner-empty",
	4:  "door-open",
	5:  "media-jam",
	6:  "offline",
	7:  "other",
	8:  "input-tray-missing",
	10: "marker-supply-missing",
	12: "output-area-full",
	13: "media-empty",
}

// blockingReasons stop the printer (the UI asks before printing).
var blockingReasons = []string{"media-empty", "media-jam", "door-open"}

// snmpInfo is what one SNMP read found.
type snmpInfo struct {
	supplies []Supply
	reasons  []string
	down     bool // hrDeviceStatus down(5)
}

// snmpReader reads supplies and error states over SNMP for backends that
// cannot see them. It never decides reachability.
type snmpReader struct {
	client *snmp.Client
	cache  *cache

	mu     sync.Mutex
	failed bool // last read failed (logged once per change)
}

func newSNMPReader(addr, community string) *snmpReader {
	c := snmp.NewClient(addr, community)
	c.Timeout = snmpTimeout / 2 // two attempts fit in snmpTimeout
	return &snmpReader{client: c, cache: newCache(cacheTTL)}
}

// read returns the cached info; zero on failure.
func (r *snmpReader) read(ctx context.Context) snmpInfo {
	v, err := r.cache.do(ctx, "snmp", func() (any, error) {
		ctx, cancel := context.WithTimeout(ctx, snmpTimeout)
		defer cancel()
		return fetchSNMP(ctx, r.client)
	})
	if ctx.Err() != nil {
		return snmpInfo{}
	}
	r.note(err)
	if err != nil {
		return snmpInfo{}
	}
	info := v.(snmpInfo)
	return snmpInfo{supplies: slices.Clone(info.supplies), reasons: slices.Clone(info.reasons), down: info.down}
}

func (r *snmpReader) note(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if (err != nil) == r.failed {
		return
	}
	r.failed = err != nil
	if err != nil {
		log.Printf("snmp: no supplies or alerts: %v", err)
	} else {
		log.Print("snmp: answering again")
	}
}

// merge adds the SNMP supplies and reasons to a status from the TCP probe.
// An unreachable printer is left as is.
func (r *snmpReader) merge(ctx context.Context, st Status) Status {
	if r == nil || st.State == StateUnreachable {
		return st
	}
	info := r.read(ctx)
	if len(st.Supplies) == 0 {
		st.Supplies = info.supplies
	}
	for _, x := range info.reasons {
		if !slices.Contains(st.Reasons, x) {
			st.Reasons = append(st.Reasons, x)
		}
	}
	if st.State == StateReady && (info.down || slices.ContainsFunc(st.Reasons, func(x string) bool {
		return slices.Contains(blockingReasons, x)
	})) {
		st.State = StateStopped
	}
	return st
}

// agentError reports an error status from the agent (a missing object),
// as opposed to no answer at all.
func agentError(err error) bool {
	var se *snmp.Error
	return errors.As(err, &se)
}

// fetchSNMP reads everything. A missing object is not an error; no answer
// is.
func fetchSNMP(ctx context.Context, c *snmp.Client) (snmpInfo, error) {
	var info snmpInfo
	get := func(oid string) (snmp.Value, bool, error) {
		m, err := c.Get(ctx, oid)
		if err != nil {
			if agentError(err) {
				return snmp.Value{}, false, nil
			}
			return snmp.Value{}, false, err
		}
		v, ok := m[oid]
		return v, ok, nil
	}
	if v, ok, err := get(oidErrorState); err != nil {
		return info, err
	} else if ok && v.Type == snmp.TypeOctetString {
		info.reasons = errorReasons(v.Bytes)
	}
	if v, ok, err := get(oidDeviceStatus); err != nil {
		return info, err
	} else if ok && v.Type == snmp.TypeInteger && v.Int == 5 {
		info.down = true
	}
	sup, err := readSupplies(ctx, c)
	if err != nil {
		return info, err
	}
	if !slices.ContainsFunc(sup, func(s Supply) bool { return s.Level >= 0 }) {
		ricoh, low, err := readRicohToners(ctx, c)
		if err != nil {
			return info, err
		}
		if len(ricoh) > 0 {
			sup = ricoh
		}
		if low && !slices.Contains(info.reasons, "toner-low") {
			info.reasons = append(info.reasons, "toner-low")
		}
	}
	info.supplies = sup
	return info, nil
}

// errorReasons decodes hrPrinterDetectedErrorState.
func errorReasons(b []byte) []string {
	var out []string
	for bit := 0; bit < len(b)*8 && bit < 16; bit++ {
		if b[bit/8]&(0x80>>(bit%8)) == 0 {
			continue
		}
		if r, ok := errorBits[bit]; ok && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// walkMap walks a table column and keys the values by row index (the OID
// suffix after the column).
func walkMap(ctx context.Context, c *snmp.Client, col string) (map[string]snmp.Value, []string, error) {
	vs, err := c.Walk(ctx, col)
	if err != nil && !agentError(err) {
		return nil, nil, err
	}
	m := map[string]snmp.Value{}
	var order []string
	for _, v := range vs {
		idx := strings.TrimPrefix(v.OID, col+".")
		if _, dup := m[idx]; !dup {
			order = append(order, idx)
		}
		m[idx] = v.Value
	}
	return m, order, nil
}

func intValue(v snmp.Value, ok bool) (int64, bool) {
	return v.Int, ok && v.Type == snmp.TypeInteger
}

// readSupplies reads the Printer-MIB supplies table, toners only.
func readSupplies(ctx context.Context, c *snmp.Client) ([]Supply, error) {
	desc, order, err := walkMap(ctx, c, oidSupplyDesc)
	if err != nil || len(order) == 0 {
		return nil, err
	}
	cols := map[string]map[string]snmp.Value{}
	for _, col := range []string{oidSupplyType, oidSupplyMax, oidSupplyLevel, oidSupplyColorant, oidColorantValue} {
		m, _, err := walkMap(ctx, c, col)
		if err != nil {
			return nil, err
		}
		cols[col] = m
	}
	var out []Supply
	for _, idx := range order {
		name := strings.TrimSpace(desc[idx].String())
		if strings.Contains(strings.ToLower(name), "waste") {
			continue
		}
		tv, tok := cols[oidSupplyType][idx]
		if t, ok := intValue(tv, tok); ok && !slices.Contains(tonerTypes, t) {
			continue
		}
		s := Supply{Name: name, Level: -1}
		mv, mok := cols[oidSupplyMax][idx]
		lv, lok := cols[oidSupplyLevel][idx]
		if mx, ok := intValue(mv, mok); ok && mx > 0 {
			if l, ok := intValue(lv, lok); ok && l >= 0 {
				s.Level = int(min(l*100/mx, 100))
			}
		}
		// The colorant table is indexed by hrDeviceIndex.colorantIndex.
		colv, colok := cols[oidSupplyColorant][idx]
		if cv, ok := intValue(colv, colok); ok && cv > 0 {
			dev, _, _ := strings.Cut(idx, ".")
			s.Color = colorName(cols[oidColorantValue][dev+"."+strconv.FormatInt(cv, 10)].String())
		}
		if s.Color == "" {
			s.Color = colorName(name)
		}
		out = append(out, s)
	}
	return out, nil
}

// readRicohToners reads the Ricoh private toner table. low is true when a
// toner reports "near empty" (-3).
func readRicohToners(ctx context.Context, c *snmp.Client) (out []Supply, low bool, err error) {
	names, order, err := walkMap(ctx, c, oidRicohTonerName)
	if err != nil || len(order) == 0 {
		return nil, false, err
	}
	pct, _, err := walkMap(ctx, c, oidRicohTonerPct)
	if err != nil {
		return nil, false, err
	}
	for _, idx := range order {
		name := strings.TrimSpace(names[idx].String())
		if name == "" || strings.Contains(strings.ToLower(name), "waste") {
			continue
		}
		s := Supply{Name: name, Color: colorName(name), Level: -1}
		pv, pok := pct[idx]
		if p, ok := intValue(pv, pok); ok {
			switch {
			case p >= 0 && p <= 100:
				s.Level = int(p)
			case p == -3:
				low = true
			}
		}
		out = append(out, s)
	}
	return out, low, nil
}

// colorName maps a colorant or supply name to the colour name the UI knows
// ("" when none matches).
func colorName(s string) string {
	s = strings.ToLower(s)
	for _, c := range []struct{ key, name string }{
		{"cyan", "cyan"}, {"magenta", "magenta"}, {"yellow", "yellow"}, {"black", "black"},
	} {
		if strings.Contains(s, c.key) {
			return c.name
		}
	}
	return ""
}
