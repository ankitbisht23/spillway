package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/testcert"
)

func TestLoadSettings(t *testing.T) {
	base := map[string]string{
		"SPILLWAY_GATEWAY_CONFIG":           "/etc/spillway/config.yaml",
		"SPILLWAY_GATEWAY_HTTP_ADDR":        ":8000",
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": "172.29.0.0/16",
	}
	env := func(over map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := over[k]; ok {
				return v
			}
			return base[k]
		}
	}

	s, err := loadSettings(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.configPath != "/etc/spillway/config.yaml" || s.identity != "" || s.httpAddr != ":8000" || s.tlsCert != "" || s.adminAddr != "127.0.0.1:8001" || len(s.allow) != 1 || s.dnsRefresh != 30*time.Second ||
		len(s.trusted) != 0 || s.accessLogSampleRate != 1 || s.otlpEndpoint != "" {
		t.Errorf("settings = %+v", s)
	}
	tlsOnly := map[string]string{"SPILLWAY_GATEWAY_HTTP_ADDR": "", "SPILLWAY_GATEWAY_TLS_CERT": "/c.pem", "SPILLWAY_GATEWAY_TLS_KEY": "/k.pem"}
	if s, err := loadSettings(env(tlsOnly)); err != nil || s.httpAddr != "" || s.tlsAddr != ":8443" || s.tlsCert != "/c.pem" || s.tlsKey != "/k.pem" {
		t.Errorf("TLS only: %+v, %v", s, err)
	}
	tlsOnly["SPILLWAY_GATEWAY_TLS_ADDR"] = "127.0.0.1:9443"
	if s, err := loadSettings(env(tlsOnly)); err != nil || s.tlsAddr != "127.0.0.1:9443" {
		t.Errorf("TLS address 127.0.0.1:9443: %+v, %v", s, err)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_IDENTITY": "/etc/spillway/identity.yaml"})); err != nil || s.identity != "/etc/spillway/identity.yaml" {
		t.Errorf("identity file: %+v, %v", s, err)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_TRUSTED_PROXIES": "10.0.0.0/8, 2001:db8::/32"})); err != nil || len(s.trusted) != 2 {
		t.Errorf("trusted proxies: %+v, %v", s, err)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_DNS_REFRESH": "5s"})); err != nil || s.dnsRefresh != 5*time.Second {
		t.Errorf("DNS refresh 5s: %+v, %v", s, err)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_ADMIN_ADDR": ":8001"})); err != nil || s.adminAddr != ":8001" {
		t.Errorf("admin address :8001: %+v, %v", s, err)
	}
	for v, want := range map[string]float64{"0": 0, "0.05": 0.05, "1": 1} {
		if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": v})); err != nil || s.accessLogSampleRate != want {
			t.Errorf("access log sample rate %s: %+v, %v", v, s, err)
		}
	}
	for _, v := range []string{"http://otel-collector:4317", "https://collector.example:4317"} {
		if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": v})); err != nil || s.otlpEndpoint != v {
			t.Errorf("OTLP endpoint %s: %+v, %v", v, s, err)
		}
	}

	for _, tt := range []struct {
		over map[string]string
		want string // the error's prefix
	}{
		{map[string]string{"SPILLWAY_GATEWAY_CONFIG": ""}, "SPILLWAY_GATEWAY_CONFIG"},
		{map[string]string{"SPILLWAY_GATEWAY_HTTP_ADDR": ""}, "no listener"},
		{map[string]string{"SPILLWAY_GATEWAY_TLS_CERT": "/c.pem"}, "SPILLWAY_GATEWAY_TLS_CERT and SPILLWAY_GATEWAY_TLS_KEY"},
		{map[string]string{"SPILLWAY_GATEWAY_TLS_KEY": "/k.pem"}, "SPILLWAY_GATEWAY_TLS_CERT and SPILLWAY_GATEWAY_TLS_KEY"},
		{map[string]string{"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": ""}, "SPILLWAY_GATEWAY_ALLOWED_NETWORKS"},
		{map[string]string{"SPILLWAY_GATEWAY_TRUSTED_PROXIES": "10.0.0.1"}, "SPILLWAY_GATEWAY_TRUSTED_PROXIES"},
		{map[string]string{"SPILLWAY_GATEWAY_DNS_REFRESH": "0s"}, "SPILLWAY_GATEWAY_DNS_REFRESH"},
		{map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": "-0.1"}, "SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"},
		{map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": "1.5"}, "SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"},
		{map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": "NaN"}, "SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"},
		{map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": "10%"}, "SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"},
		{map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": "otel-collector:4317"}, "SPILLWAY_GATEWAY_OTLP_ENDPOINT"},
		{map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": "grpc://otel-collector:4317"}, "SPILLWAY_GATEWAY_OTLP_ENDPOINT"},
		{map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": "http://otel-collector"}, "SPILLWAY_GATEWAY_OTLP_ENDPOINT"},
		{map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": "http://:4317"}, "SPILLWAY_GATEWAY_OTLP_ENDPOINT"},
	} {
		if _, err := loadSettings(env(tt.over)); err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%v: error = %v, want one starting %q", tt.over, err, tt.want)
		}
	}
}

func TestParseMemoryMax(t *testing.T) {
	for in, want := range map[string]int64{
		"536870912\n": 536870912,
		"max\n":       0,
		"":            0,
		"0\n":         0,
		"-1\n":        0,
	} {
		got, ok := parseMemoryMax([]byte(in))
		if ok != (want > 0) || ok && got != want {
			t.Errorf("parseMemoryMax(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
}

// TestServe runs the gateway against a local upstream: traffic is proxied on
// the plain-HTTP and TLS listeners, HTTP/2 over TLS, the admin port answers
// /readyz, /metrics (with the request metrics) and the healthcheck, and on
// shutdown readiness fails while an in-flight request still completes.
func TestServe(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(arrived)
			<-release
		}
		_, _ = io.WriteString(w, "upstream")
	}))
	t.Cleanup(up.Close)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "version: 7\nupstreams: [{ name: up, endpoints: [{ address: '" + up.Listener.Addr().String() + "' }] }]\n" +
		"routes: [{ name: all, match: { path_prefix: / }, upstream: up }, { name: limited, match: { path: /limited }, upstream: up }]\n" +
		"policies: [{ name: once, level: route, match: { routes: [limited] }, key_template: '{route}', algorithm: gcra, limits: [{ rate: 1, period: 1h, burst: 1 }], local: true }]\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity.yaml")
	if err := os.WriteFile(identity, []byte("version: 1\nconsumers: [{ name: alice }]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	cert := testcert.Write(t, certFile, keyFile, 1)
	s := settings{
		configPath: cfg,
		identity:   identity,
		httpAddr:   freeAddr(t),
		tlsAddr:    freeAddr(t),
		tlsCert:    certFile,
		tlsKey:     keyFile,
		adminAddr:  freeAddr(t),
		allow:      proxy.Allowlist{netip.MustParsePrefix("127.0.0.0/8")},
		dnsRefresh: time.Minute,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, slog.New(slog.DiscardHandler)) }()

	traffic, admin := "http://"+s.httpAddr, "http://"+s.adminAddr
	waitFor(t, "/readyz 200", func() bool { code, _ := get(t, admin+"/readyz"); return code == http.StatusOK })
	_, port, _ := net.SplitHostPort(s.adminAddr)
	for _, addr := range []string{s.adminAddr, ":" + port} {
		if err := healthcheck(t.Context(), adminEnv(addr)); err != nil {
			t.Errorf("healthcheck with admin address %s: %v", addr, err)
		}
	}
	if _, body := get(t, admin+"/metrics"); !strings.Contains(body, "\ngw_config_version 7\n") || !strings.Contains(body, "\ngo_goroutines ") {
		t.Errorf("/metrics lacks gw_config_version 7 or go_goroutines:\n%s", body)
	}
	if code, body := get(t, traffic+"/x"); code != http.StatusOK || body != "upstream" {
		t.Errorf("GET /x = %d %q, want 200 from the upstream", code, body)
	}
	if proto, body := getTLS(t, "https://"+s.tlsAddr+"/x", cert); proto != "HTTP/2.0" || body != "upstream" {
		t.Errorf("GET /x over TLS = %s %q, want HTTP/2.0 from the upstream", proto, body)
	}
	for _, p := range []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/"} {
		if _, body := get(t, traffic+p); body != "upstream" {
			t.Errorf("traffic port %s = %q, want it proxied like any path", p, body)
		}
	}
	// Six requests so far: /x over plain HTTP and over TLS, and the four
	// admin paths.
	requests := `gw_requests_total{code_class="2xx",method="GET",origin="upstream",reason="none",route="all",tenant="none"} `
	waitFor(t, "6 requests in /metrics", func() bool { _, body := get(t, admin+"/metrics"); return strings.Contains(body, "\n"+requests+"6\n") })

	// The config's local policy is enforced (ADR-0017).
	if code, _ := get(t, traffic+"/limited"); code != http.StatusOK {
		t.Errorf("first GET /limited = %d, want 200", code)
	}
	if code, _ := get(t, traffic+"/limited"); code != http.StatusTooManyRequests {
		t.Errorf("second GET /limited = %d, want 429", code)
	}

	// The identity file has no API keys, so any key is answered 401 (GW-17).
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, traffic+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", "unknown-key-0000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /x with an unknown API key = %d, want 401", resp.StatusCode)
	}

	slow := make(chan int)
	go func() { code, _ := get(t, traffic+"/slow"); slow <- code }()
	<-arrived
	cancel()
	waitFor(t, "/readyz 503", func() bool { code, _ := get(t, admin+"/readyz"); return code == http.StatusServiceUnavailable })
	if err := healthcheck(t.Context(), adminEnv(s.adminAddr)); err == nil {
		t.Error("healthcheck passed while draining")
	}
	close(release)
	if code := <-slow; code != http.StatusOK {
		t.Errorf("in-flight request = %d, want 200", code)
	}
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
	if err := healthcheck(t.Context(), adminEnv(s.adminAddr)); err == nil {
		t.Error("healthcheck passed after shutdown")
	}
}

// TestServeRejectsInvalidIdentity: an invalid identity file at start stops
// the gateway, as an invalid config file does (ADR-0017).
func TestServeRejectsInvalidIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg, identity := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "identity.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("consumers: [{ name: a }, { name: a }]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := settings{configPath: cfg, identity: identity, httpAddr: freeAddr(t), adminAddr: freeAddr(t), dnsRefresh: time.Minute}
	if err := serve(t.Context(), s, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), `consumer "a": defined twice`) {
		t.Errorf("serve = %v, want the identity error", err)
	}
}

// adminEnv returns a getenv with only the admin address set.
func adminEnv(adminAddr string) func(string) string {
	return func(k string) string {
		if k == "SPILLWAY_GATEWAY_ADMIN_ADDR" {
			return adminAddr
		}
		return ""
	}
}

func freeAddr(t *testing.T) string {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// get returns the status and body, or 0 if the request failed.
func get(t *testing.T, url string) (int, string) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return 0, ""
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// getTLS returns the protocol and body of a GET that trusts only cert and
// offers HTTP/2.
func getTLS(t *testing.T, url string, cert *x509.Certificate) (string, string) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}}
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
	return resp.Proto, string(body)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
