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
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestGateway checks gateway-1 in the compose stack: traffic on 8000 reaches
// the mock upstreams, the admin port 8001 answers /readyz and /metrics, with
// the request metrics, and Prometheus scrapes it (GW-24, GW-25, OB-01, OB-07).
func TestGateway(t *testing.T) {
	code, body := get(t, "http://127.0.0.1:8000/bytes/1024")
	if code != http.StatusOK || len(body) != 1024 {
		t.Errorf("GET :8000/bytes/1024 = %d with %d bytes, want 200 with 1024", code, len(body))
	}
	if code, _ := get(t, "http://127.0.0.1:8001/readyz"); code != http.StatusOK {
		t.Errorf("GET :8001/readyz = %d, want 200", code)
	}
	_, metrics := get(t, "http://127.0.0.1:8001/metrics")
	for _, m := range []string{
		"\ngo_goroutines ", "\ngo_gc_duration_seconds", "\ngw_config_version ",
		`
gw_requests_total{code_class="2xx",method="GET",origin="upstream",reason="none",route="default",tenant="none"} `,
		`
gw_request_duration_seconds_count{route="default",tenant="none"} `,
		`
gw_upstream_duration_seconds_count{attempt="1",upstream="httpbin"} `,
		"\ngw_upstream_endpoint_healthy{endpoint=",
	} {
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

// TestGatewayAPIKeys checks that gateway-1 resolves API keys against the
// compose identity file: ADR-0017's example key for alice is proxied, a wrong
// key with her key's prefix is answered 401 (GW-17, SEC-03).
func TestGatewayAPIKeys(t *testing.T) {
	for key, want := range map[string]int{"alice-example-key-1": http.StatusOK, "alice-example-key-2": http.StatusUnauthorized} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000/status/200", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", key)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET :8000/status/200 with key %s = %d, want %d", key[:12]+"…", resp.StatusCode, want)
		}
	}
}

// TestGatewayRateLimit checks gateway-1's local policies from the compose
// config: GET /anything/limited is answered 429 with Retry-After and the
// RateLimit fields once limited-per-ip's 5 per 10 s are spent; a request with
// ADR-0017's example key for alice goes through, with per-ip's fields, while
// the shadow per-key policy records its decision (RL-25 – RL-27, GW-17).
func TestGatewayRateLimit(t *testing.T) {
	client := &http.Client{Timeout: 10 * time.Second}
	send := func(path, key string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("X-API-Key", key)
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
		return resp.StatusCode, resp.Header, string(body)
	}

	const policy = `"per-ip";q=5000;w=1, "limited-per-ip";q=5;w=10`
	var limited http.Header
	var body string
	for i := 0; i < 6 && limited == nil; i++ {
		status, h, b := send("/anything/limited", "")
		if got := h.Get("RateLimit-Policy"); got != policy || h.Get("RateLimit") == "" {
			t.Errorf("request %d: RateLimit-Policy %q and RateLimit %q, want %q and a value", i+1, got, h.Get("RateLimit"), policy)
		}
		switch status {
		case http.StatusOK:
		case http.StatusTooManyRequests:
			limited, body = h, b
		default:
			t.Fatalf("request %d: status %d", i+1, status)
		}
	}
	if limited == nil {
		t.Fatal("6 requests in a row to /anything/limited were all allowed, want a 429 within 6")
	}
	if ra, err := strconv.Atoi(limited.Get("Retry-After")); err != nil || ra < 1 || ra > 10 ||
		limited.Get("Content-Type") != "application/json" || body != `{"error":"rate limited"}` ||
		!strings.Contains(limited.Get("RateLimit"), `"limited-per-ip";r=0;t=`) {
		t.Errorf("429: Retry-After %q, %q %q, RateLimit %q", limited.Get("Retry-After"), limited.Get("Content-Type"), body, limited.Get("RateLimit"))
	}

	status, h, _ := send("/status/200", "alice-example-key-1")
	if status != http.StatusOK || h.Get("RateLimit-Policy") != `"per-ip";q=5000;w=1` {
		t.Errorf("GET /status/200 with alice's key = %d with RateLimit-Policy %q, want 200 with per-ip's only", status, h.Get("RateLimit-Policy"))
	}
	_, metrics := get(t, "http://127.0.0.1:8001/metrics")
	for _, m := range []string{
		`
gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="limited-per-ip"} `,
		`
gw_ratelimit_decisions_total{decision="allow",mode="shadow",policy="per-key"} `,
		`
gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="ratelimited",route="limited",tenant="none"} `,
		`
gw_ratelimit_check_duration_seconds_count{backend="local"} `,
	} {
		if !strings.Contains(metrics, m) {
			t.Errorf("admin /metrics has no %s", strings.TrimSpace(m))
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

// TestAccessLogInLoki sends a request with its own request ID and trace
// context through gateway-1, checks the upstream got both unchanged, and finds
// the request's access log line in Loki, which gets it from the gateway
// through the OTel Collector (GW-22, OB-03).
func TestAccessLogInLoki(t *testing.T) {
	id := "itest-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	start := time.Now()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000/headers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", id)
	req.Header.Set("Traceparent", traceparent)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var echoed struct{ Headers map[string][]string } // go-httpbin's /headers
	if err := json.NewDecoder(resp.Body).Decode(&echoed); err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("X-Request-Id"); resp.StatusCode != http.StatusOK || got != id {
		t.Fatalf("GET :8000/headers = %d with request ID %q, want 200 with %q", resp.StatusCode, got, id)
	}
	if h := echoed.Headers; strings.Join(h["X-Request-Id"], ",") != id || strings.Join(h["Traceparent"], ",") != traceparent {
		t.Errorf("upstream got X-Request-Id %q and traceparent %q, want %q and %q", h["X-Request-Id"], h["Traceparent"], id, traceparent)
	}

	// The gateway exports logs every second and the Collector batches them,
	// so the line takes a few seconds to arrive.
	q := url.Values{
		"query": {`{service_name="spillway-gateway"} | request_id="` + id + `"`},
		"start": {strconv.FormatInt(start.Add(-time.Minute).UnixNano(), 10)},
	}
	var stream map[string]string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		_, body := get(t, "http://127.0.0.1:3100/loki/api/v1/query_range?"+q.Encode())
		var r struct {
			Data struct {
				Result []struct {
					Stream map[string]string `json:"stream"`
					Values [][2]string       `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(body), &r) == nil && len(r.Data.Result) > 0 {
			if n := len(r.Data.Result); n != 1 || len(r.Data.Result[0].Values) != 1 {
				t.Fatalf("Loki has %d streams for request %s, want one line: %s", n, id, body)
			}
			stream = r.Data.Result[0].Stream
			if line := r.Data.Result[0].Values[0][1]; line != "access" {
				t.Errorf("log line = %q, want access", line)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Loki has no access log line for request %s: %s", id, body)
		}
	}
	for k, want := range map[string]string{
		"trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
		"route":    "default",
		"method":   "GET",
		"status":   "200",
		"upstream": "httpbin",
		"key_hash": "",
	} {
		if stream[k] != want {
			t.Errorf("Loki line for %s: %s = %q, want %q (all: %v)", id, k, stream[k], want, stream)
		}
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
