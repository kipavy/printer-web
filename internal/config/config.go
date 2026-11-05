// Package config reads the application configuration from environment variables.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Caps are printer capabilities declared by the operator.
type Caps struct {
	Color, Duplex bool
	Papers, Trays []string
}

// Cartridge is a toner price and its ISO yield (pages at 5 % coverage).
type Cartridge struct {
	Price float64
	Yield int
}

// Pricing holds the optional cost-per-page settings.
type Pricing struct {
	Currency string
	Sheet    float64
	Toner    map[string]Cartridge // keys "C", "M", "Y", "K"
}

// Enabled reports whether any price is configured.
func (p Pricing) Enabled() bool { return p.Sheet > 0 || len(p.Toner) > 0 }

// Config is the full application configuration.
type Config struct {
	ListenAddr      string
	PublicURL       string
	PrinterName     string
	PrinterLocation string
	Backend         string // ipp | cups | raw | command
	PrinterURI      string
	PrinterProbe    string // optional host:port checked by the command backend
	TLSInsecure     bool
	RawLanguage     string // ps | pcl
	Command         []string
	Declared        Caps
	MaxUploadBytes  int64
	DraftTTL        time.Duration
	QueueShowNames  bool
	DefaultLang     string // auto | fr | en
	Model           string
	PPM, DPI        int
	Pricing         Pricing
}

// FromEnv loads the configuration from the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }

type loader struct {
	get func(string) string
	err error
}

func (l *loader) str(key, def string) string {
	if v := strings.TrimSpace(l.get(key)); v != "" {
		return v
	}
	return def
}

func (l *loader) fail(key string, format string, a ...any) {
	if l.err == nil {
		l.err = fmt.Errorf("%s %s", key, fmt.Sprintf(format, a...))
	}
}

func (l *loader) boolean(key string, def bool) bool {
	v := strings.ToLower(l.str(key, ""))
	switch v {
	case "":
		return def
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	l.fail(key, "must be a boolean (true/false/1/0/yes/no), got %q", v)
	return def
}

func (l *loader) integer(key string, def int64) int64 {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		l.fail(key, "must be a non-negative integer, got %q", v)
		return def
	}
	return n
}

func (l *loader) float(key string) (float64, bool) {
	v := l.str(key, "")
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		l.fail(key, "must be a non-negative number (decimal point), got %q", v)
		return 0, false
	}
	return f, true
}

func (l *loader) list(key string, def []string) []string {
	var out []string
	for _, p := range strings.Split(l.get(key), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func (l *loader) oneOf(key, def string, allowed ...string) string {
	v := strings.ToLower(l.str(key, def))
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.fail(key, "must be one of %s, got %q", strings.Join(allowed, "|"), v)
	return def
}

// Load builds a Config using getenv to read variables.
func Load(getenv func(string) string) (Config, error) {
	l := &loader{get: getenv}
	c := Config{
		ListenAddr:      l.str("LISTEN_ADDR", ":28431"),
		PublicURL:       l.str("PUBLIC_URL", ""),
		PrinterName:     l.str("PRINTER_NAME", "Printer"),
		PrinterLocation: l.str("PRINTER_LOCATION", ""),
		Backend:         l.oneOf("PRINTER_BACKEND", "ipp", "ipp", "cups", "raw", "command"),
		PrinterURI:      l.str("PRINTER_URI", ""),
		PrinterProbe:    l.str("PRINTER_PROBE", ""),
		TLSInsecure:     l.boolean("PRINTER_TLS_INSECURE", false),
		RawLanguage:     l.oneOf("PRINTER_RAW_LANGUAGE", "ps", "ps", "pcl"),
		DefaultLang:     l.oneOf("DEFAULT_LANG", "auto", "auto", "fr", "en"),
		QueueShowNames:  l.boolean("QUEUE_SHOW_NAMES", false),
		Model:           l.str("PRINTER_MODEL", ""),
		PPM:             int(l.integer("PRINTER_PPM", 0)),
		DPI:             int(l.integer("PRINTER_DPI", 0)),
		MaxUploadBytes:  l.integer("MAX_UPLOAD_MB", 50) << 20,
		Declared: Caps{
			Color:  l.boolean("PRINTER_COLOR", false),
			Duplex: l.boolean("PRINTER_DUPLEX", false),
			Papers: l.list("PRINTER_PAPERS", []string{"A4"}),
			Trays:  l.list("PRINTER_TRAYS", nil),
		},
	}
	if v := l.str("DRAFT_TTL", ""); v == "" {
		c.DraftTTL = 15 * time.Minute
	} else if d, err := time.ParseDuration(v); err != nil || d <= 0 {
		l.fail("DRAFT_TTL", "must be a positive duration like 15m, got %q", v)
	} else {
		c.DraftTTL = d
	}
	if c.MaxUploadBytes <= 0 && l.err == nil {
		l.fail("MAX_UPLOAD_MB", "must be greater than 0")
	}
	if c.PublicURL != "" {
		c.PublicURL = strings.TrimRight(c.PublicURL, "/")
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			l.fail("PUBLIC_URL", "must be an http:// or https:// address with a host, e.g. http://192.168.1.10:28431, got %q", c.PublicURL)
		}
	}
	if c.PrinterProbe != "" {
		h, p, err := net.SplitHostPort(c.PrinterProbe)
		if n, perr := strconv.Atoi(p); err != nil || h == "" || perr != nil || n < 1 || n > 65535 {
			l.fail("PRINTER_PROBE", "must be host:port, e.g. 192.168.1.50:9100, got %q", c.PrinterProbe)
		}
	}
	c.Pricing = loadPricing(l)
	if l.err != nil {
		return Config{}, l.err
	}
	if err := c.validateBackend(l.get("PRINTER_COMMAND")); err != nil {
		return Config{}, err
	}
	return c, nil
}

func loadPricing(l *loader) Pricing {
	p := Pricing{Currency: l.str("PRICE_CURRENCY", "€")}
	p.Sheet, _ = l.float("PRICE_SHEET")
	for _, k := range []string{"C", "M", "Y", "K"} {
		pk, yk := "PRICE_TONER_"+k, "YIELD_TONER_"+k
		price, hasP := l.float(pk)
		yield := l.integer(yk, 0)
		hasY := l.str(yk, "") != ""
		if hasP != hasY || (hasY && yield == 0) {
			l.fail(pk, "and %s must both be set (yield > 0)", yk)
			continue
		}
		if hasP {
			if p.Toner == nil {
				p.Toner = map[string]Cartridge{}
			}
			p.Toner[k] = Cartridge{Price: price, Yield: int(yield)}
		}
	}
	return p
}

func (c *Config) validateBackend(rawCmd string) error {
	switch c.Backend {
	case "ipp", "cups":
		// http:// and https:// are accepted as aliases of ipp:// and ipps://.
		u, err := url.Parse(c.PrinterURI)
		ok := err == nil && u.Hostname() != ""
		if ok {
			switch u.Scheme {
			case "ipp", "ipps", "http", "https":
			default:
				ok = false
			}
		}
		if !ok {
			return fmt.Errorf("PRINTER_URI must be an ipp:// or ipps:// URI for backend %q, e.g. ipp://192.168.1.50/ipp/print, got %q", c.Backend, c.PrinterURI)
		}
	case "raw":
		u, err := url.Parse(c.PrinterURI)
		if err != nil || u.Scheme != "socket" || u.Hostname() == "" {
			return fmt.Errorf("PRINTER_URI must be socket://host[:port] for backend raw, got %q", c.PrinterURI)
		}
		if u.Port() == "" {
			c.PrinterURI = "socket://" + u.Hostname() + ":9100"
			if strings.Contains(u.Hostname(), ":") {
				c.PrinterURI = "socket://[" + u.Hostname() + "]:9100"
			}
		}
	case "command":
		raw := strings.TrimSpace(rawCmd)
		var argv []string
		if err := json.Unmarshal([]byte(raw), &argv); err != nil {
			return fmt.Errorf(`PRINTER_COMMAND must be a JSON array of strings, e.g. ["prog","{file}"], got %q. `+
				`Hint: in an --env-file, write it without surrounding quotes; in compose YAML, wrap the whole value in single quotes`, raw)
		}
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return errors.New("PRINTER_COMMAND must be a non-empty JSON array whose first element is not empty")
		}
		c.Command = argv
	}
	return nil
}
