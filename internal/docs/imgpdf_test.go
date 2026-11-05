package docs

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 20), uint8(y * 20), 90, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withEXIF inserts an APP1 Exif segment (given byte order) after SOI.
func withEXIF(jpg []byte, orient uint16, bo interface {
	binary.ByteOrder
	binary.AppendByteOrder
}) []byte {
	tiff := []byte("II")
	if bo == binary.BigEndian {
		tiff = []byte("MM")
	}
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8)
	tiff = bo.AppendUint16(tiff, 1) // one entry
	tiff = bo.AppendUint16(tiff, 0x0112)
	tiff = bo.AppendUint16(tiff, 3)
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint16(tiff, orient)
	tiff = append(tiff, 0, 0)
	tiff = bo.AppendUint32(tiff, 0)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	seg = append(seg, payload...)
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

type parsedPDF struct {
	raw    []byte
	page   string // page dict
	mbox   [4]float64
	cm     [6]float64
	img    string // image dict
	stream []byte // image stream
}

var numRe = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

func floats(s string) []float64 {
	var out []float64
	for _, m := range numRe.FindAllString(s, -1) {
		f, _ := strconv.ParseFloat(m, 64)
		out = append(out, f)
	}
	return out
}

// parsePDF checks structure (header, EOF, xref offsets, stream lengths) and
// extracts what the tests assert on.
func parsePDF(t *testing.T, b []byte) parsedPDF {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("%PDF-")) || !bytes.HasSuffix(b, []byte("%%EOF\n")) {
		t.Fatalf("bad header/trailer")
	}
	i := bytes.LastIndex(b, []byte("startxref\n"))
	if i < 0 {
		t.Fatal("no startxref")
	}
	xoff, err := strconv.Atoi(strings.Fields(string(b[i+10:]))[0])
	if err != nil || !bytes.HasPrefix(b[xoff:], []byte("xref\n0 6\n")) {
		t.Fatalf("startxref %d does not point at xref", xoff)
	}
	lines := strings.Split(string(b[xoff:]), "\n")
	if lines[2] != "0000000000 65535 f " {
		t.Fatalf("free entry %q", lines[2])
	}
	for n := 1; n <= 5; n++ {
		var off int
		var gen string
		if _, err := fmtSscan(lines[2+n], &off, &gen); err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b[off:], []byte(strconv.Itoa(n)+" 0 obj\n")) {
			t.Fatalf("object %d: offset %d points at %q", n, off, b[off:off+12])
		}
	}
	if !strings.Contains(string(b[xoff:]), "/Size 6") || !strings.Contains(string(b[xoff:]), "/Root 1 0 R") {
		t.Fatal("trailer")
	}
	var p parsedPDF
	p.raw = b
	obj := func(n int) string {
		s := bytes.Index(b, []byte("\n"+strconv.Itoa(n)+" 0 obj\n")) + 1
		e := bytes.Index(b[s:], []byte("\nendobj\n"))
		return string(b[s : s+e])
	}
	p.page = obj(3)
	m := regexp.MustCompile(`/MediaBox \[([^\]]*)\]`).FindStringSubmatch(p.page)
	copy(p.mbox[:], floats(m[1]))
	// content stream: exact /Length
	c4 := obj(4)
	lm := regexp.MustCompile(`/Length (\d+)`).FindStringSubmatch(c4)
	n, _ := strconv.Atoi(lm[1])
	cs := c4[strings.Index(c4, "stream\n")+7:]
	cs = strings.TrimSuffix(cs, "endstream")
	if len(cs) != n {
		t.Fatalf("content length %d != %d", n, len(cs))
	}
	cmRe := regexp.MustCompile(`([-\d. ]+) cm\n`).FindStringSubmatch(cs)
	copy(p.cm[:], floats(cmRe[1]))
	// image stream: exact /Length, found by byte offset
	s5 := bytes.Index(b, []byte("5 0 obj\n"))
	hdrEnd := bytes.Index(b[s5:], []byte("stream\n"))
	p.img = string(b[s5 : s5+hdrEnd])
	lm = regexp.MustCompile(`/Length (\d+)`).FindStringSubmatch(p.img)
	n, _ = strconv.Atoi(lm[1])
	st := s5 + hdrEnd + 7
	p.stream = b[st : st+n]
	if !bytes.HasPrefix(b[st+n:], []byte("\nendstream\nendobj\n")) {
		t.Fatalf("image /Length %d is not exact: %q", n, b[st+n:st+n+20])
	}
	return p
}

func fmtSscan(line string, off *int, gen *string) (int, error) {
	f := strings.Fields(line)
	if len(f) != 3 || f[2] != "n" {
		return 0, errors.New("bad xref line " + line)
	}
	v, err := strconv.Atoi(f[0])
	*off, *gen = v, f[1]
	return 3, err
}

func convertFile(t *testing.T, data []byte, w, h float64) parsedPDF {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.bin")
	out := filepath.Join(dir, "out.pdf")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := imageToPDF(in, out, w, h); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	return parsePDF(t, b)
}

func nearTo(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestJPEGRoundTrip(t *testing.T) {
	jpg := testJPEG(t, 30, 40)
	p := convertFile(t, jpg, 595.28, 841.89)
	if !bytes.Equal(p.stream, jpg) {
		t.Error("JPEG bytes must be embedded unchanged")
	}
	for _, s := range []string{"/Filter /DCTDecode", "/ColorSpace /DeviceRGB", "/Width 30 ", "/Height 40 "} {
		if !strings.Contains(p.img, s) {
			t.Errorf("image dict lacks %q: %s", s, p.img)
		}
	}
	if strings.Contains(p.img, "/Decode") {
		t.Error("no Decode for RGB")
	}
}

func TestJPEGGrey(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	p := convertFile(t, b.Bytes(), 0, 0)
	if !strings.Contains(p.img, "/DeviceGray") {
		t.Error(p.img)
	}
}

func TestExifOrientation6(t *testing.T) {
	// 30x40 stored, rotated 90 CW: displays 40x30. Without paper: page = 40x30.
	for _, bo := range []interface {
		binary.ByteOrder
		binary.AppendByteOrder
	}{binary.LittleEndian, binary.BigEndian} {
		jpg := withEXIF(testJPEG(t, 30, 40), 6, bo)
		p := convertFile(t, jpg, 0, 0)
		if p.mbox != [4]float64{0, 0, 40, 30} {
			t.Fatalf("mediabox %v", p.mbox)
		}
		// Rotation by 90 CW: a=0 b=-30 c=40 d=0 e=0 f=30.
		want := [6]float64{0, -30, 40, 0, 0, 30}
		for i := range want {
			if !nearTo(p.cm[i], want[i]) {
				t.Fatalf("cm %v want %v", p.cm, want)
			}
		}
		if !bytes.Equal(p.stream, jpg) {
			t.Error("bytes must stay unchanged")
		}
	}
}

// TestOrientationMatrices maps the stored top-left and top-right corners and
// checks them against the EXIF definition.
func TestOrientationMatrices(t *testing.T) {
	// For a w x h stored image shown on a w' x h' page without paper, the
	// stored (col, row) corners must land on these displayed corners.
	// corners: top-left (a=0,b=0) and top-right (a=1,b=0), in page coords
	// with y up, for a 2x3 image (effective 2x3 or 3x2).
	type pt struct{ x, y float64 }
	cases := []struct {
		o      int
		pw, ph float64
		tl, tr pt
	}{
		{1, 2, 3, pt{0, 3}, pt{2, 3}},
		{2, 2, 3, pt{2, 3}, pt{0, 3}},
		{3, 2, 3, pt{2, 0}, pt{0, 0}},
		{4, 2, 3, pt{0, 0}, pt{2, 0}},
		{5, 3, 2, pt{0, 2}, pt{0, 0}},
		{6, 3, 2, pt{3, 2}, pt{3, 0}},
		{7, 3, 2, pt{3, 0}, pt{3, 2}},
		{8, 3, 2, pt{0, 0}, pt{0, 2}},
	}
	for _, c := range cases {
		im := &pdfImage{w: 2, h: 3, orient: c.o}
		pw, ph, m := placement(im, 0, 0)
		if pw != c.pw || ph != c.ph {
			t.Errorf("o=%d page %vx%v", c.o, pw, ph)
		}
		// unit square (u,v): stored a=u, b=1-v. top-left: u=0,v=1.
		tf := func(u, v float64) pt { return pt{m[0]*u + m[2]*v + m[4], m[1]*u + m[3]*v + m[5]} }
		tl, tr := tf(0, 1), tf(1, 1)
		if !nearTo(tl.x, c.tl.x) || !nearTo(tl.y, c.tl.y) || !nearTo(tr.x, c.tr.x) || !nearTo(tr.y, c.tr.y) {
			t.Errorf("o=%d tl=%v tr=%v want %v %v", c.o, tl, tr, c.tl, c.tr)
		}
	}
}

func TestFitMath(t *testing.T) {
	// 4000x3000 on A4 portrait: landscape page, fitted by height, centred horizontally.
	im := &pdfImage{w: 4000, h: 3000, orient: 1}
	pw, ph, m := placement(im, 595.28, 841.89)
	if !nearTo(pw, 841.89) || !nearTo(ph, 595.28) {
		t.Fatalf("page %v x %v", pw, ph)
	}
	s := 595.28 / 3000 // height-limited on the landscape page
	if !nearTo(m[0], 4000*s) || !nearTo(m[3], 595.28) || !nearTo(m[4], (841.89-4000*s)/2) || !nearTo(m[5], 0) {
		t.Errorf("matrix %v", m)
	}
	// 1000x2000 stays portrait, height-limited, centred horizontally.
	im = &pdfImage{w: 1000, h: 2000, orient: 1}
	pw, ph, m = placement(im, 595.28, 841.89)
	if !nearTo(pw, 595.28) || !nearTo(ph, 841.89) {
		t.Fatalf("page %v x %v", pw, ph)
	}
	s = 841.89 / 2000
	if !nearTo(m[3], 841.89) || !nearTo(m[0], 1000*s) || !nearTo(m[4], (595.28-1000*s)/2) || !nearTo(m[5], 0) {
		t.Errorf("matrix %v", m)
	}
	// Small image is upscaled to fit.
	im = &pdfImage{w: 10, h: 10, orient: 1}
	_, _, m = placement(im, 595.28, 841.89)
	if !nearTo(m[0], 595.28) {
		t.Errorf("upscale: %v", m)
	}
	// End to end, through the file: orientation 6 on a 30x40 JPEG swaps to 40x30
	// (landscape) so the A4 page switches to landscape.
	p := convertFile(t, withEXIF(testJPEG(t, 30, 40), 6, binary.BigEndian), 595.28, 841.89)
	if !nearTo(p.mbox[2], 841.89) || !nearTo(p.mbox[3], 595.28) {
		t.Errorf("mediabox %v", p.mbox)
	}
}

func inflate(t *testing.T, b []byte) []byte {
	t.Helper()
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPNGAlphaOnWhite(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	img.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	img.SetNRGBA(1, 0, color.NRGBA{0, 0, 0, 0})
	img.SetNRGBA(2, 0, color.NRGBA{0, 0, 0, 128})
	p := convertFile(t, mustPNG(t, img), 0, 0)
	if !strings.Contains(p.img, "/DeviceRGB") || !strings.Contains(p.img, "/FlateDecode") {
		t.Fatal(p.img)
	}
	px := inflate(t, p.stream)
	if len(px) != 9 {
		t.Fatalf("len %d", len(px))
	}
	if !bytes.Equal(px[0:3], []byte{255, 0, 0}) {
		t.Errorf("opaque pixel %v", px[0:3])
	}
	if !bytes.Equal(px[3:6], []byte{255, 255, 255}) {
		t.Errorf("transparent pixel %v, want white", px[3:6])
	}
	if px[6] < 126 || px[6] > 129 || px[6] != px[7] || px[7] != px[8] {
		t.Errorf("half-transparent black = %v", px[6:9])
	}
}

func TestGreyDetection(t *testing.T) {
	g := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for i := range g.Pix {
		g.Pix[i] = 100
	}
	p := convertFile(t, mustPNG(t, g), 0, 0)
	if !strings.Contains(p.img, "/DeviceGray") || len(inflate(t, p.stream)) != 4 {
		t.Errorf("grey NRGBA: %s", p.img)
	}
	g.SetNRGBA(1, 1, color.NRGBA{100, 101, 100, 255})
	p = convertFile(t, mustPNG(t, g), 0, 0)
	if !strings.Contains(p.img, "/DeviceRGB") {
		t.Errorf("near-grey must stay RGB: %s", p.img)
	}
	p = convertFile(t, mustPNG(t, image.NewGray(image.Rect(0, 0, 3, 2))), 0, 0)
	if !strings.Contains(p.img, "/DeviceGray") {
		t.Error("Gray image")
	}
}

func mustPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// bmpFile builds an uncompressed BMP. rows are given top to bottom.
func bmpFile(w, h, bpp int, topDown bool, pal [][3]byte, rows [][]byte) []byte {
	stride := (w*bpp + 31) / 32 * 4
	palSize := len(pal) * 4
	off := 14 + 40 + palSize
	b := make([]byte, off)
	copy(b, "BM")
	binary.LittleEndian.PutUint32(b[2:], uint32(off+stride*h))
	binary.LittleEndian.PutUint32(b[10:], uint32(off))
	binary.LittleEndian.PutUint32(b[14:], 40)
	binary.LittleEndian.PutUint32(b[18:], uint32(int32(w)))
	hh := int32(h)
	if topDown {
		hh = -hh
	}
	binary.LittleEndian.PutUint32(b[22:], uint32(hh))
	binary.LittleEndian.PutUint16(b[26:], 1)
	binary.LittleEndian.PutUint16(b[28:], uint16(bpp))
	binary.LittleEndian.PutUint32(b[46:], uint32(len(pal)))
	for i, c := range pal {
		b[54+4*i], b[54+4*i+1], b[54+4*i+2] = c[2], c[1], c[0] // BGR
	}
	order := rows
	if !topDown {
		order = make([][]byte, h)
		for i := range rows {
			order[h-1-i] = rows[i]
		}
	}
	for _, r := range order {
		row := make([]byte, stride)
		copy(row, r)
		b = append(b, row...)
	}
	return b
}

func TestBMP24BottomUp(t *testing.T) {
	// 2x2, top row red,green; bottom row blue,white. BGR byte order.
	rows := [][]byte{{0, 0, 255, 0, 255, 0}, {255, 0, 0, 255, 255, 255}}
	p := convertFile(t, bmpFile(2, 2, 24, false, nil, rows), 0, 0)
	want := []byte{255, 0, 0, 0, 255, 0, 0, 0, 255, 255, 255, 255}
	if got := inflate(t, p.stream); !bytes.Equal(got, want) || !strings.Contains(p.img, "/DeviceRGB") {
		t.Errorf("got %v want %v", got, want)
	}
	// Top-down gives the same picture.
	p = convertFile(t, bmpFile(2, 2, 24, true, nil, rows), 0, 0)
	if got := inflate(t, p.stream); !bytes.Equal(got, want) {
		t.Errorf("top-down got %v", got)
	}
}

func TestBMP32(t *testing.T) {
	// Alpha byte (here 0) is ignored: BI_RGB 32-bit has no real alpha.
	rows := [][]byte{{10, 20, 30, 0, 40, 50, 60, 0}}
	p := convertFile(t, bmpFile(2, 1, 32, false, nil, rows), 0, 0)
	want := []byte{30, 20, 10, 60, 50, 40}
	if got := inflate(t, p.stream); !bytes.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestBMP8Paletted(t *testing.T) {
	pal := [][3]byte{{255, 0, 0}, {0, 0, 255}}
	rows := [][]byte{{0, 1}, {1, 0}}
	p := convertFile(t, bmpFile(2, 2, 8, false, pal, rows), 0, 0)
	want := []byte{255, 0, 0, 0, 0, 255, 0, 0, 255, 255, 0, 0}
	if got := inflate(t, p.stream); !bytes.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	// Grey palette gives DeviceGray.
	gp := [][3]byte{{0, 0, 0}, {200, 200, 200}}
	p = convertFile(t, bmpFile(2, 2, 8, true, gp, rows), 0, 0)
	if !strings.Contains(p.img, "/DeviceGray") || !bytes.Equal(inflate(t, p.stream), []byte{0, 200, 200, 0}) {
		t.Errorf("grey palette: %s", p.img)
	}
}

func TestBMPUnsupported(t *testing.T) {
	b := bmpFile(2, 2, 24, false, nil, [][]byte{{}, {}})
	binary.LittleEndian.PutUint32(b[30:], 1) // BI_RLE8
	in := write(t, "a.bmp", b)
	if err := imageToPDF(in, filepath.Join(t.TempDir(), "o.pdf"), 0, 0); !errors.Is(err, ErrUnsupported) {
		t.Errorf("rle: %v", err)
	}
	b = bmpFile(2, 2, 24, false, nil, [][]byte{{}, {}})
	binary.LittleEndian.PutUint16(b[28:], 16)
	in = write(t, "b.bmp", b)
	if err := imageToPDF(in, filepath.Join(t.TempDir(), "o.pdf"), 0, 0); !errors.Is(err, ErrUnsupported) {
		t.Errorf("16bpp: %v", err)
	}
}

func TestHugeImageRejected(t *testing.T) {
	out := filepath.Join(t.TempDir(), "o.pdf")
	// PNG header only: DecodeConfig reports the size, nothing is allocated.
	err := imageToPDF(write(t, "h.png", ihdrPNG(60000, 60000, 6)), out, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("png: %v", err)
	}
	// BMP header declaring 60000x60000 with no pixel data.
	b := bmpFile(1, 1, 24, false, nil, [][]byte{{}})
	binary.LittleEndian.PutUint32(b[18:], 60000)
	binary.LittleEndian.PutUint32(b[22:], 60000)
	err = imageToPDF(write(t, "h.bmp", b), out, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("bmp: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("no output must be left behind")
	}
}

func TestInvalidImagesNoPanic(t *testing.T) {
	good := testJPEG(t, 8, 8)
	cases := map[string][]byte{
		"empty":      {},
		"jpeg-soi":   {0xFF, 0xD8, 0xFF},
		"jpeg-trunc": good[:len(good)/3],
		"jpeg-junk":  append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF}, make([]byte, 20)...),
		"png-sig":    []byte("\x89PNG\r\n\x1a\n"),
		"png-bogus":  ihdrPNG(10, 10, 2),
		"gif":        []byte("GIF89a\x01\x00"),
		"bmp":        []byte("BM\x00\x00"),
		"bmp-zero":   bmpFile(0, 0, 24, false, nil, nil),
		"text":       []byte("hello"),
	}
	for name, data := range cases {
		out := filepath.Join(t.TempDir(), "o.pdf")
		if err := imageToPDF(write(t, name, data), out, 595, 842); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := imageToPDF(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "o.pdf"), 0, 0); err == nil {
		t.Error("missing input")
	}
	// Malformed EXIF falls back to orientation 1.
	bad := withEXIF(good, 9, binary.LittleEndian)
	if p := convertFile(t, bad, 0, 0); p.mbox != [4]float64{0, 0, 8, 8} {
		t.Errorf("mediabox %v", p.mbox)
	}
	short := append([]byte{}, good[:2]...)
	short = append(short, 0xFF, 0xE1, 0, 12)
	short = append(short, []byte("Exif\x00\x00II*\x00")...)
	short = append(short, good[2:]...)
	convertFile(t, short, 0, 0)
}

func TestAdobeCMYKDecode(t *testing.T) {
	// Splice an Adobe APP14 marker into a CMYK-looking header via the
	// marker scanner and the image dict builder.
	jpg := testJPEG(t, 4, 4)
	adobe := []byte{0xFF, 0xEE, 0, 14, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0}
	spliced := append(append(append([]byte{}, jpg[:2]...), adobe...), jpg[2:]...)
	if a, o := scanJPEGMarkers(spliced); !a || o != 1 {
		t.Errorf("adobe=%v orient=%d", a, o)
	}
	if a, _ := scanJPEGMarkers(jpg); a {
		t.Error("plain JPEG has no Adobe marker")
	}
	// The image dict builder emits the Decode array for Adobe CMYK.
	im := &pdfImage{w: 1, h: 1, cs: "DeviceCMYK", filter: "DCTDecode", decode: "[1 0 1 0 1 0 1 0]", data: []byte{1}, orient: 1}
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := writeImagePDF(bw, im, 0, 0); err != nil {
		t.Fatal(err)
	}
	bw.Flush()
	p := parsePDF(t, buf.Bytes())
	if !strings.Contains(p.img, "/DeviceCMYK") || !strings.Contains(p.img, "/Decode [1 0 1 0 1 0 1 0]") {
		t.Error(p.img)
	}
}

func TestNormalizeImageNative(t *testing.T) {
	old := run
	defer func() { run = old }()
	var calls [][]string
	run = func(ctx context.Context, argv []string) ([]byte, error) { calls = append(calls, argv); return nil, nil }
	tmp := t.TempDir()
	out, err := Normalize(context.Background(), write(t, "a.jpg", testJPEG(t, 10, 10)), KindImage, 595, 842, tmp)
	if err != nil || len(calls) != 0 {
		t.Fatalf("%v calls=%v", err, calls)
	}
	b, _ := os.ReadFile(out)
	parsePDF(t, b)
	// A failing conversion leaves nothing behind.
	before, _ := os.ReadDir(tmp)
	if _, err := Normalize(context.Background(), write(t, "b.png", []byte("\x89PNG\r\n\x1a\nxx")), KindImage, 595, 842, tmp); err == nil {
		t.Fatal("expected error")
	}
	if after, _ := os.ReadDir(tmp); len(after) != len(before) {
		t.Errorf("leaked temp files")
	}
}

func TestNormalizeTIFF(t *testing.T) {
	old := run
	defer func() { run = old }()
	var calls [][]string
	run = func(ctx context.Context, argv []string) ([]byte, error) {
		calls = append(calls, argv)
		for _, a := range argv {
			if o, ok := strings.CutPrefix(a, "-sOutputFile="); ok {
				os.WriteFile(o, []byte("%PDF-"), 0o600)
			}
		}
		if argv[0] == "tiff2pdf" {
			os.WriteFile(argv[2], []byte("%PDF-"), 0o600)
		}
		return nil, nil
	}
	tmp := t.TempDir()
	tif := write(t, "a.tif", []byte("II*\x00\x08\x00\x00\x00"))
	if _, err := Normalize(context.Background(), tif, KindImage, 595.28, 841.89, tmp); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][0] != "tiff2pdf" || calls[1][0] != "gs" ||
		!strings.Contains(strings.Join(calls[1], " "), "-dPDFFitPage") {
		t.Fatalf("calls: %v", calls)
	}
	if m, _ := filepath.Glob(filepath.Join(tmp, "tiff-*")); len(m) != 0 {
		t.Errorf("intermediate not removed: %v", m)
	}
	calls = nil
	if _, err := Normalize(context.Background(), tif, KindImage, 0, 0, tmp); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0][0] != "tiff2pdf" {
		t.Fatalf("no-paper calls: %v", calls)
	}
}

func TestPipelineImageIntegration(t *testing.T) {
	if _, err := exec.LookPath("gs"); err != nil {
		t.Skip("ghostscript not installed")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	in := write(t, "a.jpg", testJPEG(t, 64, 48))
	k, err := Detect(in)
	if err != nil || k != KindImage {
		t.Fatalf("detect: %v %v", k, err)
	}
	pdf, err := Normalize(ctx, in, k, 595.28, 841.89, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := PageCount(ctx, pdf); err != nil || n != 1 {
		t.Fatalf("PageCount = %d, %v", n, err)
	}
}
