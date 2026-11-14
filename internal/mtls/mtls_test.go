package mtls_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/mtls"
	"github.com/ankitbisht23/spillway/internal/testcert"
)

func files(dir string) mtls.Files {
	return mtls.Files{Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"), CA: filepath.Join(dir, "ca.pem")}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// pki is a stream CA with a control-plane server certificate, and a second
// CA that must never be trusted.
type pki struct {
	ca, other *testcert.CA
	server    *mtls.Reloader
	dir       string
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	p := &pki{ca: testcert.NewCA(t, "stream CA"), other: testcert.NewCA(t, "other CA"), dir: t.TempDir()}
	p.ca.Issue(t, filepath.Join(p.dir, "controlplane"), testcert.Leaf{DNSNames: []string{"controlplane", "localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}})
	var err error
	if p.server, err = mtls.Load(files(filepath.Join(p.dir, "controlplane")), nil, discard()); err != nil {
		t.Fatal(err)
	}
	return p
}

// client issues a client certificate from ca into a new directory and
// loads it.
func (p *pki) client(t *testing.T, ca *testcert.CA, l testcert.Leaf) *mtls.Reloader {
	t.Helper()
	dir := t.TempDir()
	ca.Issue(t, dir, l)
	// The client trusts the stream CA whichever CA signed its certificate.
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), p.ca.PEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := mtls.Load(files(dir), nil, discard())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// handshake runs one TLS handshake between srv and cli over loopback and
// returns, on success, the client certificate the server saw, and both
// ends' errors.
func handshake(t *testing.T, srv, cli *tls.Config) (peer *x509.Certificate, serverErr, clientErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var wg sync.WaitGroup
	wg.Go(func() {
		c, err := ln.Accept()
		if err != nil {
			serverErr = err
			return
		}
		defer func() { _ = c.Close() }()
		tc := tls.Server(c, srv)
		_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
		if serverErr = tc.HandshakeContext(ctx); serverErr == nil {
			// TLS 1.3 checks the client certificate after the client's
			// handshake returns: a read sees the result.
			if _, err := tc.Write([]byte("ok")); err != nil {
				serverErr = err
			}
			peer = tc.ConnectionState().PeerCertificates[0]
		}
	})
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	tc := tls.Client(c, cli)
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	if clientErr = tc.HandshakeContext(ctx); clientErr == nil {
		var b [2]byte
		_, clientErr = tc.Read(b[:])
	}
	wg.Wait()
	return peer, serverErr, clientErr
}

// TestHandshake checks both ends of the stream's mTLS (SEC-02, ADR-0040):
// a gateway certificate from the stream CA is accepted, with its name; no
// certificate, another CA's, an expired one, or one without the
// client-auth usage is refused; the gateway refuses a server of another
// CA or name; TLS 1.2 is refused.
func TestHandshake(t *testing.T) {
	p := newPKI(t)
	gw := p.client(t, p.ca, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true})
	peer, srvErr, cliErr := handshake(t, p.server.ServerConfig(), gw.ClientConfig("controlplane"))
	if srvErr != nil || cliErr != nil {
		t.Fatalf("right certificate: server %v, client %v", srvErr, cliErr)
	}
	if name, err := mtls.ReplicaName(peer); err != nil || name != "gateway-1" {
		t.Errorf("replica name %q, %v", name, err)
	}
	// By IP too, as host processes dial 127.0.0.1.
	if _, srvErr, cliErr := handshake(t, p.server.ServerConfig(), gw.ClientConfig("127.0.0.1")); srvErr != nil || cliErr != nil {
		t.Errorf("by IP: server %v, client %v", srvErr, cliErr)
	}

	noCert := gw.ClientConfig("controlplane")
	noCert.GetClientCertificate = nil
	for name, cli := range map[string]*tls.Config{
		"no client certificate": noCert,
		"another CA":            p.client(t, p.other, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true}).ClientConfig("controlplane"),
		"expired":               p.client(t, p.ca, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true, NotAfter: time.Now().Add(-time.Minute)}).ClientConfig("controlplane"),
		"server-auth only":      p.client(t, p.ca, testcert.Leaf{DNSNames: []string{"gateway-1"}}).ClientConfig("controlplane"),
	} {
		_, srvErr, cliErr := handshake(t, p.server.ServerConfig(), cli)
		if srvErr == nil || cliErr == nil {
			t.Errorf("%s: accepted (server %v, client %v)", name, srvErr, cliErr)
		}
	}

	// The gateway's checks of the server.
	if _, _, cliErr := handshake(t, p.server.ServerConfig(), gw.ClientConfig("gateway-9")); cliErr == nil || !strings.Contains(cliErr.Error(), "gateway-9") {
		t.Errorf("wrong server name: %v", cliErr)
	}
	otherDir := t.TempDir()
	p.other.Issue(t, otherDir, testcert.Leaf{DNSNames: []string{"controlplane"}})
	impostor, err := mtls.Load(files(otherDir), nil, discard())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, cliErr := handshake(t, impostor.ServerConfig(), gw.ClientConfig("controlplane")); cliErr == nil || !strings.Contains(cliErr.Error(), "unknown authority") {
		t.Errorf("a server of another CA: %v", cliErr)
	}

	old := gw.ClientConfig("controlplane")
	old.MinVersion, old.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if _, srvErr, _ := handshake(t, p.server.ServerConfig(), old); srvErr == nil {
		t.Error("TLS 1.2 accepted")
	}
}

// TestReplicaName checks the name rule (D7, ADR-0040): exactly one DNS
// name, a DNS label.
func TestReplicaName(t *testing.T) {
	for _, tt := range []struct {
		names []string
		want  string
	}{
		{[]string{"gateway-1"}, ""},
		{[]string{"a"}, ""},
		{nil, "0 DNS names"},
		{[]string{"gateway-1", "gateway-2"}, "2 DNS names"},
		{[]string{"Gateway-1"}, "not a DNS label"},
		{[]string{"gateway-1.example"}, "not a DNS label"},
		{[]string{"-gateway"}, "not a DNS label"},
		{[]string{strings.Repeat("a", 64)}, "not a DNS label"},
	} {
		name, err := mtls.ReplicaName(&x509.Certificate{DNSNames: tt.names})
		switch {
		case tt.want == "" && (err != nil || name != tt.names[0]):
			t.Errorf("%v: %q, %v", tt.names, name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%v: %v, want %q", tt.names, err, tt.want)
		}
	}
}

// TestReload checks the reload (ADR-0040): a new certificate and CA serve
// the next handshake; a set that fails to load, or that names another
// replica, keeps the current one and is logged once; the key's content is
// never logged.
func TestReload(t *testing.T) {
	// Each set is issued into its own directory and swapped in by
	// renaming a symlink, as a Kubernetes Secret volume does, so no poll
	// sees half a set.
	root := t.TempDir()
	link := filepath.Join(root, "current")
	swap := func(issue func(dir string)) {
		t.Helper()
		dir, err := os.MkdirTemp(root, "set-")
		if err != nil {
			t.Fatal(err)
		}
		issue(dir)
		tmp := filepath.Join(root, "next")
		if err := os.Symlink(dir, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, link); err != nil {
			t.Fatal(err)
		}
	}
	ca := testcert.NewCA(t, "stream CA")
	swap(func(dir string) { ca.Issue(t, dir, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true}) })
	var logs bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(lockedWriter{&mu, &logs}, nil))
	logged := func() string { mu.Lock(); defer mu.Unlock(); return logs.String() }
	r, _, err := mtls.LoadGateway(files(link), log)
	if err != nil {
		t.Fatal(err)
	}
	first := r.Leaf().SerialNumber
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx, 10*time.Millisecond) }()
	defer func() { cancel(); <-done }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for start := time.Now(); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Since(start) > 10*time.Second {
				t.Fatal(what)
			}
		}
	}
	// A new CA and certificate, as make certs after deleting the CA.
	ca2 := testcert.NewCA(t, "stream CA 2")
	swap(func(dir string) { ca2.Issue(t, dir, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true}) })
	waitFor("the new certificate", func() bool { return r.Leaf().SerialNumber.Cmp(first) != 0 })
	second := r.Leaf().SerialNumber
	cfg := r.ClientConfig("controlplane")
	if c, err := cfg.GetClientCertificate(nil); err != nil || c.Leaf.SerialNumber.Cmp(second) != 0 {
		t.Errorf("the client presents %v, %v", c.Leaf.SerialNumber, err)
	}

	// A certificate the check refuses, then a broken key: both kept out,
	// each logged once.
	swap(func(dir string) { ca2.Issue(t, dir, testcert.Leaf{DNSNames: []string{"gateway-2"}, Client: true}) })
	waitFor("the refusal logged", func() bool {
		return strings.Contains(logged(), `the certificate names replica \"gateway-2\"; this gateway runs as \"gateway-1\" until it restarts`)
	})
	swap(func(dir string) {
		ca2.Issue(t, dir, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true})
		if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("not a key"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	waitFor("the broken key logged", func() bool { return strings.Count(logged(), "stream certificate reload failed") == 2 })
	time.Sleep(100 * time.Millisecond) // ten more polls
	out := logged()
	if n := strings.Count(out, "stream certificate reload failed"); n != 2 || r.Leaf().SerialNumber.Cmp(second) != 0 {
		t.Errorf("%d failures logged, serial %v; want 2 and the second certificate kept:\n%s", n, r.Leaf().SerialNumber, out)
	}
	if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, "not a key") {
		t.Errorf("a key shows in the logs:\n%s", out)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// TestLoadGateway checks a gateway's name (D7, ADR-0040): the
// certificate's; a certificate that does not name one replica does not
// load.
func TestLoadGateway(t *testing.T) {
	ca := testcert.NewCA(t, "stream CA")
	dir := t.TempDir()
	ca.Issue(t, dir, testcert.Leaf{DNSNames: []string{"gateway-4"}, Client: true})
	if _, name, err := mtls.LoadGateway(files(dir), discard()); err != nil || name != "gateway-4" {
		t.Errorf("LoadGateway: %q, %v", name, err)
	}
	ca.Issue(t, dir, testcert.Leaf{DNSNames: []string{"gateway-4", "gateway-5"}, Client: true})
	if _, _, err := mtls.LoadGateway(files(dir), discard()); err == nil || !strings.Contains(err.Error(), "2 DNS names") {
		t.Errorf("two names: %v", err)
	}
	if _, _, err := mtls.LoadGateway(files(filepath.Join(dir, "missing")), discard()); err == nil || !strings.Contains(err.Error(), "certificate:") {
		t.Errorf("no files: %v", err)
	}
}
