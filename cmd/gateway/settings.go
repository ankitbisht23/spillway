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
	allow      proxy.Allowlist // SPILLWAY_GATEWAY_ALLOWED_NETWORKS: comma-separated CIDRs (SEC-06)
	dnsRefresh time.Duration   // SPILLWAY_GATEWAY_DNS_REFRESH: upstream re-resolution interval (GW-06), default 30s
}

func loadSettings(getenv func(string) string) (settings, error) {
	s := settings{
		configPath: getenv("SPILLWAY_GATEWAY_CONFIG"),
		httpAddr:   getenv("SPILLWAY_GATEWAY_HTTP_ADDR"),
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
