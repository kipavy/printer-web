package docs

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // register decoders for image.DecodeConfig / image.Decode
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"strconv"
	"strings"
)

// maxImagePixels bounds decoding memory (50 megapixels).
const maxImagePixels = 50_000_000

// pdfImage is one image XObject ready to be embedded.
type pdfImage struct {
	w, h   int    // stored pixel size
	cs     string // DeviceGray | DeviceRGB | DeviceCMYK
	filter string // DCTDecode | FlateDecode
	decode string // optional /Decode array, e.g. "[1 0 1 0 1 0 1 0]"
	parms  string // optional /DecodeParms dictionary
	data   []byte // stream bytes (already filtered)
	orient int    // EXIF orientation 1..8
}

// imageToPDF writes a one-page PDF 1.4 holding the image at in. The page is
// paperW x paperH points; the image is scaled uniformly to fit inside it
// (never cropped), centred. A landscape image on a portrait sheet switches the
// page to landscape so it fills the sheet. With no paper size the page equals
// the image at 72 dpi. JPEG data is embedded untouched; PNG, GIF and BMP are
// decoded, composited on white and stored as Flate RGB or Gray.
func imageToPDF(in, out string, paperW, paperH float64) error {
	if !(paperW >= 0) || !(paperH >= 0) || math.IsInf(paperW, 0) || math.IsInf(paperH, 0) {
		return fmt.Errorf("invalid paper size %vx%v", paperW, paperH)
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	im, err := loadPDFImage(raw)
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	err = writeImagePDF(bw, im, paperW, paperH)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		removeQuiet(out)
	}
	return err
}

func loadPDFImage(raw []byte) (*pdfImage, error) {
	switch {
	case bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}):
		return jpegImage(raw)
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")), bytes.HasPrefix(raw, []byte("GIF87a")), bytes.HasPrefix(raw, []byte("GIF89a")):
		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		if err := checkSize(cfg.Width, cfg.Height); err != nil {
			return nil, err
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		return rasterImage(img)
	case bytes.HasPrefix(raw, []byte("BM")):
		img, err := decodeBMP(raw)
		if err != nil {
			return nil, err
		}
		return rasterImage(img)
	}
	return nil, ErrUnsupported
}

func checkSize(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("invalid image size %dx%d", w, h)
	}
	if int64(w)*int64(h) > maxImagePixels {
		return fmt.Errorf("image too large (%dx%d pixels)", w, h)
	}
	return nil
}

// jpegImage wraps the original JPEG bytes (DCTDecode passthrough). No pixel
// limit applies: the data is never decoded here.
func jpegImage(raw []byte) (*pdfImage, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("invalid image size %dx%d", cfg.Width, cfg.Height)
	}
	im := &pdfImage{w: cfg.Width, h: cfg.Height, filter: "DCTDecode", data: jpegEnd(raw), orient: 1}
	switch cfg.ColorModel {
	case color.GrayModel:
		im.cs = "DeviceGray"
	case color.YCbCrModel:
		im.cs = "DeviceRGB"
	case color.RGBAModel: // Adobe transform 0 or R,G,B component ids: no YCbCr step
		im.cs = "DeviceRGB"
		im.parms = " /DecodeParms << /ColorTransform 0 >>"
	case color.CMYKModel:
		im.cs = "DeviceCMYK"
	default:
		return nil, ErrUnsupported
	}
	adobe, orient := scanJPEGMarkers(raw)
	if adobe && im.cs == "DeviceCMYK" {
		im.decode = "[1 0 1 0 1 0 1 0]" // Adobe stores inverted CMYK
	}
	im.orient = orient
	return im, nil
}

// jpegEnd cuts b just after the EOI that ends the image, dropping trailing
// data such as the video of a motion photo. It walks the segments and skips
// each scan's entropy-coded data (0xFF00 stuffing and RSTn markers stay in
// the scan). Malformed input is returned unchanged.
func jpegEnd(b []byte) []byte {
	i := 2
	for i+2 <= len(b) {
		if b[i] != 0xFF {
			return b
		}
		m := b[i+1]
		switch {
		case m == 0xFF: // fill byte
			i++
			continue
		case m == 0xD9: // end of image
			return b[:i+2]
		case m == 0x01 || (m >= 0xD0 && m <= 0xD8):
			i += 2
			continue
		}
		if i+4 > len(b) {
			return b
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return b
		}
		i += 2 + n
		if m != 0xDA {
			continue
		}
		// Entropy-coded data runs until a marker other than RSTn.
		for {
			k := bytes.IndexByte(b[i:], 0xFF)
			if k < 0 || i+k+1 >= len(b) {
				return b
			}
			i += k
			next := b[i+1]
			if next == 0x00 || (next >= 0xD0 && next <= 0xD7) {
				i += 2
				continue
			}
			if next == 0xFF {
				i++
				continue
			}
			break
		}
	}
	return b
}

// scanJPEGMarkers walks the header segments up to the scan and reports an
// Adobe APP14 marker and the EXIF orientation (1 when absent or invalid).
func scanJPEGMarkers(b []byte) (adobe bool, orient int) {
	orient = 1
	seenExif := false
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return
		}
		m := b[i+1]
		if m == 0xFF { // fill byte
			i++
			continue
		}
		if m == 0xDA || m == 0xD9 { // start of scan / end of image
			return
		}
		if m == 0x01 || (m >= 0xD0 && m <= 0xD8) { // standalone markers
			i += 2
			continue
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return
		}
		seg := b[i+4 : i+2+n]
		switch {
		case m == 0xE1 && !seenExif && bytes.HasPrefix(seg, []byte("Exif\x00\x00")):
			seenExif = true // only the first Exif segment counts
			if o := exifOrientation(seg[6:]); o != 0 {
				orient = o
			}
		case m == 0xEE && bytes.HasPrefix(seg, []byte("Adobe")):
			adobe = true
		}
		i += 2 + n
	}
	return
}

// exifOrientation reads tag 0x0112 from IFD0 of a TIFF block; 0 if absent.
func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(t[2:]) != 42 {
		return 0
	}
	off := int64(bo.Uint32(t[4:]))
	if off+2 > int64(len(t)) {
		return 0
	}
	n := int64(bo.Uint16(t[off:]))
	for k := int64(0); k < n; k++ {
		e := off + 2 + k*12
		if e+12 > int64(len(t)) {
			return 0
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if bo.Uint16(t[e+2:]) != 3 || bo.Uint32(t[e+4:]) < 1 {
				return 0
			}
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
	}
	return 0
}

// rasterImage compresses decoded pixels (alpha composited on white) as 8-bit
// Flate Gray when every pixel is grey, RGB otherwise. Rows are converted once
// into a packed RGB buffer while grey-ness is tracked.
func rasterImage(img image.Image) (*pdfImage, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if err := checkSize(w, h); err != nil {
		return nil, err
	}
	pal := palInfo{}
	if p, ok := img.(*image.Paletted); ok {
		if len(p.Palette) == 0 {
			return nil, fmt.Errorf("image has an empty palette")
		}
		pal = whitePalette(p)
	}
	pix := make([]byte, 3*w*h)
	grey := true
	for y := 0; y < h; y++ {
		row := pix[3*w*y : 3*w*(y+1)]
		rgbRow(img, b.Min.Y+y, row, pal)
		if grey {
			for x := 0; x < w; x++ {
				if row[3*x] != row[3*x+1] || row[3*x] != row[3*x+2] {
					grey = false
					break
				}
			}
		}
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if grey {
		gr := make([]byte, w)
		for y := 0; y < h; y++ {
			row := pix[3*w*y:]
			for x := range gr {
				gr[x] = row[3*x]
			}
			if _, err := zw.Write(gr); err != nil {
				return nil, err
			}
		}
	} else if _, err := zw.Write(pix); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	im := &pdfImage{w: w, h: h, cs: "DeviceRGB", filter: "FlateDecode", data: buf.Bytes(), orient: 1}
	if grey {
		im.cs = "DeviceGray"
	}
	return im, nil
}

// palInfo caches white-composited palette entries of a paletted image.
type palInfo struct{ rgb [][3]byte }

func whitePalette(p *image.Paletted) palInfo {
	pi := palInfo{rgb: make([][3]byte, len(p.Palette))}
	for i, c := range p.Palette {
		r, g, b, a := c.RGBA()
		pi.rgb[i] = [3]byte{over(r, a), over(g, a), over(b, a)}
	}
	return pi
}

// over composites a premultiplied 16-bit channel on white, returning 8 bits.
func over(c, a uint32) byte { return byte((c + 0xffff - a) >> 8) }

// rgbRow fills dst with the white-composited RGB bytes of row y.
func rgbRow(img image.Image, y int, dst []byte, pal palInfo) {
	b := img.Bounds()
	n := b.Dx()
	switch m := img.(type) {
	case *image.Paletted:
		for x := 0; x < n; x++ {
			i := int(m.ColorIndexAt(b.Min.X+x, y))
			if i >= len(pal.rgb) {
				i = len(pal.rgb) - 1
			}
			copy(dst[3*x:], pal.rgb[i][:])
		}
	case *image.NRGBA:
		o := m.PixOffset(b.Min.X, y)
		for x := 0; x < n; x++ {
			p := m.Pix[o+4*x : o+4*x+4]
			a := uint32(p[3])
			if a == 255 {
				dst[3*x], dst[3*x+1], dst[3*x+2] = p[0], p[1], p[2]
				continue
			}
			for k := 0; k < 3; k++ {
				dst[3*x+k] = byte((uint32(p[k])*a + 255*(255-a) + 127) / 255)
			}
		}
	case *image.RGBA: // premultiplied
		o := m.PixOffset(b.Min.X, y)
		for x := 0; x < n; x++ {
			p := m.Pix[o+4*x : o+4*x+4]
			inv := 255 - uint32(p[3])
			dst[3*x], dst[3*x+1], dst[3*x+2] = byte(uint32(p[0])+inv), byte(uint32(p[1])+inv), byte(uint32(p[2])+inv)
		}
	case *image.Gray:
		o := m.PixOffset(b.Min.X, y)
		for x := 0; x < n; x++ {
			v := m.Pix[o+x]
			dst[3*x], dst[3*x+1], dst[3*x+2] = v, v, v
		}
	default:
		for x := 0; x < n; x++ {
			r, g, bl, a := img.At(b.Min.X+x, y).RGBA()
			dst[3*x], dst[3*x+1], dst[3*x+2] = over(r, a), over(g, a), over(bl, a)
		}
	}
}

// decodeBMP reads uncompressed (BI_RGB) 8-bit paletted, 24- and 32-bit bitmaps.
func decodeBMP(b []byte) (image.Image, error) {
	if len(b) < 54 || b[0] != 'B' || b[1] != 'M' {
		return nil, ErrUnsupported
	}
	le := binary.LittleEndian
	off := int64(le.Uint32(b[10:]))
	hs := int64(le.Uint32(b[14:]))
	if hs < 40 { // OS/2 core header and anything odd
		return nil, ErrUnsupported
	}
	w := int64(int32(le.Uint32(b[18:])))
	h := int64(int32(le.Uint32(b[22:])))
	bpp := int(le.Uint16(b[28:]))
	comp := le.Uint32(b[30:])
	top := h < 0
	if top {
		h = -h
	}
	if comp != 0 || (bpp != 8 && bpp != 24 && bpp != 32) {
		return nil, ErrUnsupported
	}
	if w <= 0 || h <= 0 || w > 1<<30 || h > 1<<30 {
		return nil, fmt.Errorf("invalid image size %dx%d", w, h)
	}
	if err := checkSize(int(w), int(h)); err != nil {
		return nil, err
	}
	stride := (w*int64(bpp) + 31) / 32 * 4
	if off < 14+hs || off+stride*h > int64(len(b)) {
		return nil, fmt.Errorf("truncated bitmap")
	}
	rowAt := func(y int64) []byte { // y counted from the top of the picture
		if !top {
			y = h - 1 - y
		}
		s := off + y*stride
		return b[s : s+stride]
	}
	rect := image.Rect(0, 0, int(w), int(h))
	if bpp == 8 {
		n := int(le.Uint32(b[46:]))
		if n == 0 || n > 256 {
			n = 256
		}
		po := 14 + hs
		if avail := int((off - po) / 4); n > avail { // short palette: use what is there
			n = avail
		}
		if n < 1 {
			return nil, fmt.Errorf("bad bitmap palette")
		}
		pal := make(color.Palette, n)
		for i := range pal {
			e := b[po+int64(i)*4:]
			pal[i] = color.RGBA{e[2], e[1], e[0], 255}
		}
		img := image.NewPaletted(rect, pal)
		for y := int64(0); y < h; y++ {
			copy(img.Pix[int(y)*img.Stride:int(y)*img.Stride+int(w)], rowAt(y))
		}
		return img, nil
	}
	img := image.NewNRGBA(rect)
	step := bpp / 8
	for y := int64(0); y < h; y++ {
		src := rowAt(y)
		dst := img.Pix[int(y)*img.Stride:]
		for x := 0; x < int(w); x++ {
			dst[4*x], dst[4*x+1], dst[4*x+2], dst[4*x+3] = src[step*x+2], src[step*x+1], src[step*x], 255
		}
	}
	return img, nil
}

// placement computes the page size and the image matrix.
func placement(im *pdfImage, paperW, paperH float64) (pw, ph float64, m [6]float64) {
	ew, eh := float64(im.w), float64(im.h)
	if im.orient >= 5 {
		ew, eh = eh, ew
	}
	pw, ph = ew, eh
	if paperW > 0 && paperH > 0 {
		pw, ph = paperW, paperH
		if ew > eh && pw < ph {
			pw, ph = ph, pw
		}
	}
	s := math.Min(pw/ew, ph/eh)
	dw, dh := ew*s, eh*s
	x0, y0 := (pw-dw)/2, (ph-dh)/2
	// c = {xa, xb, xc, ya, yb, yc}: displayed X = xa*a + xb*b + xc and
	// Y = ya*a + yb*b + yc, from stored column a and row b fractions (0..1).
	c := [9][6]float64{
		1: {1, 0, 0, 0, 1, 0},
		2: {-1, 0, 1, 0, 1, 0},
		3: {-1, 0, 1, 0, -1, 1},
		4: {1, 0, 0, 0, -1, 1},
		5: {0, 1, 0, 1, 0, 0},
		6: {0, -1, 1, 1, 0, 0},
		7: {0, -1, 1, -1, 0, 1},
		8: {0, 1, 0, -1, 0, 1},
	}[im.orient]
	m = [6]float64{
		dw * c[0], -dh * c[3], -dw * c[1], dh * c[4],
		x0 + dw*(c[1]+c[2]), y0 + dh - dh*(c[4]+c[5]),
	}
	return
}

// num formats a PDF number (3 decimals, trimmed, no "-0").
func num(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}

// countWriter tracks the offset of everything written.
type countWriter struct {
	w   *bufio.Writer
	n   int64
	err error
}

func (c *countWriter) printf(format string, a ...any) {
	if c.err != nil {
		return
	}
	n, err := fmt.Fprintf(c.w, format, a...)
	c.n += int64(n)
	c.err = err
}

func (c *countWriter) write(b []byte) {
	if c.err != nil {
		return
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.err = err
}

func writeImagePDF(bw *bufio.Writer, im *pdfImage, paperW, paperH float64) error {
	pw, ph, m := placement(im, paperW, paperH)
	content := fmt.Sprintf("q\n%s %s %s %s %s %s cm\n/Im0 Do\nQ\n", num(m[0]), num(m[1]), num(m[2]), num(m[3]), num(m[4]), num(m[5]))
	c := &countWriter{w: bw}
	offs := make([]int64, 6)
	c.write([]byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"))
	obj := func(n int, body string) {
		offs[n] = c.n
		c.printf("%d 0 obj\n%s\nendobj\n", n, body)
	}
	obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	obj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj(3, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>", num(pw), num(ph)))
	obj(4, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	offs[5] = c.n
	dec := im.parms
	if im.decode != "" {
		dec = " /Decode " + im.decode + dec
	}
	c.printf("5 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /%s /BitsPerComponent 8 /Filter /%s%s /Length %d >>\nstream\n",
		im.w, im.h, im.cs, im.filter, dec, len(im.data))
	c.write(im.data)
	c.printf("\nendstream\nendobj\n")
	xref := c.n
	c.printf("xref\n0 6\n0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		c.printf("%010d 00000 n \n", offs[i])
	}
	c.printf("trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return c.err
}

// isTIFF reports whether the file starts with a TIFF header.
func isTIFF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var h [4]byte
	if _, err := f.Read(h[:]); err != nil {
		return false
	}
	return string(h[:]) == "II*\x00" || string(h[:]) == "MM\x00*"
}
