package backend

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kipavy/printer-web/internal/config"
)

// tinyPDF builds a one-page PDF with a valid xref table.
func tinyPDF() []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		"", // content stream, below
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	stream := "BT /F1 24 Tf 72 720 Td (printer-web integration test) Tj ET"
	objs[3] = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

// TestIPPEvePrinterIntegration prints through the ipp backend to a real
// (software) IPP printer, CUPS' ippeveprinter, bound to 127.0.0.1.
func TestIPPEvePrinterIntegration(t *testing.T) {
	bin, err := exec.LookPath("ippeveprinter")
	if err != nil {
		t.Skip("ippeveprinter not installed")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	spool := t.TempDir()
	cmd := exec.Command(bin, "-k", "-d", spool, "-n", "127.0.0.1", "-p", fmt.Sprint(port),
		"-f", "application/pdf,image/pwg-raster", "printer-web test")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if i > 50 {
			t.Fatalf("ippeveprinter did not start on %s", addr)
		}
		time.Sleep(200 * time.Millisecond)
	}

	b := newIPP(config.Config{Backend: "ipp", PrinterURI: "ipp://" + addr + "/ipp/print"})
	pdf := filepath.Join(t.TempDir(), "test.pdf")
	if err := os.WriteFile(pdf, tinyPDF(), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := b.Submit(ctx(t), pdf, "integration", Options{Copies: 1, Sides: "one", Color: true})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := b.Track(ctx(t), h)
		if err == nil && st == JobDone {
			return
		}
		if err == nil && st == JobFailed {
			t.Fatal("job failed")
		}
		if files, _ := filepath.Glob(filepath.Join(spool, "*", "*")); len(files) > 0 {
			return // the job file reached the spool directory
		}
		if files, _ := filepath.Glob(filepath.Join(spool, "*")); len(files) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d not done (last state %q, err %v)", h.ID, st, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
