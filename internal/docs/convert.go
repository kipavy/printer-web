package docs

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kipavy/printer-web/internal/config"
)

// Ghostscript device names, isolated so CI can correct them in one place.
const (
	devPWG = "pwgraster"
	// devURF is the Apple raster (URF) device name in Ghostscript.
	devURF   = "appleraster"
	devPS    = "ps2write"
	devPXL   = "pxlcolor"
	devPXLBW = "pxlmono"
	devInk   = "inkcov"

	rasterDPI = 300
)

// convertExt maps an output format to a temp file extension.
func convertExt(format string) string {
	switch format {
	case "image/pwg-raster":
		return ".pwg"
	case "image/urf":
		return ".urf"
	case "application/postscript":
		return ".ps"
	case "application/vnd.hp-pcl":
		return ".pcl"
	}
	return ".out"
}

// convertArgs builds the gs argv for a non-PDF output format.
func convertArgs(pdf, format string, color bool, out string) ([]string, error) {
	a := gsBase()
	dpi := "-r" + strconv.Itoa(rasterDPI)
	switch format {
	case "image/pwg-raster":
		a = append(a, "-sDEVICE="+devPWG, dpi)
		a = append(a, cupsColor(color)...)
	case "image/urf":
		a = append(a, "-sDEVICE="+devURF, dpi)
		a = append(a, cupsColor(color)...)
	case "application/postscript":
		a = append(a, "-sDEVICE="+devPS)
		if !color {
			a = append(a, "-sColorConversionStrategy=Gray", "-dProcessColorModel=/DeviceGray")
		}
	case "application/vnd.hp-pcl":
		dev := devPXL
		if !color {
			dev = devPXLBW
		}
		a = append(a, "-sDEVICE="+dev, dpi)
	default:
		return nil, fmt.Errorf("unsupported output format %q", format)
	}
	return append(a, "-sOutputFile="+out, pdf), nil
}

// Convert renders the PDF into the printer's format and returns a file path
// (callers stat it for the exact size). application/pdf returns pdf itself.
func Convert(ctx context.Context, pdf, format string, color bool, tmp string) (string, error) {
	pdf, err := filepath.Abs(pdf)
	if err != nil {
		return "", err
	}
	if format == "application/pdf" {
		return pdf, nil
	}
	out, err := tempPath(tmp, "conv-*"+convertExt(format))
	if err != nil {
		return "", err
	}
	argv, err := convertArgs(pdf, format, color, out)
	if err != nil {
		removeQuiet(out)
		return "", err
	}
	if _, err := run(ctx, argv); err != nil {
		removeQuiet(out)
		return "", err
	}
	return out, nil
}

// Coverage is the ink coverage of one page, each channel 0..1.
type Coverage struct{ C, M, Y, K float64 }

func inkcovArgs(pdf string) []string {
	return append(gsBase(), "-sDEVICE="+devInk, "-o", "-", pdf)
}

// parseInkcov parses lines like "0.01234  0.00000  0.00000  0.05678 CMYK OK".
func parseInkcov(out []byte) ([]Coverage, error) {
	var res []Coverage
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[4] != "CMYK" {
			continue
		}
		var v [4]float64
		for i := 0; i < 4; i++ {
			x, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				return nil, fmt.Errorf("bad inkcov line %q", line)
			}
			v[i] = x
		}
		res = append(res, Coverage{v[0], v[1], v[2], v[3]})
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("no ink coverage output")
	}
	return res, nil
}

// InkCoverage measures per-page ink coverage with Ghostscript's inkcov device.
func InkCoverage(ctx context.Context, pdf string) ([]Coverage, error) {
	pdf, err := filepath.Abs(pdf)
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, inkcovArgs(pdf))
	if err != nil {
		return nil, err
	}
	return parseInkcov(out)
}

var channels = [4]string{"C", "M", "Y", "K"}

// EstimateCost returns sheets*Sheet + copies * Σ pages Σ colours
// (coverage/5%) * price/yield. In mono mode only K counts, with the colour
// channels folded in as gray. ok is false when pricing is disabled.
func EstimateCost(cov []Coverage, sheets, copies int, mono bool, p config.Pricing) (float64, bool) {
	if !p.Enabled() {
		return 0, false
	}
	var perCopy float64
	for _, c := range cov {
		ch := [4]float64{c.C, c.M, c.Y, c.K}
		if mono {
			ch = [4]float64{0, 0, 0, c.K + (c.C+c.M+c.Y)/3}
		}
		for i, v := range ch {
			k := channels[i]
			cart, ok := p.Toner[k]
			if !ok || cart.Yield <= 0 {
				continue
			}
			perCopy += v / 0.05 * cart.Price / float64(cart.Yield)
		}
	}
	return float64(sheets)*p.Sheet + float64(copies)*perCopy, true
}

// cupsColor selects the raster colour space: 18 = sGray, 19 = sRGB, 8 bits.
func cupsColor(color bool) []string {
	cs := "18"
	if color {
		cs = "19"
	}
	return []string{"-dcupsColorSpace=" + cs, "-dcupsBitsPerColor=8"}
}
