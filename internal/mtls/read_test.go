package mtls

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/ankitbisht23/spillway/internal/testcert"
)

// TestReloadReadsOneSet: a set swapped in between two of a poll's reads
// (the old certificate, then the new key and CA) is never loaded or
// reported as a set of its own; a poll that never reads the same content
// twice in a row keeps the current set and reports nothing (ADR-0040).
func TestReloadReadsOneSet(t *testing.T) {
	ca := testcert.NewCA(t, "stream CA")
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		ca.Issue(t, dir, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true})
	}
	in := func(dir string) Files {
		return Files{Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"), CA: filepath.Join(dir, "ca.pem")}
	}
	log := slog.New(slog.DiscardHandler)
	setB, err := Load(in(b), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Load(in(a), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	first := r.Leaf().SerialNumber
	// from reads a file of set a or b by the read's number.
	reads := 0
	from := func(set func(n int) string) func(string) ([]byte, error) {
		return func(name string) ([]byte, error) {
			reads++
			return os.ReadFile(filepath.Join(set(reads), filepath.Base(name))) //nolint:gosec // G304: the test's own files
		}
	}

	// The set is swapped after the first read: a's certificate with b's
	// key does not load, and would be reported.
	reads = 0
	r.readFile = from(func(n int) string {
		if n == 1 {
			return a
		}
		return b
	})
	if err := r.reload(); err != nil {
		t.Fatalf("a set swapped between two reads: %v", err)
	}
	if r.Leaf().SerialNumber.Cmp(setB.Leaf().SerialNumber) != 0 {
		t.Fatalf("serial %v, want set b's %v", r.Leaf().SerialNumber, setB.Leaf().SerialNumber)
	}

	// Content that changes on every pass: kept out, nothing reported, and
	// read again at the next poll.
	r.readFile = from(func(n int) string {
		if (n-1)/3%2 == 0 {
			return a
		}
		return b
	})
	before := r.sum
	if err := r.reload(); err != nil || r.Leaf().SerialNumber.Cmp(setB.Leaf().SerialNumber) != 0 || r.sum != before {
		t.Errorf("files changing on every pass: %v, serial %v (first %v); want nil and set b kept", err, r.Leaf().SerialNumber, first)
	}

	// The first load of files that keep changing fails.
	r2 := &Reloader{files: in(a), log: log, readFile: r.readFile}
	if err := r2.reload(); err == nil {
		t.Error("a first load of files that keep changing succeeded")
	}
}
