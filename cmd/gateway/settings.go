package main

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/ankitbisht23/spillway/internal/proxy"
)

// settings are the gateway's own settings. They come from the environment,
// never from a config snapshot, so a config change can't widen what the
// gateway may reach (ADR-0008).
type settings struct {
	configPath string          // SPILLWAY_GATEWAY_CONFIG: YAML config file (CP-05)
	identity   string          // SPILLWAY_GATEWAY_IDENTITY: YAML identity file with consumers and static API keys (GW-17), optional
	httpAddr   string          // SPILLWAY_GATEWAY_HTTP_ADDR: plain-HTTP listener, dev and benchmarks only, off when empty (SEC-01)
	tlsAddr    string          // SPILLWAY_GATEWAY_TLS_ADDR: TLS listener (GW-04, SEC-01), default :8443
	tlsCert    string          // SPILLWAY_GATEWAY_TLS_CERT: PEM certificate chain file; TLS is on when it and the key are set
	tlsKey     string          // SPILLWAY_GATEWAY_TLS_KEY: PEM private key file (SEC-09)
	adminAddr  string          // SPILLWAY_GATEWAY_ADMIN_ADDR: admin listener (GW-24, GW-25), default 127.0.0.1:8001
	allow      proxy.Allowlist // SPILLWAY_GATEWAY_ALLOWED_NETWORKS: comma-separated CIDRs (SEC-06)
	dnsRefresh time.Duration   // SPILLWAY_GATEWAY_DNS_REFRESH: upstream re-resolution interval (GW-06), default 30s

	// Clients (SEC-05)
	trusted proxy.TrustedProxies // SPILLWAY_GATEWAY_TRUSTED_PROXIES: comma-separated CIDRs of proxies whose X-Forwarded-For is believed, default none

	// Logs (OB-03)
	accessLogSampleRate float64 // SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE: share of 1xx-3xx and upstream 4xx requests access-logged, 0 to 1, default 1
	otlpEndpoint        string  // SPILLWAY_GATEWAY_OTLP_ENDPOINT: OTel Collector URL for logs over OTLP/gRPC, off when empty
}

// defaultAdminAddr keeps the admin port on loopback unless an internal
// interface is named (GW-25, SEC-02).
const defaultAdminAddr = "127.0.0.1:8001"

func loadSettings(getenv func(string) string) (settings, error) {
	s := settings{
		configPath: getenv("SPILLWAY_GATEWAY_CONFIG"),
		identity:   getenv("SPILLWAY_GATEWAY_IDENTITY"),
		httpAddr:   getenv("SPILLWAY_GATEWAY_HTTP_ADDR"),
		tlsAddr:    cmp.Or(getenv("SPILLWAY_GATEWAY_TLS_ADDR"), ":8443"),
		tlsCert:    getenv("SPILLWAY_GATEWAY_TLS_CERT"),
		tlsKey:     getenv("SPILLWAY_GATEWAY_TLS_KEY"),
		adminAddr:  adminAddr(getenv),
		dnsRefresh: 30 * time.Second,

		accessLogSampleRate: 1,
		otlpEndpoint:        getenv("SPILLWAY_GATEWAY_OTLP_ENDPOINT"),
	}
	if s.configPath == "" {
		return s, errors.New("SPILLWAY_GATEWAY_CONFIG: required")
	}
	if (s.tlsCert == "") != (s.tlsKey == "") {
		return s, errors.New("SPILLWAY_GATEWAY_TLS_CERT and SPILLWAY_GATEWAY_TLS_KEY: set both or neither")
	}
	if s.tlsCert == "" && s.httpAddr == "" {
		return s, errors.New("no listener: set SPILLWAY_GATEWAY_TLS_CERT and SPILLWAY_GATEWAY_TLS_KEY, or SPILLWAY_GATEWAY_HTTP_ADDR for plain HTTP (dev and benchmarks only)")
	}
	var err error
	if s.allow, err = proxy.ParseAllowlist(getenv("SPILLWAY_GATEWAY_ALLOWED_NETWORKS")); err != nil {
		return s, fmt.Errorf("SPILLWAY_GATEWAY_ALLOWED_NETWORKS: %w", err)
	}
	if s.trusted, err = proxy.ParseTrustedProxies(getenv("SPILLWAY_GATEWAY_TRUSTED_PROXIES")); err != nil {
		return s, fmt.Errorf("SPILLWAY_GATEWAY_TRUSTED_PROXIES: %w", err)
	}
	if v := getenv("SPILLWAY_GATEWAY_DNS_REFRESH"); v != "" {
		if s.dnsRefresh, err = time.ParseDuration(v); err != nil || s.dnsRefresh <= 0 {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_DNS_REFRESH: %q is not a positive duration", v)
		}
	}
	if v := getenv("SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"); v != "" {
		if s.accessLogSampleRate, err = strconv.ParseFloat(v, 64); err != nil || !(s.accessLogSampleRate >= 0 && s.accessLogSampleRate <= 1) {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE: %q is not a number from 0 to 1", v)
		}
	}
	if v := s.otlpEndpoint; v != "" {
		if u, err := url.Parse(v); err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.Port() == "" {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_OTLP_ENDPOINT: %q is not a URL such as http://otel-collector:4317", v)
		}
	}
	return s, nil
}

func adminAddr(getenv func(string) string) string {
	if v := getenv("SPILLWAY_GATEWAY_ADMIN_ADDR"); v != "" {
		return v
	}
	return defaultAdminAddr
}
