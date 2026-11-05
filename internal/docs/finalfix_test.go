package docs

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertArgsFitMedia(t *testing.T) {
	for _, f := range []string{"image/pwg-raster", "image/urf", "application/postscript", "application/vnd.hp-pcl"} {
		// A landscape sheet size is turned to portrait.
		a, err := convertArgs("in.pdf", f, true, 841.89, 595.28, "out", "-dDuplex=true")
		if err != nil {
			t.Fatal(err)
		}
		j := strings.Join(a, " ")
		for _, want := range []string{"-dFIXEDMEDIA", "-dPDFFitPage", "-dDEVICEWIDTHPOINTS=595.28", "-dDEVICEHEIGHTPOINTS=841.89", "-dDuplex=true"} {
			if !contains(a, want) {
				t.Errorf("%s: missing %s in %s", f, want, j)
			}
		}
		if a[len(a)-1] != "in.pdf" || a[len(a)-2] != "-sOutputFile=out" {
			t.Errorf("%s: bad tail %s", f, j)
		}
	}
	a, _ := convertArgs("in.pdf", "image/urf", true, 0, 0, "out")
	if contains(a, "-dFIXEDMEDIA") {
		t.Errorf("no paper means no fit: %v", a)
	}
}

func TestConvertPassesPaper(t *testing.T) {
	old := run
	defer func() { run = old }()
	var got []string
	run = func(_ context.Context, argv []string) ([]byte, error) { got = argv; return nil, nil }
	out, err := Convert(context.Background(), "in.pdf", "image/pwg-raster", false, 612, 792, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	if !contains(got, "-dDEVICEWIDTHPOINTS=612") || !contains(got, "-dDEVICEHEIGHTPOINTS=792") {
		t.Errorf("argv %v", got)
	}
}

func TestPdfwriteNeverRotates(t *testing.T) {
	ps, _ := normalizeArgs(KindPS, "a.ps", "a.pdf", 0, 0)
	for _, a := range [][]string{prepareArgs("a.pdf", []int{1}, "o.pdf"), psToPDFArgs("a.ps", "a.pdf"), tiffFitArgs("a", "b", 1, 2), ps} {
		if !contains(a, "-dAutoRotatePages=/None") {
			t.Errorf("missing no-rotate: %v", a)
		}
	}
}

func TestHeicArgsFallback(t *testing.T) {
	old := lookPath
	defer func() { lookPath = old }()
	have := map[string]bool{}
	lookPath = func(n string) (string, error) {
		if have[n] {
			return "/usr/bin/" + n, nil
		}
		return "", errors.New("not found")
	}
	if a := heicArgs("i", "o"); a[0] != "heif-convert" {
		t.Errorf("neither installed: %v", a)
	}
	have["heif-dec"] = true
	if a := heicArgs("i", "o"); a[0] != "heif-dec" || a[1] != "i" || a[2] != "o" {
		t.Errorf("heif-dec only: %v", a)
	}
	have["heif-convert"] = true
	if a := heicArgs("i", "o"); a[0] != "heif-convert" {
		t.Errorf("both: %v", a)
	}
}

func TestJPEGEndMotionPhoto(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 16, 16)), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	jpg := b.Bytes()
	video := append([]byte("\x00\x00\x00\x18ftypmp42"), bytes.Repeat([]byte{0xFF, 0xD9, 0x01}, 10)...)
	if got := jpegEnd(append(append([]byte(nil), jpg...), video...)); !bytes.Equal(got, jpg) {
		t.Errorf("motion photo not truncated: %d bytes, want %d", len(got), len(jpg))
	}
	if got := jpegEnd(jpg); !bytes.Equal(got, jpg) {
		t.Error("plain JPEG changed")
	}

	// Hand-made scan with 0xFF00 stuffing, an RSTn marker and fill bytes,
	// and an APP segment hiding a fake EOI.
	soi := []byte{0xFF, 0xD8}
	app := []byte{0xFF, 0xE1, 0x00, 0x06, 0xFF, 0xD9, 0xAA, 0xBB}
	sos := []byte{0xFF, 0xDA, 0x00, 0x08, 1, 1, 0, 0, 63, 0}
	scan := []byte{0x12, 0xFF, 0x00, 0x34, 0xFF, 0xD0, 0x56, 0xFF, 0xFF, 0xD1, 0x78}
	img := append(append(append(append(append([]byte{}, soi...), app...), sos...), scan...), 0xFF, 0xD9)
	tail := []byte("trailing video \xFF\xD9")
	if got := jpegEnd(append(append([]byte{}, img...), tail...)); !bytes.Equal(got, img) {
		t.Errorf("got %x\nwant %x", got, img)
	}
	// Progressive: two scans, then EOI.
	two := append(append(append(append([]byte{}, soi...), sos...), scan...), sos...)
	two = append(append(two, scan...), 0xFF, 0xD9)
	if got := jpegEnd(append(append([]byte{}, two...), tail...)); !bytes.Equal(got, two) {
		t.Error("progressive scans")
	}
	// Malformed: no EOI, returned unchanged.
	bad := append(append([]byte{}, soi...), sos...)
	bad = append(bad, 1, 2, 3)
	if got := jpegEnd(bad); !bytes.Equal(got, bad) {
		t.Error("malformed changed")
	}
}

func TestMotionPhotoEmbedsOnlyImage(t *testing.T) {
	jpg := testJPEG(t, 20, 10)
	in := append(append([]byte(nil), jpg...), []byte("MOTIONPHOTO-VIDEO-DATA")...)
	p := convertFile(t, in, 595.28, 841.89)
	if !bytes.Equal(p.stream, jpg) {
		t.Errorf("embedded %d bytes, want %d", len(p.stream), len(jpg))
	}
}

func TestFirstPageOrientation(t *testing.T) {
	for in, want := range map[string]string{
		"1 0 obj\n<< /Type /Page /MediaBox [0 0 841.89 595.28] >>\nendobj":          "landscape",
		"1 0 obj\n<< /Type /Page /MediaBox [0 0 595.28 841.89] >>\nendobj":          "portrait",
		"1 0 obj\n<< /Type /Page /MediaBox [0 0 595 842] /Rotate 90 >>\nendobj":     "landscape",
		"1 0 obj\n<< /Type /Page /Rotate 270 /MediaBox [0 0 842 595] >>\nendobj":    "portrait",
		"1 0 obj\n<< /MediaBox [0 0 842 595] >>\nendobj\n2 0 obj\n<< /Rotate 90 >>": "landscape",
		"1 0 obj\n<< /MediaBox [0 0 500 500] >>\nendobj":                            "",
		"%PDF-1.5 compressed": "",
	} {
		p := filepath.Join(t.TempDir(), "a.pdf")
		if err := os.WriteFile(p, []byte(in), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := FirstPageOrientation(p); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if FirstPageOrientation(filepath.Join(t.TempDir(), "missing.pdf")) != "" {
		t.Error("missing file")
	}
}
