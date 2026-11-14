// Package mtls is the config stream's mutual TLS (SEC-02, D7, ADR-0040):
// a certificate, its key and the pinned stream CA from PEM files, reloaded
// when their content changes, and the TLS configs of the control plane's
// gRPC server and the gateway's client. Both ends trust the stream CA only,
// never the system roots, and speak TLS 1.3 only.
package mtls

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sync/atomic"
	"time"
)

// Files are the PEM files of one end.
type Files struct {
	Cert string // certificate chain
	Key  string // private key (SEC-09)
	CA   string // the stream CA's certificate, the only trusted root
}

// set is one loaded certificate, key and CA.
type set struct {
	cert *tls.Certificate
	pool *x509.CertPool
}

// Reloader holds the current set and swaps in a new one when a file's
// content changes. Like the traffic certificate (ADR-0012) the files are
// polled, which also catches the symlink swaps of a Kubernetes Secret.
type Reloader struct {
	files Files
	check func(leaf *x509.Certificate) error
	log   *slog.Logger
	cur   atomic.Pointer[set]
	sum   [3][sha256.Size]byte // content last tried, good or bad
}

// Load reads the files once; a set that does not load, or whose leaf
// certificate check refuses, is an error. check may be nil.
func Load(files Files, check func(leaf *x509.Certificate) error, log *slog.Logger) (*Reloader, error) {
	r := &Reloader{files: files, check: check, log: log}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Run re-reads the files every interval until ctx ends. New handshakes use
// a new set; open connections keep theirs. A set that fails to load keeps
// the current one and is logged once per distinct content.
func (r *Reloader) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.reload(); err != nil {
				r.log.Error("stream certificate reload failed; keeping the current one", "cert", r.files.Cert,
					"serial", r.Leaf().SerialNumber.String(), "err", err)
			}
		}
	}
}

// Leaf is the current certificate.
func (r *Reloader) Leaf() *x509.Certificate { return r.cur.Load().cert.Leaf }

// reload swaps in the set if any file changed. The same bad content, or the
// same unreadable file (hashed as empty), is reported once. Writing a new
// set one file at a time may log one failed load before the last file
// lands.
func (r *Reloader) reload() error {
	var data [3][]byte
	var errs [3]error
	var sum [3][sha256.Size]byte
	for i, f := range []string{r.files.Cert, r.files.Key, r.files.CA} {
		data[i], errs[i] = os.ReadFile(f) //nolint:gosec // G304: the operator names the files
		sum[i] = sha256.Sum256(data[i])
	}
	if r.cur.Load() != nil && sum == r.sum {
		return nil
	}
	r.sum = sum
	for i, what := range []string{"certificate", "key", "CA"} {
		if errs[i] != nil {
			return fmt.Errorf("%s: %w", what, errs[i])
		}
	}
	cert, err := tls.X509KeyPair(data[0], data[1])
	if err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data[2]) {
		return fmt.Errorf("CA %s: no PEM certificate", r.files.CA)
	}
	if r.check != nil {
		if err := r.check(cert.Leaf); err != nil {
			return err
		}
	}
	r.cur.Store(&set{cert: &cert, pool: pool})
	r.log.Info("stream certificate loaded", "cert", r.files.Cert, "serial", cert.Leaf.SerialNumber.String(),
		"dns_names", cert.Leaf.DNSNames, "not_after", cert.Leaf.NotAfter, "ca", r.files.CA)
	return nil
}

// ServerConfig is the control plane's gRPC server config: TLS 1.3, the
// current certificate, and a client certificate required and verified
// against the current stream CA only, with the client-auth extended key
// usage (crypto/tls checks it). Session tickets are off, so every
// connection is verified in full against the CA of its time.
func (r *Reloader) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			s := r.cur.Load()
			return &tls.Config{
				MinVersion:             tls.VersionTLS13,
				Certificates:           []tls.Certificate{*s.cert},
				ClientAuth:             tls.RequireAndVerifyClientCert,
				ClientCAs:              s.pool,
				SessionTicketsDisabled: true,
			}, nil
		},
	}
}

// ClientConfig is the gateway's config for the control plane at
// serverName: TLS 1.3, the current certificate presented, and the server's
// certificate verified for serverName against the current stream CA only.
// crypto/tls cannot change RootCAs per handshake, so its own check is off
// and VerifyConnection does it with the CA of the moment.
func (r *Reloader) ClientConfig(serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // G402: VerifyConnection verifies the chain and name against the pinned stream CA
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return r.cur.Load().cert, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verify(cs.PeerCertificates, r.cur.Load().pool, serverName)
		},
	}
}

// verify checks a server's chain for name against roots, as crypto/tls
// would.
func verify(chain []*x509.Certificate, roots *x509.CertPool, name string) error {
	if len(chain) == 0 {
		return errors.New("the control plane sent no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}

// replicaLabel is a DNS label, as a replica name must be.
var replicaLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ReplicaName is the replica name a gateway's client certificate carries
// (D7, ADR-0040): its only DNS name, which must be a DNS label.
func ReplicaName(leaf *x509.Certificate) (string, error) {
	if len(leaf.DNSNames) != 1 {
		return "", fmt.Errorf("the certificate has %d DNS names; a gateway's has exactly one, its replica name", len(leaf.DNSNames))
	}
	if n := leaf.DNSNames[0]; !replicaLabel.MatchString(n) {
		return "", fmt.Errorf("the certificate's DNS name %q is not a DNS label (lower-case letters, digits and '-', at most 63)", n)
	}
	return leaf.DNSNames[0], nil
}

// LoadGateway loads a gateway's files and returns its replica name, the
// certificate's (D7, ADR-0040). A later certificate that names another
// replica is refused like any set that does not load: the name holds until
// the gateway restarts.
func LoadGateway(files Files, log *slog.Logger) (*Reloader, string, error) {
	var name string
	r, err := Load(files, func(leaf *x509.Certificate) error {
		n, err := ReplicaName(leaf)
		switch {
		case err != nil:
			return err
		case name == "":
			name = n // the first load, before Run starts
		case n != name:
			return fmt.Errorf("the certificate names replica %q; this gateway runs as %q until it restarts", n, name)
		}
		return nil
	}, log)
	return r, name, err
}
