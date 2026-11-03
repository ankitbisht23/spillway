package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Certs serves the traffic port's certificate from a pair of PEM files and
// reloads it when either file's content changes (GW-04). Like the config, the
// files are polled, which also catches the symlink swaps of a Kubernetes
// Secret.
type Certs struct {
	certFile, keyFile string
	log               *slog.Logger
	current           atomic.Pointer[tls.Certificate]
	sum               [2][sha256.Size]byte // content last tried, good or bad
}

// NewCerts loads the pair once; an invalid pair is an error.
func NewCerts(certFile, keyFile string, log *slog.Logger) (*Certs, error) {
	c := &Certs{certFile: certFile, keyFile: keyFile, log: log}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// Run re-reads the files every interval until ctx ends. A pair that fails to
// load leaves the current certificate in place.
func (c *Certs) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.reload(); err != nil {
				c.log.Error("certificate reload failed; keeping the current certificate", "cert", c.certFile,
					"serial", c.current.Load().Leaf.SerialNumber.String(), "err", err)
			}
		}
	}
}

// TLSConfig is the traffic port's TLS config (SEC-01): TLS 1.2 or later, and
// on TLS 1.2 only ECDHE key exchange with AEAD ciphers (every TLS 1.3 suite is
// one). ALPN offers HTTP/2, then HTTP/1.1 (GW-04).
func (c *Certs) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.current.Load(), nil
		},
	}
}

// reload swaps in the pair if either file changed. The same bad content, or
// the same unreadable file (hashed as empty), is reported once, not on every
// poll. Writing a new pair one file at a time may log one failed load before
// the second file lands.
func (c *Certs) reload() error {
	certPEM, certErr := os.ReadFile(c.certFile)
	keyPEM, keyErr := os.ReadFile(c.keyFile)
	sum := [2][sha256.Size]byte{sha256.Sum256(certPEM), sha256.Sum256(keyPEM)}
	if c.current.Load() != nil && sum == c.sum {
		return nil
	}
	c.sum = sum
	if certErr != nil {
		return fmt.Errorf("certificate: %w", certErr)
	}
	if keyErr != nil {
		return fmt.Errorf("certificate key: %w", keyErr)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	c.current.Store(&cert)
	c.log.Info("certificate loaded", "cert", c.certFile, "serial", cert.Leaf.SerialNumber.String(),
		"dns_names", cert.Leaf.DNSNames, "not_after", cert.Leaf.NotAfter)
	return nil
}
