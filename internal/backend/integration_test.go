package backend

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// lockedBuf collects a child's stderr; safe to read while it runs.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// eveServer is a running ippeveprinter.
type eveServer struct {
	addr   string
	stderr *lockedBuf
	exited chan error
}

// startEve starts ippeveprinter on a free loopback port and waits until it
// accepts connections. It returns the exit error (and stderr) if the
// process dies first.
func startEve(t *testing.T, bin, spool string, extra ...string) (*eveServer, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	args := append([]string{"-k", "-d", spool, "-n", "127.0.0.1", "-p", fmt.Sprint(port),
		"-f", "application/pdf,image/pwg-raster"}, extra...)
	args = append(args, "printer-web test")
	cmd := exec.Command(bin, args...)
	s := &eveServer{addr: fmt.Sprintf("127.0.0.1:%d", port), stderr: &lockedBuf{}, exited: make(chan error, 1)}
	cmd.Stdout = s.stderr
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { s.exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-s.exited
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-s.exited:
			s.exited <- err // keep it for Cleanup
			return nil, fmt.Errorf("ippeveprinter exited early (%v): %s", err, s.stderr.String())
		default:
		}
		c, err := net.DialTimeout("tcp", s.addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return s, nil
		}
		if time.Now().After(deadline) {
			t.Fatalf("ippeveprinter did not listen on %s; output: %s", s.addr, s.stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// spooledDocument returns a spooled file that really holds the document:
// a PDF, or a non-empty PWG/Apple raster.
func spooledDocument(spool string) string {
	var found string
	_ = filepath.WalkDir(spool, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if bytes.HasPrefix(b, []byte("%PDF")) ||
			(len(b) > 8 && (bytes.HasPrefix(b, []byte("RaS2")) || bytes.HasPrefix(b, []byte("UNIRAST")))) {
			found = p
		}
		return nil
	})
	return found
}

// TestIPPEvePrinterIntegration prints through the ipp backend to a real
// (software) IPP printer, CUPS' ippeveprinter, bound to 127.0.0.1.
func TestIPPEvePrinterIntegration(t *testing.T) {
	bin, err := exec.LookPath("ippeveprinter")
	if err != nil {
		t.Skip("ippeveprinter not installed")
	}
	spool := t.TempDir()
	srv, err := startEve(t, bin, spool)
	if err != nil {
		low := strings.ToLower(err.Error())
		if !strings.Contains(low, "dns-sd") && !strings.Contains(low, "dnssd") &&
			!strings.Contains(low, "avahi") && !strings.Contains(low, "mdns") && !strings.Contains(low, "bonjour") {
			t.Fatal(err)
		}
		// No DNS-SD daemon here: "-r off" turns advertising off.
		t.Logf("retrying without DNS-SD: %v", err)
		if srv, err = startEve(t, bin, spool, "-r", "off"); err != nil {
			t.Skipf("ippeveprinter cannot run without DNS-SD on this machine: %v", err)
		}
	}

	b := newIPP(config.Config{Backend: "ipp", PrinterURI: "ipp://" + srv.addr + "/ipp/print"})
	b.tmp = t.TempDir()
	pdf := filepath.Join(t.TempDir(), "test.pdf")
	if err := os.WriteFile(pdf, tinyPDF(), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := b.Submit(ctx(t), pdf, "integration", Options{Copies: 1, Sides: "one", Color: true})
	if err != nil {
		t.Fatalf("Submit: %v; ippeveprinter output: %s", err, srv.stderr.String())
	}
	if h.ID <= 0 {
		t.Fatalf("no job id: %+v", h)
	}

	deadline := time.Now().Add(20 * time.Second)
	var st JobState
	for time.Now().Before(deadline) {
		select {
		case err := <-srv.exited:
			srv.exited <- err
			t.Fatalf("ippeveprinter died (%v): %s", err, srv.stderr.String())
		default:
		}
		st, err = b.Track(ctx(t), h)
		if err == nil && st == JobDone {
			return
		}
		if err == nil && (st == JobFailed || st == JobCanceled) {
			t.Fatalf("job ended %s; ippeveprinter output: %s", st, srv.stderr.String())
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Fallback: some ippeveprinter builds keep the job "processing" without a
	// print command; accept it only if the spooled file is the document.
	if f := spooledDocument(spool); f != "" {
		t.Logf("job %d still %q, but the document reached the spool (%s)", h.ID, st, filepath.Base(f))
		return
	}
	t.Fatalf("job %d not done (last state %q, err %v); ippeveprinter output: %s", h.ID, st, err, srv.stderr.String())
}
