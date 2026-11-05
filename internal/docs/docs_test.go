package docs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kipavy/printer-web/internal/config"
)

func write(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetect(t *testing.T) {
	heic := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic")...)
	bmp := append([]byte("BM"), make([]byte, 30)...)
	cases := []struct {
		name string
		data []byte
		want Kind
		err  error
	}{
		{"a.pdf", []byte("%PDF-1.4\n..."), KindPDF, nil},
		{"a.ps", []byte("%!PS-Adobe-3.0\n"), KindPS, nil},
		{"a.jpg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0}, KindImage, nil},
		{"a.png", []byte("\x89PNG\r\n\x1a\nxxxx"), KindImage, nil},
		{"a.gif", []byte("GIF89a\x01\x00"), KindImage, nil},
		{"a.tif", []byte("II*\x00\x08\x00\x00\x00"), KindImage, nil},
		{"b.tif", []byte("MM\x00*\x00\x00\x00\x08"), KindImage, nil},
		{"a.bmp", bmp, KindImage, nil},
		{"a.heic", heic, KindHEIC, nil},
		{"a.txt", []byte("héllo wörld\n€\n"), KindText, nil},
		{"bm.txt", []byte("BM is a text file"), KindText, nil},
		{"bom.pdf", []byte("\xef\xbb\xbf  \n%PDF-1.7"), KindPDF, nil},
		{"zipwithpdf.pdf", []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00...%PDF-1.4..."), 0, ErrUnsupported},
		{"hevc.heic", append([]byte{0, 0, 0, 0x18}, []byte("ftyphevm")...), KindHEIC, nil},
		{"zip.pdf", []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00"), 0, ErrUnsupported},
		{"bin.txt", []byte{0x7f, 'E', 'L', 'F', 2, 1, 1}, 0, ErrUnsupported},
		{"latin1.txt", []byte{'a', 0xE9, 'b'}, 0, ErrUnsupported},
		{"mp4.heic", append([]byte{0, 0, 0, 0x18}, []byte("ftypmp42")...), 0, ErrUnsupported},
		{"empty.pdf", nil, 0, ErrEmpty},
	}
	for _, c := range cases {
		got, err := Detect(write(t, c.name, c.data))
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, got, err, c.want, c.err)
		}
	}
}

func TestDetectTruncatedRune(t *testing.T) {
	data := append(bytes.Repeat([]byte("a"), sniffLen-1), []byte("é")...) // rune split at the window edge
	if k, err := Detect(write(t, "big.txt", data)); err != nil || k != KindText {
		t.Fatalf("got (%v, %v)", k, err)
	}
}

func TestParsePages(t *testing.T) {
	ok := []struct {
		spec string
		n    int
		want []int
	}{
		{"", 3, []int{1, 2, 3}},
		{"  ", 2, []int{1, 2}},
		{"1-3, 5", 6, []int{1, 2, 3, 5}},
		{"4-", 6, []int{4, 5, 6}},
		{"-2", 6, []int{1, 2}},
		{"5,1,3,3", 6, []int{1, 3, 5}},
		{"2-3,3-4", 6, []int{2, 3, 4}},
	}
	for _, c := range ok {
		got, err := ParsePages(c.spec, c.n)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v, %v want %v", c.spec, got, err, c.want)
		}
	}
	for _, s := range []string{"0", "7", "3-1", "a", "1,,2", "-", "1-9", "1-2-3"} {
		if got, err := ParsePages(s, 6); err == nil {
			t.Errorf("%q: expected error, got %v", s, got)
		}
	}
	if _, err := ParsePages("", 0); err == nil {
		t.Error("zero pages must fail")
	}
}

func TestParseInkcov(t *testing.T) {
	out := []byte("0.01234  0.00000  0.00000  0.05678 CMYK OK\n0.10000  0.20000  0.30000  0.40000 CMYK OK\n\nnoise line\n")
	got, err := parseInkcov(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Coverage{{0.01234, 0, 0, 0.05678}, {0.1, 0.2, 0.3, 0.4}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := parseInkcov([]byte("garbage")); err == nil {
		t.Error("no coverage lines must fail")
	}
	if _, err := parseInkcov([]byte("x 0 0 0 CMYK OK")); err == nil {
		t.Error("bad number must fail")
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEstimateCost(t *testing.T) {
	p := config.Pricing{Sheet: 0.01, Toner: map[string]config.Cartridge{"K": {Price: 50, Yield: 2000}}}
	got, ok := EstimateCost([]Coverage{{K: 0.05}}, 1, 1, false, p)
	if !ok || !near(got, 0.035) {
		t.Fatalf("got %v %v want 0.035", got, ok)
	}
	// Mono folds CMY into K: K' = 0.02 + 0.09/3 = 0.05.
	got, _ = EstimateCost([]Coverage{{C: 0.03, M: 0.03, Y: 0.03, K: 0.02}}, 1, 1, true, p)
	if !near(got, 0.035) {
		t.Fatalf("mono: got %v", got)
	}
	// Colour mode ignores colours without a cartridge.
	got, _ = EstimateCost([]Coverage{{C: 0.03, M: 0.03, Y: 0.03, K: 0.02}}, 1, 1, false, p)
	if !near(got, 0.01+0.4*0.025) {
		t.Fatalf("colour w/o cartridges: got %v", got)
	}
	// Duplex: 2 pages on 1 sheet.
	got, _ = EstimateCost([]Coverage{{K: 0.05}, {K: 0.05}}, 1, 1, false, p)
	if !near(got, 0.01+2*0.025) {
		t.Fatalf("duplex: got %v", got)
	}
	// Simplex: 2 sheets.
	got, _ = EstimateCost([]Coverage{{K: 0.05}, {K: 0.05}}, 2, 1, false, p)
	if !near(got, 0.02+2*0.025) {
		t.Fatalf("simplex: got %v", got)
	}
	// Copies multiply toner per copy.
	got, _ = EstimateCost([]Coverage{{K: 0.05}}, 3, 3, false, p)
	if !near(got, 0.03+3*0.025) {
		t.Fatalf("copies: got %v", got)
	}
	// Full colour set.
	full := config.Pricing{Toner: map[string]config.Cartridge{
		"C": {Price: 60, Yield: 1000}, "M": {Price: 60, Yield: 1000},
		"Y": {Price: 60, Yield: 1000}, "K": {Price: 50, Yield: 2000}}}
	got, _ = EstimateCost([]Coverage{{0.05, 0.05, 0.05, 0.05}}, 1, 1, false, full)
	if !near(got, 3*0.06+0.025) {
		t.Fatalf("full: got %v", got)
	}
	if _, ok := EstimateCost([]Coverage{{K: 0.05}}, 1, 1, false, config.Pricing{}); ok {
		t.Error("pricing disabled must return ok=false")
	}
}

func TestNormalizeArgs(t *testing.T) {
	got, err := normalizeArgs(KindImage, "in.jpg", "out.pdf", 595.28, 841.89)
	want := []string{"img2pdf", "--pagesize", "595.28ptx841.89pt", "--fit", "into", "-o", "out.pdf", "in.jpg"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("img: %v %v", got, err)
	}
	got, _ = normalizeArgs(KindImage, "in.jpg", "out.pdf", 0, 0)
	if !reflect.DeepEqual(got, []string{"img2pdf", "-o", "out.pdf", "in.jpg"}) {
		t.Fatalf("img nopaper: %v", got)
	}
	got, _ = normalizeArgs(KindPS, "in.ps", "out.pdf", 612, 792)
	want = append(gsBase(), "-sDEVICE=pdfwrite", "-sOutputFile=out.pdf", "in.ps")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ps: %v", got)
	}
	if _, err := normalizeArgs(KindText, "a", "b", 1, 1); err == nil {
		t.Error("text has no direct argv")
	}
	if !reflect.DeepEqual(heicArgs("a.heic", "a.jpg"), []string{"heif-convert", "a.heic", "a.jpg"}) {
		t.Error("heic argv")
	}
	if !reflect.DeepEqual(enscriptArgs("a.txt", "a.ps", 595, 842), []string{"enscript", "-B", "-q", "--encoding=latin1", "--media=A4", "-o", "a.ps", "a.txt"}) {
		t.Error("enscript argv")
	}
	if !reflect.DeepEqual(psToPDFArgs("a.ps", "a.pdf"), append(gsBase(), "-sDEVICE=pdfwrite", "-sOutputFile=a.pdf", "a.ps")) {
		t.Error("ps2pdf argv")
	}
}

func TestToLatin1(t *testing.T) {
	got := toLatin1([]byte("café €"))
	want := []byte{'c', 'a', 'f', 0xE9, ' ', '?'}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if !bytes.Equal(toLatin1([]byte{0xFF}), []byte{'?'}) {
		t.Error("invalid UTF-8 must become ?")
	}
}

func hasSafe(a []string) bool {
	has := map[string]bool{}
	for _, x := range a {
		has[x] = true
	}
	return a[0] == "gs" && has["-dSAFER"] && has["-dBATCH"] && has["-dNOPAUSE"] && has["-q"]
}

func TestGsAlwaysSafe(t *testing.T) {
	all := [][]string{
		pageCountArgs("a.pdf"),
		prepareArgs("a.pdf", []int{1}, "o.pdf"),
		inkcovArgs("a.pdf"),
		psToPDFArgs("a.ps", "a.pdf"),
	}
	a, _ := normalizeArgs(KindPS, "a.ps", "a.pdf", 0, 0)
	all = append(all, a)
	for _, f := range []string{"image/pwg-raster", "image/urf", "application/postscript", "application/vnd.hp-pcl"} {
		for _, color := range []bool{true, false} {
			a, err := convertArgs("a.pdf", f, color, "o")
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, a)
		}
	}
	for _, a := range all {
		if !hasSafe(a) {
			t.Errorf("missing safety flags: %v", a)
		}
	}
}

func TestConvertArgs(t *testing.T) {
	dev := func(f string, color bool) string {
		a, err := convertArgs("in.pdf", f, color, "out")
		if err != nil {
			t.Fatal(err)
		}
		if a[len(a)-1] != "in.pdf" || a[len(a)-2] != "-sOutputFile=out" {
			t.Fatalf("bad tail: %v", a)
		}
		for _, x := range a {
			if strings.HasPrefix(x, "-sDEVICE=") {
				return strings.TrimPrefix(x, "-sDEVICE=")
			}
		}
		t.Fatalf("no device: %v", a)
		return ""
	}
	for f, want := range map[string]string{
		"image/pwg-raster":       "pwgraster",
		"image/urf":              "appleraster",
		"application/postscript": "ps2write",
	} {
		if got := dev(f, true); got != want {
			t.Errorf("%s: %s want %s", f, got, want)
		}
	}
	if dev("application/vnd.hp-pcl", true) != "pxlcolor" || dev("application/vnd.hp-pcl", false) != "pxlmono" {
		t.Error("pcl devices")
	}
	a, _ := convertArgs("in.pdf", "image/pwg-raster", true, "out")
	if !contains(a, "-r300") {
		t.Errorf("pwg must be 300 dpi: %v", a)
	}
	for _, f := range []string{"image/pwg-raster", "image/urf"} {
		m, _ := convertArgs("in.pdf", f, false, "out")
		c, _ := convertArgs("in.pdf", f, true, "out")
		if !contains(m, "-dcupsColorSpace=18") || !contains(c, "-dcupsColorSpace=19") || !contains(m, "-dcupsBitsPerColor=8") || !contains(c, "-r300") || !contains(m, "-r300") {
			t.Errorf("%s cups params: %v | %v", f, m, c)
		}
	}
	if _, err := convertArgs("in.pdf", "text/html", true, "out"); err == nil {
		t.Error("unknown format must fail")
	}
}

func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

func TestPageCountArgsQuoting(t *testing.T) {
	a := pageCountArgs(`C:\a (b)\x.pdf`)
	if !contains(a, `--permit-file-read=C:\a (b)\x.pdf`) || !contains(a, `(C:\\a \(b\)\\x.pdf) (r) file runpdfbegin pdfpagecount = quit`) {
		t.Fatalf("got %q", a)
	}
}

func TestPrepareArgs(t *testing.T) {
	a := prepareArgs("in.pdf", []int{1, 3, 5}, "out.pdf")
	if !contains(a, "-sPageList=1,3,5") || a[len(a)-1] != "in.pdf" {
		t.Fatalf("got %v", a)
	}
}

func TestConvertPDFNoop(t *testing.T) {
	abs, _ := filepath.Abs("x.pdf")
	got, err := Convert(context.Background(), "x.pdf", "application/pdf", true, t.TempDir())
	if err != nil || got != abs {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := Convert(context.Background(), "x.pdf", "bogus/type", true, t.TempDir()); err == nil {
		t.Error("unknown format must fail")
	}
}

func TestRunErrorCapturesStderr(t *testing.T) {
	t.Setenv("DOCS_HELPER", "1") // re-exec this binary as a failing tool
	_, err := run(context.Background(), []string{os.Args[0], "-test.run=TestHelperFail"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("stderr missing: %v", err)
	}
	if len(err.Error()) > 2*maxTail+len(os.Args[0])+200 {
		t.Fatalf("error not truncated: %d", len(err.Error()))
	}
}

func TestHelperFail(t *testing.T) {
	if os.Getenv("DOCS_HELPER") != "1" {
		return
	}
	if os.Getenv("DOCS_HELPER_MODE") == "sleep" {
		time.Sleep(30 * time.Second)
	}
	if os.Getenv("DOCS_HELPER_MODE") == "stdout" {
		os.Stdout.WriteString("Error: /undefined in foo\n")
		os.Exit(1)
	}
	os.Stderr.WriteString(strings.Repeat("x", 5000) + " boom")
	os.Exit(3)
}

func TestNormalizeWithFakeRunner(t *testing.T) {
	var calls [][]string
	old := run
	defer func() { run = old }()
	run = func(ctx context.Context, argv []string) ([]byte, error) {
		calls = append(calls, argv)
		return nil, nil
	}
	tmp := t.TempDir()
	txt := write(t, "a.txt", []byte("héllo €"))
	out, err := Normalize(context.Background(), txt, KindText, 595, 842, tmp)
	if err != nil || filepath.Dir(out) != tmp {
		t.Fatalf("got %q %v", out, err)
	}
	if len(calls) != 2 || calls[0][0] != "enscript" || calls[1][0] != "gs" {
		t.Fatalf("calls: %v", calls)
	}
	calls = nil
	// heif-convert fake: creates out.jpg, or out-1.jpg+out-2.jpg when told so.
	for _, names := range [][]string{{"out.jpg"}, {"out-2.jpg", "out-1.jpg"}} {
		calls = nil
		run = func(ctx context.Context, argv []string) ([]byte, error) {
			calls = append(calls, argv)
			if argv[0] == "heif-convert" {
				dir := filepath.Dir(argv[2])
				if filepath.Base(argv[2]) != "out.jpg" || filepath.Dir(dir) != tmp {
					t.Errorf("bad heif target %s", argv[2])
				}
				for _, n := range names {
					os.WriteFile(filepath.Join(dir, n), []byte("j"), 0o600)
				}
			}
			return nil, nil
		}
		heic := write(t, "a.heic", []byte("x"))
		if _, err := Normalize(context.Background(), heic, KindHEIC, 595, 842, tmp); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 2 || calls[0][0] != "heif-convert" || calls[1][0] != "img2pdf" {
			t.Fatalf("heic calls: %v", calls)
		}
		if filepath.Base(calls[1][len(calls[1])-1]) != names[len(names)-1] {
			t.Errorf("img2pdf input: %v", calls[1])
		}
		if m, _ := filepath.Glob(filepath.Join(tmp, "heic-*")); len(m) != 0 {
			t.Errorf("heic dir not removed: %v", m)
		}
	}
	// heif-convert producing nothing is an error.
	run = func(ctx context.Context, argv []string) ([]byte, error) { return nil, nil }
	if _, err := Normalize(context.Background(), write(t, "b.heic", []byte("x")), KindHEIC, 0, 0, tmp); err == nil {
		t.Error("missing heif output must fail")
	}
	// PDFs pass through untouched.
	if p, _ := Normalize(context.Background(), "x.pdf", KindPDF, 0, 0, tmp); p != mustAbs("x.pdf") {
		t.Error("pdf passthrough")
	}
	// Failure removes the temp output.
	run = func(ctx context.Context, argv []string) ([]byte, error) { return nil, errors.New("fail") }
	before, _ := os.ReadDir(tmp)
	if _, err := Normalize(context.Background(), write(t, "b.ps", []byte("%!PS")), KindPS, 0, 0, tmp); err == nil {
		t.Fatal("expected error")
	}
	if after, _ := os.ReadDir(tmp); len(after) != len(before) {
		t.Errorf("leaked temp files: %d -> %d", len(before), len(after))
	}
}

func TestPipelineIntegration(t *testing.T) {
	if _, err := exec.LookPath("gs"); err != nil {
		t.Skip("ghostscript not installed")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	ps := write(t, "two.ps", []byte("%!PS\n/Helvetica findfont 24 scalefont setfont\n"+
		"100 700 moveto (Page 1) show showpage\n100 700 moveto 1 0 0 setrgbcolor (Page 2) show showpage\n"))
	k, err := Detect(ps)
	if err != nil || k != KindPS {
		t.Fatalf("detect: %v %v", k, err)
	}
	pdf, err := Normalize(ctx, ps, k, 595.28, 841.89, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := PageCount(ctx, pdf); err != nil || n != 2 {
		t.Fatalf("PageCount = %d, %v", n, err)
	}
	cov, err := InkCoverage(ctx, pdf)
	if err != nil || len(cov) != 2 {
		t.Fatalf("InkCoverage = %v, %v", cov, err)
	}
	one, err := Prepare(ctx, pdf, []int{2}, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := PageCount(ctx, one); err != nil || n != 1 {
		t.Fatalf("prepared PageCount = %d, %v", n, err)
	}
	for _, c := range []struct{ format, magic string }{
		{"application/pdf", "%PDF"},
		{"image/pwg-raster", "RaS2"},
		{"image/urf", "UNIRAST"},
		{"application/postscript", "%!PS"},
		{"application/vnd.hp-pcl", ") HP-PCL XL"},
	} {
		for _, color := range []bool{true, false} {
			out, err := Convert(ctx, one, c.format, color, tmp)
			if err != nil {
				t.Errorf("%s color=%v: %v", c.format, color, err)
				continue
			}
			b, err := os.ReadFile(out)
			if err != nil || len(b) == 0 {
				t.Errorf("%s: empty output (%v)", c.format, err)
				continue
			}
			if !bytes.Contains(b[:min(len(b), 512)], []byte(c.magic)) {
				t.Errorf("%s color=%v: magic %q not found in %q", c.format, color, c.magic, b[:min(len(b), 40)])
			}
			if c.format == "image/pwg-raster" && len(b) >= 408 {
				// PWG 5102.4: 4-byte sync, then the 1796-byte header; cupsColorSpace
				// is at header offset 400 (after cupsColorOrder at 396).
				cs := binary.BigEndian.Uint32(b[4+400:])
				want := uint32(18)
				if color {
					want = 19
				}
				if cs != want {
					t.Errorf("pwg color=%v: cupsColorSpace = %d, want %d", color, cs, want)
				}
			}
		}
	}
}

func mustAbs(p string) string { a, _ := filepath.Abs(p); return a }

func TestRunErrorIncludesStdout(t *testing.T) {
	t.Setenv("DOCS_HELPER", "1")
	t.Setenv("DOCS_HELPER_MODE", "stdout")
	_, err := run(context.Background(), []string{os.Args[0], "-test.run=TestHelperFail"})
	if err == nil || !strings.Contains(err.Error(), "Error: /undefined in foo") {
		t.Fatalf("stdout missing from error: %v", err)
	}
}

func TestTailRuneBoundary(t *testing.T) {
	s := tail([]byte(strings.Repeat("é", 400)), 511) // cuts mid-rune
	if !utf8.ValidString(s) {
		t.Fatal("tail must be valid UTF-8")
	}
}

func TestPageCountLastLine(t *testing.T) {
	old := run
	defer func() { run = old }()
	run = func(ctx context.Context, argv []string) ([]byte, error) {
		return []byte("Warning: something\r\n 7 \r\n\r\n"), nil
	}
	if n, err := PageCount(context.Background(), "a.pdf"); err != nil || n != 7 {
		t.Fatalf("got %d %v", n, err)
	}
	run = func(ctx context.Context, argv []string) ([]byte, error) { return []byte("junk\n"), nil }
	if _, err := PageCount(context.Background(), "a.pdf"); err == nil {
		t.Error("junk must fail")
	}
}

func TestToLatin1BOM(t *testing.T) {
	if got := toLatin1([]byte("\xef\xbb\xbfab")); string(got) != "ab" {
		t.Fatalf("got %q", got)
	}
}

func TestEnscriptMedia(t *testing.T) {
	for _, c := range []struct {
		w, h float64
		want string
	}{{595.28, 841.89, "A4"}, {612, 792, "Letter"}, {0, 0, "A4"}, {300, 400, "A4"}} {
		if got := enscriptMedia(c.w, c.h); got != c.want {
			t.Errorf("%v x %v: %s want %s", c.w, c.h, got, c.want)
		}
	}
}

func writePNG(t *testing.T, name string, img image.Image) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return write(t, name, b.Bytes())
}

func TestFlattenPNG(t *testing.T) {
	tmp := t.TempDir()
	rgba := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	rgba.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	rgba.SetNRGBA(1, 0, color.NRGBA{0, 0, 0, 0}) // transparent
	in := writePNG(t, "a.png", rgba)
	out, err := flattenPNG(in, tmp)
	if err != nil || out == "" {
		t.Fatalf("got %q %v", out, err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if !isOpaque(img) {
		t.Error("output must be opaque")
	}
	if r, g, b, a := img.At(1, 0).RGBA(); r != 0xffff || g != 0xffff || b != 0xffff || a != 0xffff {
		t.Errorf("transparent pixel = %x %x %x %x, want white", r, g, b, a)
	}
	if r, g, b, _ := img.At(0, 0).RGBA(); r != 0xffff || g != 0 || b != 0 {
		t.Error("opaque pixel changed")
	}
	// Fully opaque RGBA and non-PNG files are left alone.
	op := image.NewRGBA(image.Rect(0, 0, 1, 1))
	op.Set(0, 0, color.RGBA{1, 2, 3, 255})
	if out, err := flattenPNG(writePNG(t, "o.png", op), tmp); err != nil || out != "" {
		t.Errorf("opaque png: %q %v", out, err)
	}
	if out, err := flattenPNG(write(t, "j.jpg", []byte{0xFF, 0xD8, 0xFF, 0, 0, 0, 0, 0, 0}), tmp); err != nil || out != "" {
		t.Errorf("jpeg: %q %v", out, err)
	}
}

func TestNormalizeFlattensPNG(t *testing.T) {
	old := run
	defer func() { run = old }()
	var argvs [][]string
	run = func(ctx context.Context, argv []string) ([]byte, error) { argvs = append(argvs, argv); return nil, nil }
	tmp := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	in := writePNG(t, "t.png", img) // fully transparent
	if _, err := Normalize(context.Background(), in, KindImage, 595, 842, tmp); err != nil {
		t.Fatal(err)
	}
	if len(argvs) != 1 || argvs[0][len(argvs[0])-1] == in {
		t.Fatalf("img2pdf must read the flattened copy: %v", argvs)
	}
	if m, _ := filepath.Glob(filepath.Join(tmp, "flat-*")); len(m) != 0 {
		t.Errorf("flattened copy not removed: %v", m)
	}
}

func chunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

func ihdrPNG(w, h uint32, colorType byte, extra ...[]byte) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, colorType
	b := append([]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", ihdr)...)
	for _, e := range extra {
		b = append(b, e...)
	}
	return append(b, chunk("IDAT", []byte{0})...)
}

func TestFlattenPNGBounds(t *testing.T) {
	tmp := t.TempDir()
	huge := write(t, "huge.png", ihdrPNG(60000, 60000, 6))
	if out, err := flattenPNG(huge, tmp); err == nil || out != "" || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("got %q %v", out, err)
	}
	// Opaque RGB with a bogus IDAT is returned unchanged without decoding.
	rgb := write(t, "rgb.png", ihdrPNG(10, 10, 2))
	if out, err := flattenPNG(rgb, tmp); err != nil || out != "" {
		t.Fatalf("rgb: %q %v", out, err)
	}
	// RGB with tRNS must be decoded (here it fails on the bogus data).
	trns := write(t, "trns.png", ihdrPNG(10, 10, 2, chunk("tRNS", []byte{0, 0, 0, 0, 0, 0})))
	if _, err := flattenPNG(trns, tmp); err == nil {
		t.Error("tRNS PNG must be decoded")
	}
}

func TestRunWrapsDeadline(t *testing.T) {
	t.Setenv("DOCS_HELPER", "1")
	t.Setenv("DOCS_HELPER_MODE", "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := run(ctx, []string{os.Args[0], "-test.run=TestHelperFail"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
