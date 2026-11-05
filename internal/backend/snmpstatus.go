package backend

import (
	"context"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kipavy/printer-web/internal/snmp"
)

// snmpTimeout bounds one whole SNMP read (all its requests). It stays under
// the API's 6 s status timeout.
const snmpTimeout = 4 * time.Second

// snmpAttempt is the wait for one answer before the one retry.
const snmpAttempt = time.Second

// OIDs read over SNMP (RFC 3805 Printer-MIB, RFC 2790 Host Resources MIB and
// the Ricoh private MIB).
const (
	oidSupplyColorant = "1.3.6.1.2.1.43.11.1.1.3" // prtMarkerSuppliesColorantIndex
	oidSupplyClass    = "1.3.6.1.2.1.43.11.1.1.4" // prtMarkerSuppliesClass
	oidSupplyType     = "1.3.6.1.2.1.43.11.1.1.5" // prtMarkerSuppliesType
	oidSupplyDesc     = "1.3.6.1.2.1.43.11.1.1.6" // prtMarkerSuppliesDescription
	oidSupplyMax      = "1.3.6.1.2.1.43.11.1.1.8" // prtMarkerSuppliesMaxCapacity
	oidSupplyLevel    = "1.3.6.1.2.1.43.11.1.1.9" // prtMarkerSuppliesLevel
	oidColorantValue  = "1.3.6.1.2.1.43.12.1.1.4" // prtMarkerColorantValue
	oidDeviceType     = "1.3.6.1.2.1.25.3.2.1.2"  // hrDeviceType
	oidDeviceStatus   = "1.3.6.1.2.1.25.3.2.1.5"  // hrDeviceStatus
	oidErrorState     = "1.3.6.1.2.1.25.3.5.1.2"  // hrPrinterDetectedErrorState
	oidDevicePrinter  = "1.3.6.1.2.1.25.3.1.5"    // hrDeviceType value: printer
	oidRicohTonerName = "1.3.6.1.4.1.367.3.2.1.2.24.1.1.3"
	oidRicohTonerPct  = "1.3.6.1.4.1.367.3.2.1.2.24.1.1.5"
)

// classReceptacle is prtMarkerSuppliesClass receptacle(4): waste bins.
const classReceptacle = 4

// hiddenType reports prtMarkerSuppliesType values that are not toner or
// ink: wasteToner(4), wasteInk(8), opc(9), developer(10), fuserOil(11),
// and wasteWax(14) up to covers(34) except tonerCartridge(21). other(1) and
// unknown(2) are kept.
func hiddenType(t int64) bool {
	switch t {
	case 4, 8, 9, 10, 11:
		return true
	}
	return t >= 14 && t != 21
}

// errorBits maps hrPrinterDetectedErrorState bits (bit 0 is the high bit of
// the first byte) to printer-state-reasons keywords the UI knows.
// serviceRequested(7), outputTrayMissing(9), outputNearFull(11) and
// overduePreventMaint(14) have no keyword.
var errorBits = map[int]string{
	0:  "media-low",
	1:  "media-empty",
	2:  "toner-low",
	3:  "toner-empty",
	4:  "door-open",
	5:  "media-jam",
	6:  "offline",
	8:  "input-tray-missing",
	10: "marker-supply-missing",
	12: "output-area-full",
	13: "media-needed", // inputTrayEmpty: one tray, maybe not the only one
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
	c.Timeout = snmpAttempt
	return &snmpReader{client: c, cache: newCache(cacheTTL)}
}

// read returns the cached info. A failed read keeps what was found before
// the failure (possibly nothing).
func (r *snmpReader) read(ctx context.Context) snmpInfo {
	v, err := r.cache.do(ctx, "snmp", func() (any, error) {
		ctx, cancel := context.WithTimeout(ctx, snmpTimeout)
		defer cancel()
		return fetchSNMP(ctx, r.client)
	})
	if ctx.Err() == nil {
		r.note(err)
	}
	info, _ := v.(snmpInfo)
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
		log.Printf("snmp: read failed, supplies or alerts may be missing: %v", err)
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

// fetchSNMP reads the printer state, then the supplies, on one socket. When
// the supplies fail, the state already read is returned with the error.
func fetchSNMP(ctx context.Context, c *snmp.Client) (snmpInfo, error) {
	var info snmpInfo
	s, err := c.Open(ctx)
	if err != nil {
		return info, err
	}
	defer s.Close()

	dev, err := s.WalkColumns(ctx, oidErrorState, oidDeviceStatus, oidDeviceType)
	if err != nil {
		return info, err
	}
	if idx, ok := printerRow(dev); ok {
		if v, ok := row(dev[oidErrorState], oidErrorState, idx); ok && v.Type == snmp.TypeOctetString {
			info.reasons = errorReasons(v.Bytes)
		}
		if v, ok := row(dev[oidDeviceStatus], oidDeviceStatus, idx); ok && v.Type == snmp.TypeInteger && v.Int == 5 {
			info.down = true
		}
	}

	sup, err := readSupplies(ctx, s)
	if err != nil {
		return info, err
	}
	info.supplies = sup
	if !slices.ContainsFunc(sup, func(s Supply) bool { return s.Level >= 0 }) {
		ricoh, extra, err := readRicohToners(ctx, s)
		if err != nil {
			return info, err
		}
		if len(ricoh) > 0 {
			info.supplies = ricoh
		}
		for _, x := range extra {
			if !slices.Contains(info.reasons, x) {
				info.reasons = append(info.reasons, x)
			}
		}
	}
	return info, nil
}

// printerRow picks the hrDeviceIndex of the printer: the first row of the
// printer table, else the first device of type printer.
func printerRow(dev map[string][]snmp.Var) (string, bool) {
	if vs := dev[oidErrorState]; len(vs) > 0 {
		return strings.TrimPrefix(vs[0].OID, oidErrorState+"."), true
	}
	for _, v := range dev[oidDeviceType] {
		if v.Value.Type == snmp.TypeOID && v.Value.OID == oidDevicePrinter {
			return strings.TrimPrefix(v.OID, oidDeviceType+"."), true
		}
	}
	return "", false
}

// row finds the value of one row in a walked column.
func row(vs []snmp.Var, col, idx string) (snmp.Value, bool) {
	for _, v := range vs {
		if v.OID == col+"."+idx {
			return v.Value, true
		}
	}
	return snmp.Value{}, false
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

// byIndex keys a walked column by row index (the OID suffix after the
// column) and keeps the row order.
func byIndex(vs []snmp.Var, col string) (map[string]snmp.Value, []string) {
	m := map[string]snmp.Value{}
	var order []string
	for _, v := range vs {
		idx := strings.TrimPrefix(v.OID, col+".")
		if _, dup := m[idx]; !dup {
			order = append(order, idx)
		}
		m[idx] = v.Value
	}
	return m, order
}

func intValue(v snmp.Value, ok bool) (int64, bool) {
	return v.Int, ok && v.Type == snmp.TypeInteger
}

// readSupplies reads the Printer-MIB supplies table, toners only.
func readSupplies(ctx context.Context, s *snmp.Session) ([]Supply, error) {
	cols := []string{oidSupplyDesc, oidSupplyClass, oidSupplyType, oidSupplyMax, oidSupplyLevel, oidSupplyColorant, oidColorantValue}
	walked, err := s.WalkColumns(ctx, cols...)
	if err != nil {
		return nil, err
	}
	desc, order := byIndex(walked[oidSupplyDesc], oidSupplyDesc)
	m := map[string]map[string]snmp.Value{}
	for _, col := range cols[1:] {
		m[col], _ = byIndex(walked[col], col)
	}
	get := func(col, idx string) (int64, bool) {
		v, ok := m[col][idx]
		return intValue(v, ok)
	}
	var out []Supply
	for _, idx := range order {
		name := strings.TrimSpace(desc[idx].String())
		// Waste bins: by class, else by name.
		if class, ok := get(oidSupplyClass, idx); ok {
			if class == classReceptacle {
				continue
			}
		} else if strings.Contains(strings.ToLower(name), "waste") {
			continue
		}
		if t, ok := get(oidSupplyType, idx); ok && hiddenType(t) {
			continue
		}
		sp := Supply{Name: name, Level: -1}
		if mx, ok := get(oidSupplyMax, idx); ok && mx > 0 {
			if l, ok := get(oidSupplyLevel, idx); ok && l >= 0 {
				sp.Level = int(min(l, mx) * 100 / mx)
			}
		}
		// The colorant table is indexed by hrDeviceIndex.colorantIndex.
		if ci, ok := get(oidSupplyColorant, idx); ok && ci > 0 {
			dev, _, _ := strings.Cut(idx, ".")
			sp.Color = colorName(m[oidColorantValue][dev+"."+strconv.FormatInt(ci, 10)].String())
		}
		if sp.Color == "" {
			sp.Color = colorName(name)
		}
		out = append(out, sp)
	}
	return out, nil
}

// readRicohToners reads the Ricoh private toner table. reasons holds
// toner-low for "near empty" (-3) and toner-empty for 0 %.
func readRicohToners(ctx context.Context, s *snmp.Session) (out []Supply, reasons []string, err error) {
	walked, err := s.WalkColumns(ctx, oidRicohTonerName, oidRicohTonerPct)
	if err != nil {
		return nil, nil, err
	}
	names, order := byIndex(walked[oidRicohTonerName], oidRicohTonerName)
	pct, _ := byIndex(walked[oidRicohTonerPct], oidRicohTonerPct)
	add := func(r string) {
		if !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	for _, idx := range order {
		name := strings.TrimSpace(names[idx].String())
		if name == "" || strings.Contains(strings.ToLower(name), "waste") {
			continue
		}
		sp := Supply{Name: name, Color: colorName(name), Level: -1}
		pv, pok := pct[idx]
		if p, ok := intValue(pv, pok); ok {
			switch {
			case p == 0:
				sp.Level = 0
				add("toner-empty")
			case p > 0 && p <= 100:
				sp.Level = int(p)
			case p == -3:
				add("toner-low")
			}
		}
		out = append(out, sp)
	}
	return out, reasons, nil
}

// colorName maps a colorant or supply name to the colour name the UI knows
// ("" when none matches).
func colorName(s string) string {
	s = strings.ToLower(s)
	for _, c := range []string{"cyan", "magenta", "yellow", "black"} {
		if strings.Contains(s, c) {
			return c
		}
	}
	return ""
}
