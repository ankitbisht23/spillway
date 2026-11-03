package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/testcert"
)

// serveTLS serves 200s with c's TLS config through the traffic server, as
// cmd/gateway does, and returns the address.
func serveTLS(t *testing.T, c *Certs) string {
	t.Helper()
	srv := NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), slog.New(slog.DiscardHandler))
	srv.TLSConfig = c.TLSConfig()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// handshake connects with cfg and returns the connection state.
func handshake(t *testing.T, addr string, cfg *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer func() { _ = conn.Close() }()
	return conn.(*tls.Conn).ConnectionState(), nil
}

// newCerts writes a certificate with the given serial and loads it.
func newCerts(t *testing.T, serial int64) (*Certs, *x509.CertPool, string, string) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	roots := x509.NewCertPool()
	roots.AddCert(testcert.Write(t, certFile, keyFile, serial))
	c, err := NewCerts(certFile, keyFile, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return c, roots, certFile, keyFile
}

// TestTLSVersionsAndSuites: TLS 1.2 or later, and on TLS 1.2 only ECDHE with
// AEAD ciphers (SEC-01).
func TestTLSVersionsAndSuites(t *testing.T) {
	c, roots, _, _ := newCerts(t, 1)
	addr := serveTLS(t, c)
	//nolint:gosec // clients offer old versions and weak suites on purpose: the server must refuse them
	for _, tt := range []struct {
		name string
		cfg  *tls.Config
		ok   bool
	}{
		{"TLS 1.1", &tls.Config{MinVersion: tls.VersionTLS11, MaxVersion: tls.VersionTLS11}, false},
		{"TLS 1.2 with CBC", &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA}}, false},
		{"TLS 1.2 with AES-GCM", &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}}, true},
		{"TLS 1.2 with ChaCha20-Poly1305", &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256}}, true},
		{"TLS 1.3", &tls.Config{MinVersion: tls.VersionTLS13}, true},
	} {
		tt.cfg.RootCAs = roots
		if _, err := handshake(t, addr, tt.cfg); (err == nil) != tt.ok {
			t.Errorf("%s: handshake error = %v, want ok = %v", tt.name, err, tt.ok)
		}
	}
}

// TestALPN: a client that offers h2 gets HTTP/2; one that offers only
// HTTP/1.1 gets HTTP/1.1 (GW-04).
func TestALPN(t *testing.T) {
	c, roots, _, _ := newCerts(t, 1)
	addr := serveTLS(t, c)
	for want, tr := range map[string]*http.Transport{
		"HTTP/2.0": {TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true},
		"HTTP/1.1": {TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.Proto != want {
			t.Errorf("client offering %s got %s", want, resp.Proto)
		}
	}
}

// TestCertsReload: rotating the files serves the new certificate without a
// restart; a pair that does not load keeps the current one (GW-04).
func TestCertsReload(t *testing.T) {
	c, roots, certFile, keyFile := newCerts(t, 1)
	addr := serveTLS(t, c)
	serial := func() int64 {
		t.Helper()
		st, err := handshake(t, addr, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		return st.PeerCertificates[0].SerialNumber.Int64()
	}
	if s := serial(); s != 1 {
		t.Fatalf("serial %d, want 1", s)
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { c.Run(ctx, 10*time.Millisecond); close(stopped) }()
	roots.AddCert(testcert.Write(t, certFile, keyFile, 2))
	for deadline := time.Now().Add(5 * time.Second); serial() != 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the rotated certificate was not served")
		}
	}
	cancel()
	<-stopped

	// A certificate that does not match the key is refused, and so is a
	// missing key file.
	key3 := filepath.Join(t.TempDir(), "key.pem")
	roots.AddCert(testcert.Write(t, certFile, key3, 3))
	if err := c.reload(); err == nil {
		t.Error("a certificate with another certificate's key loaded")
	}
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	if err := c.reload(); err == nil {
		t.Error("a missing key file loaded")
	}
	if s := serial(); s != 2 {
		t.Errorf("serial %d after bad pairs, want 2 kept", s)
	}
	if err := os.Rename(key3, keyFile); err != nil {
		t.Fatal(err)
	}
	if err := c.reload(); err != nil || serial() != 3 {
		t.Errorf("fixed pair: %v, serial %d; want 3", err, serial())
	}

	if _, err := NewCerts(certFile, key3, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("NewCerts loaded without a key file")
	}
}
