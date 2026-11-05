package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"PRINTER_URI": "ipp://192.168.1.50/ipp/print"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":28431" || c.PrinterName != "Printer" || c.Backend != "ipp" ||
		c.RawLanguage != "ps" || c.MaxUploadBytes != 50<<20 || c.DraftTTL != 15*time.Minute ||
		c.QueueShowNames || c.DefaultLang != "auto" || c.TLSInsecure || c.Declared.Duplex || c.Declared.Color {
		t.Fatalf("bad defaults: %+v", c)
	}
	if !reflect.DeepEqual(c.Declared.Papers, []string{"A4"}) || len(c.Declared.Trays) != 0 {
		t.Fatalf("bad caps: %+v", c.Declared)
	}
	if c.Pricing.Enabled() || c.Pricing.Currency != "€" {
		t.Fatalf("bad pricing: %+v", c.Pricing)
	}
}

func TestBackendValidation(t *testing.T) {
	cases := []struct {
		name, backend, uri string
		ok                 bool
	}{
		{"ipp ok", "ipp", "ipp://192.168.1.50/ipp/print", true},
		{"ipps ok", "ipp", "ipps://printer.local:631/ipp/print", true},
		{"ipp http alias", "ipp", "http://printer.local:631/ipp/print", true},
		{"ipp https alias", "ipp", "https://printer.local/ipp/print", true},
		{"ipp bad", "ipp", "ftp://printer.local", false},
		{"ipp nohost", "ipp", "ipp:///ipp/print", false},
		{"ipp empty", "ipp", "", false},
		{"cups ok", "cups", "ipp://cups.local:631/printers/Q", true},
		{"cups bad", "cups", "socket://printer.local", false},
		{"raw ok", "raw", "socket://printer.local:9100", true},
		{"raw noport", "raw", "socket://printer.local", true},
		{"raw bad", "raw", "ipp://printer.local", false},
		{"raw nohost", "raw", "socket://", false},
		{"unknown", "lpd", "ipp://printer.local", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(map[string]string{"PRINTER_BACKEND": tc.backend, "PRINTER_URI": tc.uri}))
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v want ok=%v", err, tc.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "PRINTER_") {
				t.Fatalf("error should name variable: %v", err)
			}
			if tc.name == "raw noport" && c.PrinterURI != "socket://printer.local:9100" {
				t.Fatalf("default port not applied: %q", c.PrinterURI)
			}
		})
	}
	if _, err := Load(env(map[string]string{"PRINTER_BACKEND": "lpd"})); err == nil || !strings.Contains(err.Error(), "PRINTER_BACKEND") {
		t.Fatalf("unknown backend error: %v", err)
	}
	if _, err := Load(env(map[string]string{"PRINTER_URI": "ipp://p", "PRINTER_RAW_LANGUAGE": "zpl"})); err == nil || !strings.Contains(err.Error(), "PRINTER_RAW_LANGUAGE") {
		t.Fatalf("raw language error: %v", err)
	}
	if _, err := Load(env(map[string]string{"PRINTER_URI": "ipp://p", "DEFAULT_LANG": "de"})); err == nil || !strings.Contains(err.Error(), "DEFAULT_LANG") {
		t.Fatalf("lang error: %v", err)
	}
}

func TestCommandJSON(t *testing.T) {
	c, err := Load(env(map[string]string{"PRINTER_BACKEND": "command", "PRINTER_COMMAND": `["ricoh-spc240","print","{file}"]`}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Command, []string{"ricoh-spc240", "print", "{file}"}) {
		t.Fatalf("got %v", c.Command)
	}
	bad := []string{"ricoh-spc240 print", "", "[]", `[""]`, `[1]`}
	for _, b := range bad {
		_, err := Load(env(map[string]string{"PRINTER_BACKEND": "command", "PRINTER_COMMAND": b}))
		if err == nil || !strings.Contains(err.Error(), "PRINTER_COMMAND") {
			t.Fatalf("%q: err=%v", b, err)
		}
	}
	_, err = Load(env(map[string]string{"PRINTER_BACKEND": "command", "PRINTER_COMMAND": "ricoh-spc240 print"}))
	if !strings.Contains(err.Error(), "PRINTER_COMMAND must be a JSON array") {
		t.Fatalf("msg: %v", err)
	}
}

func TestDeclaredCaps(t *testing.T) {
	c, err := Load(env(map[string]string{
		"PRINTER_URI": "ipp://p", "PRINTER_COLOR": "YES", "PRINTER_DUPLEX": "1",
		"PRINTER_PAPERS": " A4, A3 ,,Letter", "PRINTER_TRAYS": "auto, bypass",
		"MAX_UPLOAD_MB": "10", "DRAFT_TTL": "30s", "QUEUE_SHOW_NAMES": "true",
		"PRINTER_TLS_INSECURE": "1", "PPM": "", "PRINTER_PPM": "24", "PRINTER_DPI": "600",
	}))
	if err != nil {
		t.Fatal(err)
	}
	d := c.Declared
	if !d.Color || !d.Duplex || !reflect.DeepEqual(d.Papers, []string{"A4", "A3", "Letter"}) ||
		!reflect.DeepEqual(d.Trays, []string{"auto", "bypass"}) {
		t.Fatalf("caps: %+v", d)
	}
	if c.MaxUploadBytes != 10<<20 || c.DraftTTL != 30*time.Second || !c.QueueShowNames || !c.TLSInsecure || c.PPM != 24 || c.DPI != 600 {
		t.Fatalf("cfg: %+v", c)
	}
	for k, v := range map[string]string{"PRINTER_COLOR": "maybe", "MAX_UPLOAD_MB": "x", "DRAFT_TTL": "soon", "PRINTER_PPM": "1.5"} {
		_, err := Load(env(map[string]string{"PRINTER_URI": "ipp://p", k: v}))
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("%s=%s: %v", k, v, err)
		}
	}
}

func TestPricing(t *testing.T) {
	base := map[string]string{"PRINTER_URI": "ipp://p"}
	with := func(kv ...string) func(string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return env(m)
	}
	c, err := Load(with("PRICE_SHEET", "0.01", "PRICE_CURRENCY", "$", "PRICE_TONER_K", "80.5", "YIELD_TONER_K", "12000"))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pricing
	if !p.Enabled() || p.Currency != "$" || p.Sheet != 0.01 || p.Toner["K"] != (Cartridge{80.5, 12000}) || len(p.Toner) != 1 {
		t.Fatalf("pricing: %+v", p)
	}
	if _, err := Load(with("PRICE_TONER_C", "50")); err == nil || !strings.Contains(err.Error(), "PRICE_TONER_C") || !strings.Contains(err.Error(), "YIELD_TONER_C") {
		t.Fatalf("missing yield: %v", err)
	}
	if _, err := Load(with("YIELD_TONER_M", "5000")); err == nil || !strings.Contains(err.Error(), "PRICE_TONER_M") {
		t.Fatalf("missing price: %v", err)
	}
	if _, err := Load(with("PRICE_SHEET", "abc")); err == nil || !strings.Contains(err.Error(), "PRICE_SHEET") {
		t.Fatalf("bad sheet: %v", err)
	}
	if c, _ := Load(with("PRICE_SHEET", "0.02")); !c.Pricing.Enabled() {
		t.Fatal("sheet alone enables")
	}
}

func TestCommandParseErrorHint(t *testing.T) {
	_, err := Load(env(map[string]string{"PRINTER_BACKEND": "command", "PRINTER_COMMAND": `'["lp","{file}"]'`}))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`got "'[\"lp\",\"{file}\"]'"`, "--env-file", "single quotes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestPublicURL(t *testing.T) {
	base := map[string]string{"PRINTER_URI": "ipp://192.168.1.50/ipp/print"}
	for in, want := range map[string]string{
		"http://192.168.1.50:28431/": "http://192.168.1.50:28431",
		"https://printer.local//":    "https://printer.local",
		"http://printer.local:28431": "http://printer.local:28431",
		"192.168.1.50:28431":         "",
		"ftp://printer.local":        "",
		"http://":                    "",
		"printer.local":              "",
	} {
		base["PUBLIC_URL"] = in
		c, err := Load(env(base))
		if want == "" {
			if err == nil || !strings.Contains(err.Error(), "PUBLIC_URL") {
				t.Errorf("%q: err = %v", in, err)
			}
			continue
		}
		if err != nil || c.PublicURL != want {
			t.Errorf("%q: %q %v", in, c.PublicURL, err)
		}
	}
}

func TestMaxUploadPositive(t *testing.T) {
	_, err := Load(env(map[string]string{"PRINTER_URI": "ipp://192.168.1.50/ipp/print", "MAX_UPLOAD_MB": "0"}))
	if err == nil || !strings.Contains(err.Error(), "MAX_UPLOAD_MB") {
		t.Errorf("err = %v", err)
	}
}

func TestPrinterProbe(t *testing.T) {
	for in, ok := range map[string]bool{
		"192.168.1.50:9100":   true,
		"printer.local:9100":  true,
		"[::1]:9100":          true,
		"192.168.1.50":        false,
		":9100":               false,
		"printer.local:x":     false,
		"printer.local:70000": false,
	} {
		c, err := Load(env(map[string]string{"PRINTER_BACKEND": "command", "PRINTER_COMMAND": `["true"]`, "PRINTER_PROBE": in}))
		if (err == nil) != ok {
			t.Errorf("%q: err = %v", in, err)
		}
		if ok && c.PrinterProbe != in {
			t.Errorf("%q: got %q", in, c.PrinterProbe)
		}
	}
}

func TestPrinterSNMP(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.50":          "192.168.1.50:161",
		"192.168.1.50:1161":     "192.168.1.50:1161",
		"printer.local":         "printer.local:161",
		"[::1]:161":             "[::1]:161",
		"::1":                   "[::1]:161",
		"[::1]":                 "[::1]:161",
		":161":                  "",
		"printer.local:x":       "",
		"printer.local:0":       "",
		"printer.local:7000000": "",
		"http://192.168.1.50":   "",
		"a b":                   "",
		"1:2:3:zz":              "",
	} {
		c, err := Load(env(map[string]string{"PRINTER_BACKEND": "command", "PRINTER_COMMAND": `["true"]`, "PRINTER_SNMP": in}))
		if (err == nil) != (want != "") {
			t.Errorf("%q: err = %v", in, err)
		}
		if err == nil && c.SNMP != want {
			t.Errorf("%q: got %q, want %q", in, c.SNMP, want)
		}
	}
	c, err := Load(env(map[string]string{"PRINTER_BACKEND": "command", "PRINTER_COMMAND": `["true"]`}))
	if err != nil || c.SNMP != "" || c.SNMPCommunity != "public" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, _ = Load(env(map[string]string{"PRINTER_BACKEND": "command", "PRINTER_COMMAND": `["true"]`, "PRINTER_SNMP_COMMUNITY": "office"}))
	if c.SNMPCommunity != "office" {
		t.Fatalf("community %q", c.SNMPCommunity)
	}
}
