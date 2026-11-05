// Package docs turns uploaded files into print-ready documents.
package docs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"unicode/utf8"
)

// Kind is a detected input document type.
type Kind int

// Supported kinds.
const (
	KindPDF Kind = iota + 1
	KindPS
	KindImage
	KindHEIC
	KindText
)

var (
	// ErrEmpty is returned for zero-byte files.
	ErrEmpty = errors.New("empty file")
	// ErrUnsupported is returned for unrecognised formats.
	ErrUnsupported = errors.New("unsupported file type")
)

const sniffLen = 64 * 1024

var heicBrands = []string{"heic", "heix", "hevc", "hevx", "hevm", "hevs", "heim", "heis", "mif1", "msf1"}

// Detect identifies a file by its magic bytes (never by its name).
func Detect(path string) (Kind, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, sniffLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, err
	}
	return detectBytes(buf[:n], n == sniffLen)
}

func detectBytes(b []byte, truncated bool) (Kind, error) {
	if len(b) == 0 {
		return 0, ErrEmpty
	}
	switch {
	case isPDF(b):
		return KindPDF, nil
	case bytes.HasPrefix(b, []byte("%!PS")), bytes.HasPrefix(b, []byte("\x04%!PS")):
		return KindPS, nil
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}),
		bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")),
		bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")),
		bytes.HasPrefix(b, []byte("II*\x00")), bytes.HasPrefix(b, []byte("MM\x00*")):
		return KindImage, nil
	}
	// BMP: "BM" plus a plausible header; plain text starting with "BM" has no NULs.
	if len(b) >= 26 && b[0] == 'B' && b[1] == 'M' && bytes.IndexByte(b[:26], 0) >= 0 {
		return KindImage, nil
	}
	if len(b) >= 12 && string(b[4:8]) == "ftyp" {
		for _, br := range heicBrands {
			if string(b[8:12]) == br {
				return KindHEIC, nil
			}
		}
		return 0, ErrUnsupported
	}
	if looksText(b, truncated) {
		return KindText, nil
	}
	return 0, ErrUnsupported
}

// looksText reports whether b is UTF-8 without binary control bytes.
// When the sniff window was truncated, a split rune at the end is tolerated.
func looksText(b []byte, truncated bool) bool {
	if truncated {
		for i := 0; i < utf8.UTFMax-1 && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
	}
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if (c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f') || c == 0x7f {
			return false
		}
	}
	return true
}

// isPDF reports whether the header sits at the start, after an optional BOM
// and whitespace (so a ZIP holding a PDF entry is not a PDF).
func isPDF(b []byte) bool {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	b = bytes.TrimLeft(b, " \t\r\n\f")
	return bytes.HasPrefix(b, []byte("%PDF-"))
}
