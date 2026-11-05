package docs

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// psString quotes s as a PostScript string literal.
func psString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return "(" + r.Replace(s) + ")"
}

func pageCountArgs(pdf string) []string {
	return append(gsBase(), "--permit-file-read="+pdf, "-dNODISPLAY",
		"-c", psString(pdf)+" (r) file runpdfbegin pdfpagecount = quit")
}

// PageCount returns the number of pages of a PDF.
func PageCount(ctx context.Context, pdf string) (int, error) {
	out, err := run(ctx, pageCountArgs(pdf))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("unreadable page count %q", strings.TrimSpace(string(out)))
	}
	return n, nil
}

func prepareArgs(pdf string, pages []int, out string) []string {
	ps := make([]string, len(pages))
	for i, p := range pages {
		ps[i] = strconv.Itoa(p)
	}
	return append(gsBase(), "-sDEVICE=pdfwrite", "-sPageList="+strings.Join(ps, ","), "-sOutputFile="+out, pdf)
}

// Prepare keeps only the selected pages and returns the new PDF.
func Prepare(ctx context.Context, pdf string, pages []int, tmp string) (string, error) {
	if len(pages) == 0 {
		return "", fmt.Errorf("no pages selected")
	}
	out, err := tempPath(tmp, "prep-*.pdf")
	if err != nil {
		return "", err
	}
	if _, err := run(ctx, prepareArgs(pdf, pages, out)); err != nil {
		removeQuiet(out)
		return "", err
	}
	return out, nil
}

// ParsePages parses "1-3, 5" or "4-" against a page count; empty means all.
// The result is sorted and deduplicated.
func ParsePages(spec string, count int) ([]int, error) {
	if count < 1 {
		return nil, fmt.Errorf("document has no pages")
	}
	spec = strings.TrimSpace(spec)
	set := map[int]bool{}
	if spec == "" {
		spec = "1-"
	}
	bad := func() error { return fmt.Errorf("invalid page selection %q", spec) }
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, bad()
		}
		var lo, hi int
		var err1, err2 error
		if i := strings.Index(part, "-"); i >= 0 {
			a, b := strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
			if a == "" && b == "" {
				return nil, bad()
			}
			lo, hi = 1, count
			if a != "" {
				lo, err1 = strconv.Atoi(a)
			}
			if b != "" {
				hi, err2 = strconv.Atoi(b)
			}
		} else {
			lo, err1 = strconv.Atoi(part)
			hi = lo
		}
		if err1 != nil || err2 != nil || lo < 1 || hi < lo {
			return nil, bad()
		}
		if hi > count {
			return nil, fmt.Errorf("page %d is beyond the last page (%d)", hi, count)
		}
		for i := lo; i <= hi; i++ {
			set[i] = true
		}
	}
	res := make([]int, 0, len(set))
	for p := range set {
		res = append(res, p)
	}
	sort.Ints(res)
	return res, nil
}
