//go:build integration

package integration

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestGateway checks gateway-1 in the compose stack: traffic on 8000 reaches
// the mock upstreams, the admin port 8001 answers /readyz and /metrics, and
// Prometheus scrapes it (GW-24, GW-25, OB-07).
func TestGateway(t *testing.T) {
	code, body := get(t, "http://127.0.0.1:8000/bytes/1024")
	if code != http.StatusOK || len(body) != 1024 {
		t.Errorf("GET :8000/bytes/1024 = %d with %d bytes, want 200 with 1024", code, len(body))
	}
	if code, _ := get(t, "http://127.0.0.1:8001/readyz"); code != http.StatusOK {
		t.Errorf("GET :8001/readyz = %d, want 200", code)
	}
	_, metrics := get(t, "http://127.0.0.1:8001/metrics")
	for _, m := range []string{"\ngo_goroutines ", "\ngo_gc_duration_seconds", "\ngw_config_version "} {
		if !strings.Contains(metrics, m) {
			t.Errorf("admin /metrics has no %s", strings.TrimSpace(m))
		}
	}
	if _, body := get(t, "http://127.0.0.1:8000/metrics"); strings.Contains(body, "gw_config_version") {
		t.Error("the traffic port serves the gateway's metrics")
	}

	// Prometheus scrapes every 5 s, so a freshly booted stack may need a moment.
	q := "http://127.0.0.1:9091/api/v1/query?query=" + url.QueryEscape(`up{job="gateway",instance="gateway-1:8001"}`)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(time.Second) {
		_, body := get(t, q)
		var r struct {
			Data struct {
				Result []struct {
					Value [2]any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(body), &r) == nil && len(r.Data.Result) == 1 && r.Data.Result[0].Value[1] == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Prometheus does not report gateway-1 up: %s", body)
		}
	}
}

// TestGatewayTLS checks gateway-1's TLS port 8443: HTTP/2 over TLS 1.2 or
// later with the certificate from make certs, which the test trusts directly
// (GW-04, SEC-01).
func TestGatewayTLS(t *testing.T) {
	pem, err := os.ReadFile("../../deploy/certs/gateway.pem")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("deploy/certs/gateway.pem has no certificate")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://127.0.0.1:8443/bytes/1024", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(body) != 1024 || resp.ProtoMajor != 2 {
		t.Errorf("GET :8443/bytes/1024 = %d %s with %d bytes, want 200 HTTP/2.0 with 1024", resp.StatusCode, resp.Proto, len(body))
	}
}

func get(t *testing.T, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}
