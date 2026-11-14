package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
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
		len(s.trusted) != 0 || s.accessLogSampleRate != 1 || s.otlpEndpoint != "" || s.drain != 5*time.Second || s.shutdownGrace != 15*time.Second ||
		s.maxConnections != 8192 || s.maxInFlight != 4096 || s.traceSampleRatio != 1 {
		t.Errorf("settings = %+v", s)
	}
	streamCerts := map[string]string{"SPILLWAY_GATEWAY_CP_CERT": "/s/cert.pem", "SPILLWAY_GATEWAY_CP_KEY": "/s/key.pem", "SPILLWAY_GATEWAY_CP_CA": "/s/ca.pem"}
	managedEnv := map[string]string{"SPILLWAY_GATEWAY_CONFIG": "", "SPILLWAY_GATEWAY_CONTROL_PLANE": "controlplane:9090"}
	for k, v := range streamCerts {
		managedEnv[k] = v
	}
	if s, err := loadSettings(env(managedEnv)); err != nil || s.controlPlane != "controlplane:9090" || s.snapshot != "/var/lib/spillway/snapshot.pb" ||
		s.streamTLS.Cert != "/s/cert.pem" || s.streamTLS.Key != "/s/key.pem" || s.streamTLS.CA != "/s/ca.pem" || s.cpServerName != "controlplane" {
		t.Errorf("managed mode: %+v, %v", s, err)
	}
	managedEnv["SPILLWAY_GATEWAY_CP_SERVER_NAME"] = "cp.internal"
	if s, err := loadSettings(env(managedEnv)); err != nil || s.cpServerName != "cp.internal" {
		t.Errorf("server name: %+v, %v", s, err)
	}
	// Static mode ignores the stream's settings, which compose sets on
	// every gateway.
	if s, err := loadSettings(env(streamCerts)); err != nil || s.controlPlane != "" {
		t.Errorf("static mode with stream certificates: %+v, %v", s, err)
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
	if s.redisAddrs != nil || s.redisAddrMap != nil || s.redisAuth.Username != "" || s.redisAuth.Password.IsSet() {
		t.Errorf("Redis by default: %v, %v, %+v; want none", s.redisAddrs, s.redisAddrMap, s.redisAuth)
	}
	redis := map[string]string{
		"SPILLWAY_GATEWAY_REDIS_ADDRS": "redis-0:6379, redis-1:6379,",
		"SPILLWAY_REDIS_ADDR_MAP":      "redis-0:6379=127.0.0.1:7000",
	}
	if s, err := loadSettings(env(redis)); err != nil || !slices.Equal(s.redisAddrs, []string{"redis-0:6379", "redis-1:6379"}) || s.redisAddrMap["redis-0:6379"] != "127.0.0.1:7000" {
		t.Errorf("Redis: %+v, %v", s, err)
	}
	if s.devMode {
		t.Error("dev mode on by default")
	}
	for v, want := range map[string]bool{"true": true, "1": true, "false": false} {
		if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_DEV_MODE": v})); err != nil || s.devMode != want {
			t.Errorf("dev mode %s: %+v, %v", v, s, err)
		}
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_DNS_REFRESH": "5s"})); err != nil || s.dnsRefresh != 5*time.Second {
		t.Errorf("DNS refresh 5s: %+v, %v", s, err)
	}
	for _, v := range []string{"0s", "0", "250ms"} {
		want, _ := time.ParseDuration(v)
		if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_DRAIN": v})); err != nil || s.drain != want {
			t.Errorf("drain %s: %+v, %v", v, s, err)
		}
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_SHUTDOWN_GRACE": "30s"})); err != nil || s.shutdownGrace != 30*time.Second {
		t.Errorf("shutdown grace 30s: %+v, %v", s, err)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_MAX_CONNECTIONS": "100", "SPILLWAY_GATEWAY_MAX_IN_FLIGHT": "50"})); err != nil || s.maxConnections != 100 || s.maxInFlight != 50 {
		t.Errorf("caps 100 and 50: %+v, %v", s, err)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_ADMIN_ADDR": ":8001"})); err != nil || s.adminAddr != ":8001" {
		t.Errorf("admin address :8001: %+v, %v", s, err)
	}
	for v, want := range map[string]float64{"0": 0, "0.05": 0.05, "1": 1} {
		if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": v})); err != nil || s.accessLogSampleRate != want {
			t.Errorf("access log sample rate %s: %+v, %v", v, s, err)
		}
	}
	for v, want := range map[string]float64{"0": 0, "0.01": 0.01, "1": 1} {
		if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO": v})); err != nil || s.traceSampleRatio != want {
			t.Errorf("trace sample ratio %s: %+v, %v", v, s, err)
		}
	}
	if s.jwtIssuer != "" || s.jwtJWKSURL != "" || s.jwtAudience != "" || s.jwtCA != "" {
		t.Errorf("JWT on by default: %+v", s)
	}
	jwt := map[string]string{
		"SPILLWAY_GATEWAY_JWT_ISSUER":   "https://sso.example/realms/spillway",
		"SPILLWAY_GATEWAY_JWT_JWKS_URL": "https://keycloak.internal/realms/spillway/protocol/openid-connect/certs",
		"SPILLWAY_GATEWAY_JWT_AUDIENCE": "spillway-gateway",
		"SPILLWAY_GATEWAY_JWT_CA":       "/etc/spillway-jwt/ca.pem",
	}
	if s, err := loadSettings(env(jwt)); err != nil || s.jwtIssuer != jwt["SPILLWAY_GATEWAY_JWT_ISSUER"] || s.jwtJWKSURL != jwt["SPILLWAY_GATEWAY_JWT_JWKS_URL"] ||
		s.jwtAudience != "spillway-gateway" || s.jwtCA != "/etc/spillway-jwt/ca.pem" {
		t.Errorf("JWT: %+v, %v", s, err)
	}
	// Compose's plain-HTTP Keycloak, in dev mode only (SEC-04).
	devJWT := map[string]string{
		"SPILLWAY_GATEWAY_JWT_ISSUER":   "http://localhost:8180/auth/realms/spillway",
		"SPILLWAY_GATEWAY_JWT_JWKS_URL": "http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/certs",
		"SPILLWAY_GATEWAY_JWT_AUDIENCE": "spillway-gateway",
		"SPILLWAY_GATEWAY_DEV_MODE":     "true",
	}
	if s, err := loadSettings(env(devJWT)); err != nil || s.jwtIssuer != devJWT["SPILLWAY_GATEWAY_JWT_ISSUER"] {
		t.Errorf("JWT in dev mode: %+v, %v", s, err)
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
		// Managed mode (ADR-0038).
		{map[string]string{"SPILLWAY_GATEWAY_CONTROL_PLANE": "controlplane:9090"}, "SPILLWAY_GATEWAY_CONFIG and SPILLWAY_GATEWAY_IDENTITY: not with SPILLWAY_GATEWAY_CONTROL_PLANE"},
		{map[string]string{"SPILLWAY_GATEWAY_CONFIG": "", "SPILLWAY_GATEWAY_IDENTITY": "/i.yaml", "SPILLWAY_GATEWAY_CONTROL_PLANE": "controlplane:9090"}, "SPILLWAY_GATEWAY_CONFIG and SPILLWAY_GATEWAY_IDENTITY"},
		{map[string]string{"SPILLWAY_GATEWAY_CONFIG": "", "SPILLWAY_GATEWAY_CONTROL_PLANE": "controlplane"}, `SPILLWAY_GATEWAY_CONTROL_PLANE: "controlplane" is not host:port`},
		{map[string]string{"SPILLWAY_GATEWAY_CONFIG": "", "SPILLWAY_GATEWAY_CONTROL_PLANE": "cp:9090"}, "SPILLWAY_GATEWAY_CP_CERT, SPILLWAY_GATEWAY_CP_KEY and SPILLWAY_GATEWAY_CP_CA: required"},
		{map[string]string{"SPILLWAY_GATEWAY_CONFIG": "", "SPILLWAY_GATEWAY_CONTROL_PLANE": "cp:9090", "SPILLWAY_GATEWAY_CP_CERT": "/c", "SPILLWAY_GATEWAY_CP_KEY": "/k"}, "SPILLWAY_GATEWAY_CP_CERT, SPILLWAY_GATEWAY_CP_KEY and SPILLWAY_GATEWAY_CP_CA: required"},
		{map[string]string{"SPILLWAY_GATEWAY_SNAPSHOT": "/s.pb"}, "SPILLWAY_GATEWAY_SNAPSHOT: managed mode only"},
		{map[string]string{"SPILLWAY_GATEWAY_REDIS_USERNAME": "spillway"}, "SPILLWAY_GATEWAY_REDIS_USERNAME: set with SPILLWAY_GATEWAY_REDIS_PASSWORD"},
		{map[string]string{"SPILLWAY_GATEWAY_REDIS_PASSWORD": "a", "SPILLWAY_GATEWAY_REDIS_PASSWORD_FILE": "/p"}, "set SPILLWAY_GATEWAY_REDIS_PASSWORD or SPILLWAY_GATEWAY_REDIS_PASSWORD_FILE, not both"},
		{map[string]string{"SPILLWAY_GATEWAY_REDIS_PASSWORD_FILE": "/nonexistent/p"}, "SPILLWAY_GATEWAY_REDIS_PASSWORD_FILE: cannot read the file"},
		{map[string]string{"SPILLWAY_GATEWAY_HTTP_ADDR": ""}, "no listener"},
		{map[string]string{"SPILLWAY_GATEWAY_TLS_CERT": "/c.pem"}, "SPILLWAY_GATEWAY_TLS_CERT and SPILLWAY_GATEWAY_TLS_KEY"},
		{map[string]string{"SPILLWAY_GATEWAY_TLS_KEY": "/k.pem"}, "SPILLWAY_GATEWAY_TLS_CERT and SPILLWAY_GATEWAY_TLS_KEY"},
		{map[string]string{"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": ""}, "SPILLWAY_GATEWAY_ALLOWED_NETWORKS"},
		{map[string]string{"SPILLWAY_GATEWAY_TRUSTED_PROXIES": "10.0.0.1"}, "SPILLWAY_GATEWAY_TRUSTED_PROXIES"},
		{map[string]string{"SPILLWAY_GATEWAY_DNS_REFRESH": "0s"}, "SPILLWAY_GATEWAY_DNS_REFRESH"},
		{map[string]string{"SPILLWAY_GATEWAY_DRAIN": "-1s"}, `SPILLWAY_GATEWAY_DRAIN: "-1s" is not a duration of 0 or more`},
		{map[string]string{"SPILLWAY_GATEWAY_DRAIN": "5"}, `SPILLWAY_GATEWAY_DRAIN: "5" is not a duration of 0 or more`},
		{map[string]string{"SPILLWAY_GATEWAY_SHUTDOWN_GRACE": "0s"}, `SPILLWAY_GATEWAY_SHUTDOWN_GRACE: "0s" is not a positive duration`},
		{map[string]string{"SPILLWAY_GATEWAY_SHUTDOWN_GRACE": "x"}, `SPILLWAY_GATEWAY_SHUTDOWN_GRACE: "x" is not a positive duration`},
		{map[string]string{"SPILLWAY_GATEWAY_MAX_CONNECTIONS": "0"}, `SPILLWAY_GATEWAY_MAX_CONNECTIONS: "0" is not a positive integer`},
		{map[string]string{"SPILLWAY_GATEWAY_MAX_CONNECTIONS": "1e4"}, `SPILLWAY_GATEWAY_MAX_CONNECTIONS: "1e4" is not a positive integer`},
		{map[string]string{"SPILLWAY_GATEWAY_MAX_IN_FLIGHT": "-1"}, `SPILLWAY_GATEWAY_MAX_IN_FLIGHT: "-1" is not a positive integer`},
		{map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": "-0.1"}, "SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"},
		{map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": "1.5"}, "SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"},
		{map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": "NaN"}, "SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"},
		{map[string]string{"SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE": "10%"}, "SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"},
		{map[string]string{"SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO": "-0.5"}, `SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO: "-0.5" is not a number from 0 to 1`},
		{map[string]string{"SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO": "2"}, "SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO"},
		{map[string]string{"SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO": "NaN"}, "SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO"},
		{map[string]string{"SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO": "1%"}, "SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO"},
		{map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": "otel-collector:4317"}, "SPILLWAY_GATEWAY_OTLP_ENDPOINT"},
		{map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": "grpc://otel-collector:4317"}, "SPILLWAY_GATEWAY_OTLP_ENDPOINT"},
		{map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": "http://otel-collector"}, "SPILLWAY_GATEWAY_OTLP_ENDPOINT"},
		{map[string]string{"SPILLWAY_GATEWAY_OTLP_ENDPOINT": "http://:4317"}, "SPILLWAY_GATEWAY_OTLP_ENDPOINT"},
		{map[string]string{"SPILLWAY_GATEWAY_REDIS_ADDRS": "redis-0"}, `SPILLWAY_GATEWAY_REDIS_ADDRS: "redis-0" is not host:port`},
		{map[string]string{"SPILLWAY_GATEWAY_REDIS_ADDRS": "redis-0:6379,:6379"}, `SPILLWAY_GATEWAY_REDIS_ADDRS: ":6379"`},
		{map[string]string{"SPILLWAY_REDIS_ADDR_MAP": "redis-0:6379"}, "SPILLWAY_REDIS_ADDR_MAP"},
		{map[string]string{"SPILLWAY_GATEWAY_DEV_MODE": "yes"}, `SPILLWAY_GATEWAY_DEV_MODE: "yes" is not true or false`},
		// Bearer tokens (SEC-04, ADR-0039).
		{map[string]string{"SPILLWAY_GATEWAY_JWT_JWKS_URL": "https://kc/certs"}, "SPILLWAY_GATEWAY_JWT_JWKS_URL, SPILLWAY_GATEWAY_JWT_AUDIENCE and SPILLWAY_GATEWAY_JWT_CA: set with SPILLWAY_GATEWAY_JWT_ISSUER only"},
		{map[string]string{"SPILLWAY_GATEWAY_JWT_AUDIENCE": "spillway-gateway"}, "SPILLWAY_GATEWAY_JWT_JWKS_URL, SPILLWAY_GATEWAY_JWT_AUDIENCE"},
		{map[string]string{"SPILLWAY_GATEWAY_JWT_CA": "/ca.pem"}, "SPILLWAY_GATEWAY_JWT_JWKS_URL, SPILLWAY_GATEWAY_JWT_AUDIENCE"},
		{map[string]string{"SPILLWAY_GATEWAY_JWT_ISSUER": "https://kc/realms/x", "SPILLWAY_GATEWAY_JWT_AUDIENCE": "a"}, "SPILLWAY_GATEWAY_JWT_JWKS_URL: required"},
		{map[string]string{"SPILLWAY_GATEWAY_JWT_ISSUER": "https://kc/realms/x", "SPILLWAY_GATEWAY_JWT_JWKS_URL": "https://kc/certs"}, "SPILLWAY_GATEWAY_JWT_AUDIENCE: required with SPILLWAY_GATEWAY_JWT_ISSUER"},
		{map[string]string{"SPILLWAY_GATEWAY_JWT_ISSUER": "http://kc/realms/x", "SPILLWAY_GATEWAY_JWT_JWKS_URL": "https://kc/certs", "SPILLWAY_GATEWAY_JWT_AUDIENCE": "a"}, "SPILLWAY_GATEWAY_JWT_ISSUER: an http:// URL is accepted only in dev mode"},
		{map[string]string{"SPILLWAY_GATEWAY_JWT_ISSUER": "https://kc/realms/x", "SPILLWAY_GATEWAY_JWT_JWKS_URL": "http://kc/certs", "SPILLWAY_GATEWAY_JWT_AUDIENCE": "a"}, "SPILLWAY_GATEWAY_JWT_JWKS_URL: an http:// URL is accepted only in dev mode"},
		{map[string]string{"SPILLWAY_GATEWAY_JWT_ISSUER": "kc/realms/x", "SPILLWAY_GATEWAY_JWT_JWKS_URL": "https://kc/certs", "SPILLWAY_GATEWAY_JWT_AUDIENCE": "a"}, "SPILLWAY_GATEWAY_JWT_ISSUER: want an absolute https:// URL"},
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

		shutdownGrace: 5 * time.Second,

		maxConnections: defaultMaxConnections,
		maxInFlight:    defaultMaxInFlight,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, slog.New(slog.DiscardHandler), nil, noFlush) }()

	traffic, admin := "http://"+s.httpAddr, "http://"+s.adminAddr
	waitFor(t, "/readyz 200", func() bool { code, _ := get(t, admin+"/readyz"); return code == http.StatusOK })
	_, port, _ := net.SplitHostPort(s.adminAddr)
	for _, addr := range []string{s.adminAddr, ":" + port} {
		if err := healthcheck(t.Context(), adminEnv(addr)); err != nil {
			t.Errorf("healthcheck with admin address %s: %v", addr, err)
		}
	}
	if _, body := get(t, admin+"/metrics"); !strings.Contains(body, "\ngw_config_version 7\n") || !strings.Contains(body, "\ngo_goroutines ") ||
		strings.Contains(body, "gw_config_sync_age_seconds") {
		t.Errorf("/metrics lacks gw_config_version 7 or go_goroutines, or has the sync age, which static mode does not export:\n%s", body)
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
	if err := serve(t.Context(), s, slog.New(slog.DiscardHandler), nil, noFlush); err == nil || !strings.Contains(err.Error(), `consumer "a": defined twice`) {
		t.Errorf("serve = %v, want the identity error", err)
	}
}

// TestServeRedisPolicies: a config with a Redis policy stops a gateway that
// has no Redis; with Redis set but unreachable, the gateway is ready anyway
// (GW-24), and the policy allows until failure modes arrive (ADR-0021).
func TestServeRedisPolicies(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "upstream") }))
	t.Cleanup(up.Close)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "version: 1\nupstreams: [{ name: up, endpoints: [{ address: '" + up.Listener.Addr().String() + "' }] }]\n" +
		"routes: [{ name: all, match: { path_prefix: / }, upstream: up }]\n" +
		"policies: [{ name: shared, level: route, match: { routes: ['*'] }, key_template: '{route}', algorithm: gcra, limits: [{ rate: 1, period: 1h, burst: 1 }] }]\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	s := settings{configPath: cfg, httpAddr: freeAddr(t), adminAddr: freeAddr(t), allow: proxy.Allowlist{netip.MustParsePrefix("127.0.0.0/8")}, dnsRefresh: time.Minute, shutdownGrace: 5 * time.Second, maxConnections: defaultMaxConnections, maxInFlight: defaultMaxInFlight}
	if err := serve(t.Context(), s, slog.New(slog.DiscardHandler), nil, noFlush); err == nil || !strings.Contains(err.Error(), `policy "shared" runs in Redis (local is not true), but SPILLWAY_GATEWAY_REDIS_ADDRS is not set`) {
		t.Errorf("serve without Redis = %v, want the Redis policy error", err)
	}

	// Plain HTTP only: the plain-HTTP server sets an empty TLSConfig when
	// it starts, which must not open a TLS listener (run with -race).
	s.tlsAddr = freeAddr(t)
	s.redisAddrs = []string{freeAddr(t)} // nothing listens there
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, slog.New(slog.DiscardHandler), nil, noFlush) }()
	waitFor(t, "/readyz 200", func() bool { code, _ := get(t, "http://"+s.adminAddr+"/readyz"); return code == http.StatusOK })
	for range 2 {
		if code, body := get(t, "http://"+s.httpAddr+"/x"); code != http.StatusOK || body != "upstream" {
			t.Errorf("GET /x with Redis down = %d %q, want 200 from the upstream", code, body)
		}
	}
	if c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", s.tlsAddr); err == nil {
		_ = c.Close()
		t.Errorf("a TLS listener on %s without a certificate", s.tlsAddr)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
}

// TestServeTokenRoutes: a route that needs bearer tokens stops a gateway
// without JWT settings (ADR-0039), and a static one, whose consumers have
// no JWT subjects (ADR-0045); api_key routes need neither.
func TestServeTokenRoutes(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	base := settings{configPath: cfg, httpAddr: freeAddr(t), adminAddr: freeAddr(t), dnsRefresh: time.Minute}
	withJWT := base
	withJWT.jwtIssuer, withJWT.jwtJWKSURL, withJWT.jwtAudience, withJWT.devMode = "http://127.0.0.1:1/realms/x", "http://127.0.0.1:1/certs", "spillway-gateway", true
	for _, tt := range []struct {
		mode string
		s    settings
		want string
	}{
		{"jwt", base, `route "r" uses auth_mode jwt, but SPILLWAY_GATEWAY_JWT_ISSUER is not set`},
		{"either", base, `route "r" uses auth_mode either, but SPILLWAY_GATEWAY_JWT_ISSUER is not set`},
		{"jwt", withJWT, `route "r" uses auth_mode jwt, which needs managed mode`},
		{"either", withJWT, `route "r" uses auth_mode either, which needs managed mode`},
	} {
		yaml := "version: 1\nupstreams: [{ name: up, endpoints: [{ address: '127.0.0.1:1' }] }]\nroutes: [{ name: r, match: { path_prefix: / }, upstream: up, auth_mode: " + tt.mode + " }]\n"
		if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := serve(t.Context(), tt.s, slog.New(slog.DiscardHandler), nil, noFlush); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("auth_mode %s, issuer %q: serve = %v, want %q", tt.mode, tt.s.jwtIssuer, err, tt.want)
		}
	}
	snap, err := config.Compile(&configpb.Config{
		Upstreams: []*configpb.Upstream{{Name: "up", Endpoints: []*configpb.Endpoint{{Address: "127.0.0.1:1"}}}},
		Routes: []*configpb.Route{
			{Name: "keys", Upstream: "up", Match: &configpb.RouteMatch{PathPrefix: "/k"}, AuthMode: config.AuthAPIKey},
			{Name: "tokens", Upstream: "up", Match: &configpb.RouteMatch{PathPrefix: "/t"}, AuthMode: config.AuthJWT},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAuth(snap, true, true); err != nil {
		t.Errorf("managed, with JWT settings: %v", err)
	}
	snap.Routes = snap.Routes[:1]
	if err := checkAuth(snap, false, false); err != nil {
		t.Errorf("an api_key route, static, without JWT settings: %v", err)
	}
}

// TestJWKSClient: SPILLWAY_GATEWAY_JWT_CA adds its certificates to the
// roots the keys are fetched with; a file without one stops the start.
func TestJWKSClient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") }))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := jwksClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET with the CA: %v", err)
	}
	_ = resp.Body.Close()
	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{notPEM: "SPILLWAY_GATEWAY_JWT_CA: no PEM certificate in", "/nonexistent/ca.pem": "SPILLWAY_GATEWAY_JWT_CA: open"} {
		if _, err := jwksClient(file); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("jwksClient(%s) = %v, want %q", file, err, want)
		}
	}
}

func noFlush(context.Context) {}

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
