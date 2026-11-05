package docs

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJPEGRGBNoColorTransform(t *testing.T) {
	jpg := testJPEG(t, 8, 8)
	i := bytes.Index(jpg, []byte{0xFF, 0xC0})
	if i < 0 {
		t.Fatal("no SOF0")
	}
	// Component ids R,G,B make the stdlib (and PDF readers) treat it as RGB.
	rgb := append([]byte{}, jpg...)
	rgb[i+10], rgb[i+13], rgb[i+16] = 'R', 'G', 'B'
	// Same through an Adobe APP14 marker with transform 0.
	adobe := []byte{0xFF, 0xEE, 0, 14, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0}
	app14 := append(append(append([]byte{}, jpg[:2]...), adobe...), jpg[2:]...)
	for name, data := range map[string][]byte{"ids": rgb, "app14": app14} {
		p := convertFile(t, data, 0, 0)
		if !strings.Contains(p.img, "/DeviceRGB") || !strings.Contains(p.img, "/DecodeParms << /ColorTransform 0 >>") {
			t.Errorf("%s: %s", name, p.img)
		}
	}
	// Ordinary YCbCr keeps the default transform.
	if p := convertFile(t, jpg, 0, 0); strings.Contains(p.img, "ColorTransform") {
		t.Error("YCbCr must not set ColorTransform")
	}
}

func TestJPEGCMYKHeader(t *testing.T) {
	// SOI, Adobe APP14, SOF0 with 4 components: enough for DecodeConfig.
	jfif := []byte{0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0} // lets DecodeConfig stop at the SOF
	b := append([]byte{0xFF, 0xD8}, jfif...)
	adobe := []byte{0xFF, 0xEE, 0, 14, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0}
	b = append(b, adobe...)
	b = append(b, 0xFF, 0xC0, 0, 20, 8, 0, 6, 0, 5, 4)
	for id := byte(1); id <= 4; id++ {
		b = append(b, id, 0x11, 0)
	}
	b = append(b, 0xFF, 0xD9)
	im, err := jpegImage(b)
	if err != nil {
		t.Fatal(err)
	}
	if im.cs != "DeviceCMYK" || im.decode != "[1 0 1 0 1 0 1 0]" || im.w != 5 || im.h != 6 {
		t.Errorf("%+v", im)
	}
	// Without the Adobe marker no inversion is requested.
	noAdobe := bytes.Replace(b, adobe, nil, 1)
	if im, err := jpegImage(noAdobe); err != nil || im.decode != "" || im.cs != "DeviceCMYK" {
		t.Errorf("%+v %v", im, err)
	}
}

func TestRasterSpeed(t *testing.T) {
	start := time.Now()
	for _, img := range []image.Image{
		image.NewRGBA(image.Rect(0, 0, 2000, 2000)),
		image.NewNRGBA(image.Rect(0, 0, 2000, 2000)),
		image.NewGray(image.Rect(0, 0, 2000, 2000)),
	} {
		if _, err := rasterImage(img); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("3 x 4 MP took %v", d)
	}
}

func TestRGBAPremultiplied(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.SetRGBA(0, 0, color.RGBA{0, 0, 0, 0})     // transparent
	img.SetRGBA(1, 0, color.RGBA{100, 0, 0, 100}) // premultiplied red at 100/255
	im, err := rasterImage(img)
	if err != nil {
		t.Fatal(err)
	}
	px := inflate(t, im.data)
	if !bytes.Equal(px[:3], []byte{255, 255, 255}) || px[3] != 255 || px[4] != 155 || px[5] != 155 {
		t.Errorf("got %v", px)
	}
}

func TestEmptyPaletteAndPaper(t *testing.T) {
	if _, err := rasterImage(image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{})); err == nil {
		t.Error("empty palette must fail")
	}
	in := write(t, "a.jpg", testJPEG(t, 4, 4))
	out := filepath.Join(t.TempDir(), "o.pdf")
	for _, c := range [][2]float64{{-1, 100}, {100, -1}, {math.NaN(), 1}, {1, math.Inf(1)}} {
		if err := imageToPDF(in, out, c[0], c[1]); err == nil {
			t.Errorf("paper %v accepted", c)
		}
	}
}

func TestHugeOver50MP(t *testing.T) {
	err := imageToPDF(write(t, "h.png", ihdrPNG(8000, 7000, 2)), filepath.Join(t.TempDir(), "o.pdf"), 0, 0)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("56 MP accepted: %v", err)
	}
}

func TestBMPShortPalette(t *testing.T) {
	pal := [][3]byte{{255, 0, 0}, {0, 0, 255}}
	b := bmpFile(2, 1, 8, false, pal, [][]byte{{0, 1}})
	binary.LittleEndian.PutUint32(b[46:], 0) // biClrUsed 0 means 256, but only 2 exist
	p := convertFile(t, b, 0, 0)
	if got := inflate(t, p.stream); !bytes.Equal(got, []byte{255, 0, 0, 0, 0, 255}) {
		t.Errorf("got %v", got)
	}
	// An index past the short palette is clamped, not a panic.
	b = bmpFile(2, 1, 8, false, pal, [][]byte{{0, 9}})
	binary.LittleEndian.PutUint32(b[46:], 0)
	convertFile(t, b, 0, 0)
}

func TestHostileEXIF(t *testing.T) {
	le := binary.LittleEndian
	tiff := func(off uint32, count uint16, tail []byte) []byte {
		b := []byte("II")
		b = le.AppendUint16(b, 42)
		b = le.AppendUint32(b, off)
		b = le.AppendUint16(b, count)
		return append(b, tail...)
	}
	for name, b := range map[string][]byte{
		"offset past end": tiff(0x7fffffff, 1, nil),
		"offset max":      tiff(0xffffffff, 1, nil),
		"count 0xFFFF":    tiff(8, 0xFFFF, make([]byte, 24)),
		"truncated entry": tiff(8, 1, make([]byte, 5)),
		"tiny":            []byte("II*"),
		"bad magic":       append([]byte("II"), make([]byte, 20)...),
	} {
		if o := exifOrientation(b); o != 0 {
			t.Errorf("%s: orientation %d", name, o)
		}
	}
	good := testJPEG(t, 8, 8)
	app1 := func(payload []byte) []byte {
		n := len(payload) + 2
		return append([]byte{0xFF, 0xE1, byte(n >> 8), byte(n)}, payload...)
	}
	exif := func(o uint16) []byte { return withEXIF([]byte{0xFF, 0xD8}, o, le)[2:] }
	build := func(segs ...[]byte) []byte {
		out := []byte{0xFF, 0xD8}
		for _, s := range segs {
			out = append(out, s...)
		}
		return append(out, good[2:]...)
	}
	xmp := app1([]byte("http://ns.adobe.com/xap/1.0/\x00<x/>"))
	if _, o := scanJPEGMarkers(build(xmp, exif(6))); o != 6 {
		t.Errorf("XMP before Exif: %d", o)
	}
	if _, o := scanJPEGMarkers(build(exif(6), exif(3))); o != 6 {
		t.Errorf("two Exif segments: first must win, got %d", o)
	}
	if _, o := scanJPEGMarkers(build(exif(9), exif(3))); o != 1 {
		t.Errorf("invalid first Exif must not fall through: %d", o)
	}
}

func FuzzExifOrientation(f *testing.F) {
	le := binary.LittleEndian
	seed := []byte("II")
	seed = le.AppendUint16(seed, 42)
	seed = le.AppendUint32(seed, 8)
	seed = le.AppendUint16(seed, 1)
	seed = le.AppendUint16(seed, 0x0112)
	seed = le.AppendUint16(seed, 3)
	seed = le.AppendUint32(seed, 1)
	seed = le.AppendUint16(seed, 6)
	f.Add(seed)
	f.Add([]byte("MM\x00*\xff\xff\xff\xff\xff\xff"))
	f.Add([]byte("II*\x00\x08\x00\x00\x00\xff\xff"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		if o := exifOrientation(b); o < 0 || o > 8 {
			t.Fatalf("orientation %d", o)
		}
		n := len(b) + 8
		if n > 0xFFFF {
			return
		}
		seg := append([]byte{0xFF, 0xD8, 0xFF, 0xE1, byte(n >> 8), byte(n)}, []byte("Exif\x00\x00")...)
		scanJPEGMarkers(append(seg, b...))
	})
}

func TestParseMediaBox(t *testing.T) {
	for _, c := range []struct {
		in   string
		w, h float64
		ok   bool
	}{
		{"<< /Type /Page /MediaBox [0 0 841.89 595.28] >>", 841.89, 595.28, true},
		{"/MediaBox[ 10 20 110 220 ]", 100, 200, true},
		{"/MediaBox [0 0 612 792]/Rotate 0", 612, 792, true},
		{"/MediaBox [0 0 612]", 0, 0, false},
		{"/MediaBox [0 0 a b]", 0, 0, false},
		{"/MediaBox [0 0 0 0]", 0, 0, false},
		{"/MediaBox [0 0 612 792", 0, 0, false},
		{"/MediaBox 5", 0, 0, false},
		{"nothing", 0, 0, false},
	} {
		w, h, ok := parseMediaBox([]byte(c.in))
		if ok != c.ok || (ok && (!near2(w, c.w) || !near2(h, c.h))) {
			t.Errorf("%q: %v %v %v", c.in, w, h, ok)
		}
	}
}

func near2(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestNormalizeTIFFLandscape(t *testing.T) {
	old := run
	defer func() { run = old }()
	var fit []string
	run = func(ctx context.Context, argv []string) ([]byte, error) {
		if argv[0] == "tiff2pdf" {
			return nil, os.WriteFile(argv[2], []byte("%PDF-1.1\n3 0 obj\n<< /MediaBox [0 0 800 600] >>\n"), 0o600)
		}
		fit = argv
		return nil, nil
	}
	tif := write(t, "a.tif", []byte("II*\x00\x08\x00\x00\x00"))
	if _, err := Normalize(context.Background(), tif, KindImage, 595.28, 841.89, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	j := strings.Join(fit, " ")
	if !strings.Contains(j, "-dDEVICEWIDTHPOINTS=841.89") || !strings.Contains(j, "-dDEVICEHEIGHTPOINTS=595.28") {
		t.Errorf("landscape TIFF must swap the sheet: %s", j)
	}
}
