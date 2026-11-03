package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/ankitbisht23/spillway/internal/proxy"
)

// settings are the gateway's own settings. They come from the environment,
// never from a config snapshot, so a config change can't widen what the
// gateway may reach (ADR-0008).
type settings struct {
	configPath string          // SPILLWAY_GATEWAY_CONFIG: YAML config file (CP-05)
	httpAddr   string          // SPILLWAY_GATEWAY_HTTP_ADDR: plain-HTTP listener, dev and benchmarks only (SEC-01)
	adminAddr  string          // SPILLWAY_GATEWAY_ADMIN_ADDR: admin listener (GW-24, GW-25), default 127.0.0.1:8001
	allow      proxy.Allowlist // SPILLWAY_GATEWAY_ALLOWED_NETWORKS: comma-separated CIDRs (SEC-06)
	dnsRefresh time.Duration   // SPILLWAY_GATEWAY_DNS_REFRESH: upstream re-resolution interval (GW-06), default 30s
}

// defaultAdminAddr keeps the admin port on loopback unless an internal
// interface is named (GW-25, SEC-02).
const defaultAdminAddr = "127.0.0.1:8001"

func loadSettings(getenv func(string) string) (settings, error) {
	s := settings{
		configPath: getenv("SPILLWAY_GATEWAY_CONFIG"),
		httpAddr:   getenv("SPILLWAY_GATEWAY_HTTP_ADDR"),
		adminAddr:  adminAddr(getenv),
		dnsRefresh: 30 * time.Second,
	}
	if s.configPath == "" {
		return s, errors.New("SPILLWAY_GATEWAY_CONFIG: required")
	}
	if s.httpAddr == "" {
		return s, errors.New("SPILLWAY_GATEWAY_HTTP_ADDR: required, the gateway has no other listener yet")
	}
	var err error
	if s.allow, err = proxy.ParseAllowlist(getenv("SPILLWAY_GATEWAY_ALLOWED_NETWORKS")); err != nil {
		return s, fmt.Errorf("SPILLWAY_GATEWAY_ALLOWED_NETWORKS: %w", err)
	}
	if v := getenv("SPILLWAY_GATEWAY_DNS_REFRESH"); v != "" {
		if s.dnsRefresh, err = time.ParseDuration(v); err != nil || s.dnsRefresh <= 0 {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_DNS_REFRESH: %q is not a positive duration", v)
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
