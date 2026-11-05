package docs

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"unicode/utf8"
)

func pt(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// normalizeArgs builds the argv turning a PS or image file into a PDF.
// Text and HEIC need several steps and are handled in Normalize.
func normalizeArgs(k Kind, in, out string, paperW, paperH float64) ([]string, error) {
	switch k {
	case KindPS:
		// No -dFIXEDMEDIA/-dPDFFitPage: those only apply to PDF input and
		// would clip or misplace PostScript that sets its own page size.
		a := append(gsBase(), "-sDEVICE=pdfwrite")
		return append(a, "-sOutputFile="+out, in), nil
	case KindImage:
		a := []string{"img2pdf"}
		if paperW > 0 && paperH > 0 {
			a = append(a, "--pagesize", pt(paperW)+"ptx"+pt(paperH)+"pt", "--fit", "into")
		}
		return append(a, "-o", out, in), nil
	}
	return nil, fmt.Errorf("no direct conversion for kind %d", k)
}

func heicArgs(in, out string) []string { return []string{"heif-convert", in, out} }

// enscriptMedia picks A4 unless the printer paper is about US Letter.
func enscriptMedia(paperW, paperH float64) string {
	if math.Abs(paperW-612) < 3 && math.Abs(paperH-792) < 3 {
		return "Letter"
	}
	return "A4"
}

func enscriptArgs(in, out string, paperW, paperH float64) []string {
	return []string{"enscript", "-B", "-q", "--encoding=latin1", "--media=" + enscriptMedia(paperW, paperH), "-o", out, in}
}

func psToPDFArgs(in, out string) []string {
	return append(gsBase(), "-sDEVICE=pdfwrite", "-sOutputFile="+out, in)
}

// toLatin1 converts UTF-8 to ISO-8859-1, replacing unmappable runes with '?'.
// A leading BOM is dropped.
func toLatin1(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		b = b[n:]
		if r < 0x100 && r != utf8.RuneError {
			out = append(out, byte(r))
		} else {
			out = append(out, '?')
		}
	}
	return out
}

// flattenPNG writes an opaque copy of a PNG that has transparency, drawn on
// white (img2pdf would otherwise emit an alpha channel printers render black).
// It returns "" when the file needs no change.
func flattenPNG(in, tmp string) (string, error) {
	f, err := os.Open(in)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var magic [8]byte
	if _, err := f.Read(magic[:]); err != nil || string(magic[:]) != "\x89PNG\r\n\x1a\n" {
		return "", nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return "", err
	}
	img, err := png.Decode(f)
	if err != nil {
		return "", err
	}
	if isOpaque(img) {
		return "", nil
	}
	b := img.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, b, img, b.Min, draw.Over)
	out, err := tempPath(tmp, "flat-*.png")
	if err != nil {
		return "", err
	}
	w, err := os.Create(out)
	if err != nil {
		removeQuiet(out)
		return "", err
	}
	err = png.Encode(w, dst)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		removeQuiet(out)
		return "", err
	}
	return out, nil
}

func isOpaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}

// heicToJPEG converts with heif-convert into a private dir; multi-image files
// yield out-1.jpg, out-2.jpg... and the first one is used. The returned
// cleanup removes the dir.
func heicToJPEG(ctx context.Context, in, tmp string) (jpg string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(tmp, "heic-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	want := filepath.Join(dir, "out.jpg")
	if _, err := run(ctx, heicArgs(in, want)); err != nil {
		return "", cleanup, err
	}
	if _, err := os.Stat(want); err == nil {
		return want, cleanup, nil
	}
	m, _ := filepath.Glob(filepath.Join(dir, "out-*.jpg"))
	if len(m) == 0 {
		return "", cleanup, fmt.Errorf("heif-convert produced no JPEG")
	}
	sort.Strings(m)
	return m[0], cleanup, nil
}

// Normalize converts an input file to a PDF inside tmp. PDFs are returned as is.
func Normalize(ctx context.Context, in string, k Kind, paperW, paperH float64, tmp string) (string, error) {
	in, err := filepath.Abs(in)
	if err != nil {
		return "", err
	}
	if k == KindPDF {
		return in, nil
	}
	out, err := tempPath(tmp, "norm-*.pdf")
	if err != nil {
		return "", err
	}
	out, _ = filepath.Abs(out)
	fail := func(err error) (string, error) { removeQuiet(out); return "", err }
	img := func(src string) error {
		argv, err := normalizeArgs(KindImage, src, out, paperW, paperH)
		if err != nil {
			return err
		}
		_, err = run(ctx, argv)
		return err
	}
	switch k {
	case KindPS:
		argv, _ := normalizeArgs(k, in, out, paperW, paperH)
		if _, err := run(ctx, argv); err != nil {
			return fail(err)
		}
	case KindImage:
		src := in
		flat, err := flattenPNG(in, tmp)
		if err != nil {
			return fail(err)
		}
		if flat != "" {
			defer removeQuiet(flat)
			src, _ = filepath.Abs(flat)
		}
		if err := img(src); err != nil {
			return fail(err)
		}
	case KindHEIC:
		jpg, cleanup, err := heicToJPEG(ctx, in, tmp)
		defer cleanup()
		if err != nil {
			return fail(err)
		}
		jpg, _ = filepath.Abs(jpg)
		if err := img(jpg); err != nil {
			return fail(err)
		}
	case KindText:
		raw, err := os.ReadFile(in)
		if err != nil {
			return fail(err)
		}
		txt, err := tempPath(tmp, "text-*.txt")
		if err != nil {
			return fail(err)
		}
		defer removeQuiet(txt)
		if err := os.WriteFile(txt, toLatin1(raw), 0o600); err != nil {
			return fail(err)
		}
		ps, err := tempPath(tmp, "text-*.ps")
		if err != nil {
			return fail(err)
		}
		defer removeQuiet(ps)
		txt, _ = filepath.Abs(txt)
		ps, _ = filepath.Abs(ps)
		if _, err := run(ctx, enscriptArgs(txt, ps, paperW, paperH)); err != nil {
			return fail(err)
		}
		if _, err := run(ctx, psToPDFArgs(ps, out)); err != nil {
			return fail(err)
		}
	default:
		return fail(ErrUnsupported)
	}
	return out, nil
}
