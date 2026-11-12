package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/testcert"
)

// TestServeSheds runs the gateway with a cap of 2 requests in flight and 2
// open connections (GW-26, ADR-0030, ADR-0033). With a slow request held on
// a plain connection and another on an HTTP/2 one, a third request on the
// HTTP/2 connection is shed with 503 by the in-flight cap; with both
// connections' requests in progress, a third connection is shed by the
// connection cap with the fixed HTTP/1.1 503; the admin port stays
// reachable and /metrics counts both.
func TestServeSheds(t *testing.T) {
	u := newShutdownUpstream(t)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "version: 1\nupstreams: [{ name: up, endpoints: [{ address: '" + u.Listener.Addr().String() + "' }] }]\n" +
		"routes: [{ name: all, match: { path_prefix: / }, upstream: up }]\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	roots := x509.NewCertPool()
	roots.AddCert(testcert.Write(t, certFile, keyFile, 1))
	s := settings{
		configPath:     cfg,
		httpAddr:       freeAddr(t),
		tlsAddr:        freeAddr(t),
		tlsCert:        certFile,
		tlsKey:         keyFile,
		adminAddr:      freeAddr(t),
		allow:          proxy.Allowlist{netip.MustParsePrefix("127.0.0.0/8")},
		dnsRefresh:     time.Minute,
		shutdownGrace:  5 * time.Second,
		maxConnections: 2,
		maxInFlight:    2,
	}
	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, slog.New(slog.NewJSONHandler(logs, nil)), nil, noFlush) }()
	admin := "http://" + s.adminAddr
	waitFor(t, "/readyz 200", func() bool { code, _ := get(t, admin+"/readyz"); return code == http.StatusOK })

	slow := slowRequest(t, s, u) // connection 1, request 1
	// Connection 2, over TLS with HTTP/2: request 2 is held too, and the
	// in-flight cap sheds request 3 on it.
	secure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}}
	t.Cleanup(secure.CloseIdleConnections)
	slow2 := slowRequestWith(t, secure, "https://"+s.tlsAddr+"/slow", u)
	if code, retryAfter, body := do(t, secure, "https://"+s.tlsAddr+"/x"); code != http.StatusServiceUnavailable || retryAfter != "1" || body != `{"error":"overloaded"}` {
		t.Errorf("past the in-flight cap: %d, Retry-After %q, %q; want 503, 1, overloaded", code, retryAfter, body)
	}
	// Connection 3: both others have a request in progress, so the
	// connection cap sheds it.
	plain := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(plain.CloseIdleConnections)
	if code, retryAfter, body := do(t, plain, "http://"+s.httpAddr+"/x"); code != http.StatusServiceUnavailable || retryAfter != "1" || body != `{"error":"overloaded"}` {
		t.Errorf("past the connection cap: %d, Retry-After %q, %q; want 503, 1, overloaded", code, retryAfter, body)
	}
	// The admin port is exempt.
	for _, series := range []string{
		`gw_requests_total{code_class="5xx",method="GET",origin="gateway",reason="shed",route="none",tenant="none"} 1`,
		`gw_requests_total{code_class="5xx",method="other",origin="gateway",reason="shed",route="none",tenant="none"} 1`,
	} {
		if code, body := get(t, admin+"/metrics"); code != http.StatusOK || !strings.Contains(body, "\n"+series+"\n") {
			t.Errorf("/metrics: %d without %s", code, series)
		}
	}
	if !strings.Contains(logs.String(), `"msg":"shedding connections","connections":1,"max_connections":2`) {
		t.Errorf("no shedding log line in\n%s", logs.String())
	}
	u.free()
	for _, c := range []<-chan int{slow, slow2} {
		if code := <-c; code != http.StatusOK {
			t.Errorf("held request: %d, want 200", code)
		}
	}
	// Both connections now open are idle: a new one takes the place of
	// the one idle longest instead of being shed (ADR-0033).
	if code, _, _ := do(t, plain, "http://"+s.httpAddr+"/x"); code != http.StatusOK {
		t.Errorf("second plain connection: %d, want 200", code)
	}
	other := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(other.CloseIdleConnections)
	if code, _, _ := do(t, other, "http://"+s.httpAddr+"/x"); code != http.StatusOK {
		t.Errorf("past the cap with idle connections: %d, want 200", code)
	}
	if !strings.Contains(logs.String(), `"msg":"closing waiting connections at the connection cap"`) {
		t.Errorf("no log line for the closed idle connection in\n%s", logs.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
}

// do sends a GET with client and returns the status, Retry-After and body.
func do(t *testing.T, client *http.Client, url string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Retry-After"), string(body)
}
