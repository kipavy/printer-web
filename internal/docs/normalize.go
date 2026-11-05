package docs

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"unicode/utf8"
)

func pt(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// normalizeArgs builds the argv turning a PS or image file into a PDF.
// Text and HEIC need several steps and are handled in Normalize.
func normalizeArgs(k Kind, in, out string, paperW, paperH float64) ([]string, error) {
	switch k {
	case KindPS:
		a := append(gsBase(), "-sDEVICE=pdfwrite")
		if paperW > 0 && paperH > 0 {
			a = append(a, "-dDEVICEWIDTHPOINTS="+pt(paperW), "-dDEVICEHEIGHTPOINTS="+pt(paperH), "-dFIXEDMEDIA", "-dPDFFitPage")
		}
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

func enscriptArgs(in, out string) []string {
	return []string{"enscript", "-B", "-q", "--encoding=latin1", "--media=A4", "-o", out, in}
}

func psToPDFArgs(in, out string) []string {
	return append(gsBase(), "-sDEVICE=pdfwrite", "-sOutputFile="+out, in)
}

// toLatin1 converts UTF-8 to ISO-8859-1, replacing unmappable runes with '?'.
func toLatin1(b []byte) []byte {
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

// Normalize converts an input file to a PDF inside tmp. PDFs are returned as is.
func Normalize(ctx context.Context, in string, k Kind, paperW, paperH float64, tmp string) (string, error) {
	if k == KindPDF {
		return in, nil
	}
	out, err := tempPath(tmp, "norm-*.pdf")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) { removeQuiet(out); return "", err }
	switch k {
	case KindPS, KindImage:
		argv, err := normalizeArgs(k, in, out, paperW, paperH)
		if err != nil {
			return fail(err)
		}
		if _, err := run(ctx, argv); err != nil {
			return fail(err)
		}
	case KindHEIC:
		jpg, err := tempPath(tmp, "heic-*.jpg")
		if err != nil {
			return fail(err)
		}
		defer removeQuiet(jpg)
		if _, err := run(ctx, heicArgs(in, jpg)); err != nil {
			return fail(err)
		}
		argv, _ := normalizeArgs(KindImage, jpg, out, paperW, paperH)
		if _, err := run(ctx, argv); err != nil {
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
		if _, err := run(ctx, enscriptArgs(txt, ps)); err != nil {
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
