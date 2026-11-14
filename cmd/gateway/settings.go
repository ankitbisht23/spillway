package main

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/managed"
	"github.com/ankitbisht23/spillway/internal/mtls"
	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// settings are the gateway's own settings. They come from the environment,
// never from a config snapshot, so a config change can't widen what the
// gateway may reach (ADR-0008).
type settings struct {
	configPath string // SPILLWAY_GATEWAY_CONFIG: YAML config file (CP-05); static mode only
	identity   string // SPILLWAY_GATEWAY_IDENTITY: YAML identity file with consumers and static API keys (GW-17), optional; static mode only

	// Managed mode (ADR-0038): on when the control plane is set.
	controlPlane string // SPILLWAY_GATEWAY_CONTROL_PLANE: the ConfigService's host:port
	snapshot     string // SPILLWAY_GATEWAY_SNAPSHOT: the on-disk last good snapshot (CP-07), default /var/lib/spillway/snapshot.pb
	// The stream's mTLS (SEC-02, ADR-0040), managed mode only: PEM files,
	// reloaded when they change. The replica name is the certificate's
	// DNS name (D7).
	streamTLS    mtls.Files // SPILLWAY_GATEWAY_CP_CERT, SPILLWAY_GATEWAY_CP_KEY: the client certificate and key; SPILLWAY_GATEWAY_CP_CA: the stream CA the control plane's certificate must chain to
	cpServerName string     // SPILLWAY_GATEWAY_CP_SERVER_NAME: the name the control plane's certificate must carry, default the host part of SPILLWAY_GATEWAY_CONTROL_PLANE

	httpAddr   string          // SPILLWAY_GATEWAY_HTTP_ADDR: plain-HTTP listener, dev and benchmarks only, off when empty (SEC-01)
	tlsAddr    string          // SPILLWAY_GATEWAY_TLS_ADDR: TLS listener (GW-04, SEC-01), default :8443
	tlsCert    string          // SPILLWAY_GATEWAY_TLS_CERT: PEM certificate chain file; TLS is on when it and the key are set
	tlsKey     string          // SPILLWAY_GATEWAY_TLS_KEY: PEM private key file (SEC-09)
	adminAddr  string          // SPILLWAY_GATEWAY_ADMIN_ADDR: admin listener (GW-24, GW-25), default 127.0.0.1:8001
	allow      proxy.Allowlist // SPILLWAY_GATEWAY_ALLOWED_NETWORKS: comma-separated CIDRs (SEC-06)
	dnsRefresh time.Duration   // SPILLWAY_GATEWAY_DNS_REFRESH: upstream re-resolution interval (GW-06), default 30s

	// Shutdown (GW-23, ADR-0030)
	drain         time.Duration // SPILLWAY_GATEWAY_DRAIN: how long the gateway keeps serving after SIGTERM with /readyz failing, default 5s, 0 allowed
	shutdownGrace time.Duration // SPILLWAY_GATEWAY_SHUTDOWN_GRACE: after the drain, how long in-flight requests get to finish, default 15s

	// Load shedding (GW-26, ADR-0030); the admin port is exempt
	maxConnections int64 // SPILLWAY_GATEWAY_MAX_CONNECTIONS: open client connections across the traffic listeners, default 8192
	maxInFlight    int64 // SPILLWAY_GATEWAY_MAX_IN_FLIGHT: requests being handled at once, default 4096

	// Clients (SEC-05)
	trusted proxy.TrustedProxies // SPILLWAY_GATEWAY_TRUSTED_PROXIES: comma-separated CIDRs of proxies whose X-Forwarded-For is believed, default none

	// Rate limiting (ADR-0021)
	redisAddrs   []string          // SPILLWAY_GATEWAY_REDIS_ADDRS: comma-separated host:port seeds of the limiter's Redis Cluster, none by default
	redisAddrMap map[string]string // SPILLWAY_REDIS_ADDR_MAP: dev and tests only (ADR-0004)
	redisAuth    redis.Auth        // SPILLWAY_GATEWAY_REDIS_USERNAME, and SPILLWAY_GATEWAY_REDIS_PASSWORD or _FILE (SEC-09): Redis AUTH, off when the password is unset
	devMode      bool              // SPILLWAY_GATEWAY_DEV_MODE: dev-only behaviour, the Spillway-Decision-Time header (ADR-0021), default false

	// Logs and traces (OB-03, OB-02)
	accessLogSampleRate float64 // SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE: share of 1xx-3xx and upstream 4xx requests access-logged, 0 to 1, default 1
	otlpEndpoint        string  // SPILLWAY_GATEWAY_OTLP_ENDPOINT: OTel Collector URL for logs and traces over OTLP/gRPC, off when empty
	traceSampleRatio    float64 // SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO: head-sampling ratio of traces without a parent decision, 0 to 1, default 1 (ADR-0030)
}

// defaultAdminAddr keeps the admin port on loopback unless an internal
// interface is named (GW-25, SEC-02).
const defaultAdminAddr = "127.0.0.1:8001"

// The load-shedding caps' defaults (GW-26), with their file descriptor and
// memory budget in ADR-0030.
const (
	defaultMaxConnections = 8192
	defaultMaxInFlight    = 4096
)

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

		drain:         5 * time.Second,
		shutdownGrace: 15 * time.Second,

		maxConnections: defaultMaxConnections,
		maxInFlight:    defaultMaxInFlight,

		accessLogSampleRate: 1,
		traceSampleRatio:    1,
		otlpEndpoint:        getenv("SPILLWAY_GATEWAY_OTLP_ENDPOINT"),
	}
	s.controlPlane, s.snapshot = getenv("SPILLWAY_GATEWAY_CONTROL_PLANE"), getenv("SPILLWAY_GATEWAY_SNAPSHOT")
	s.streamTLS = mtls.Files{Cert: getenv("SPILLWAY_GATEWAY_CP_CERT"), Key: getenv("SPILLWAY_GATEWAY_CP_KEY"), CA: getenv("SPILLWAY_GATEWAY_CP_CA")}
	s.cpServerName = getenv("SPILLWAY_GATEWAY_CP_SERVER_NAME")
	if err := s.mode(); err != nil {
		return s, err
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
	for _, a := range strings.Split(getenv("SPILLWAY_GATEWAY_REDIS_ADDRS"), ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if host, port, err := net.SplitHostPort(a); err != nil || host == "" || port == "" {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_REDIS_ADDRS: %q is not host:port", a)
		}
		s.redisAddrs = append(s.redisAddrs, a)
	}
	if s.redisAddrMap, err = redis.ParseAddrMap(getenv(redis.AddrMapEnv)); err != nil {
		return s, err
	}
	s.redisAuth.Username = getenv("SPILLWAY_GATEWAY_REDIS_USERNAME")
	if s.redisAuth.Password, err = secret.FromEnv(getenv, "SPILLWAY_GATEWAY_REDIS_PASSWORD"); err != nil {
		return s, err
	}
	if s.redisAuth.Username != "" && !s.redisAuth.Password.IsSet() {
		return s, errors.New("SPILLWAY_GATEWAY_REDIS_USERNAME: set with SPILLWAY_GATEWAY_REDIS_PASSWORD (or _FILE) only")
	}
	if v := getenv("SPILLWAY_GATEWAY_DEV_MODE"); v != "" {
		if s.devMode, err = strconv.ParseBool(v); err != nil {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_DEV_MODE: %q is not true or false", v)
		}
	}
	if v := getenv("SPILLWAY_GATEWAY_DNS_REFRESH"); v != "" {
		if s.dnsRefresh, err = time.ParseDuration(v); err != nil || s.dnsRefresh <= 0 {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_DNS_REFRESH: %q is not a positive duration", v)
		}
	}
	if v := getenv("SPILLWAY_GATEWAY_DRAIN"); v != "" {
		if s.drain, err = time.ParseDuration(v); err != nil || s.drain < 0 {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_DRAIN: %q is not a duration of 0 or more", v)
		}
	}
	if v := getenv("SPILLWAY_GATEWAY_SHUTDOWN_GRACE"); v != "" {
		if s.shutdownGrace, err = time.ParseDuration(v); err != nil || s.shutdownGrace <= 0 {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_SHUTDOWN_GRACE: %q is not a positive duration", v)
		}
	}
	if v := getenv("SPILLWAY_GATEWAY_MAX_CONNECTIONS"); v != "" {
		if s.maxConnections, err = strconv.ParseInt(v, 10, 64); err != nil || s.maxConnections <= 0 {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_MAX_CONNECTIONS: %q is not a positive integer", v)
		}
	}
	if v := getenv("SPILLWAY_GATEWAY_MAX_IN_FLIGHT"); v != "" {
		if s.maxInFlight, err = strconv.ParseInt(v, 10, 64); err != nil || s.maxInFlight <= 0 {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_MAX_IN_FLIGHT: %q is not a positive integer", v)
		}
	}
	if v := getenv("SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE"); v != "" {
		if s.accessLogSampleRate, err = strconv.ParseFloat(v, 64); err != nil || !(s.accessLogSampleRate >= 0 && s.accessLogSampleRate <= 1) {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE: %q is not a number from 0 to 1", v)
		}
	}
	if v := getenv("SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO"); v != "" {
		if s.traceSampleRatio, err = strconv.ParseFloat(v, 64); err != nil || !(s.traceSampleRatio >= 0 && s.traceSampleRatio <= 1) {
			return s, fmt.Errorf("SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO: %q is not a number from 0 to 1", v)
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

// mode checks the settings of static or managed mode (ADR-0038): static
// needs the config file; managed needs the control plane and its mTLS files
// (ADR-0040) and takes config and identity only from it, so the files are
// refused. The mTLS settings are ignored in static mode, so compose can set
// them on every gateway.
func (s *settings) mode() error {
	if s.controlPlane == "" {
		if s.configPath == "" {
			return errors.New("SPILLWAY_GATEWAY_CONFIG: required, unless SPILLWAY_GATEWAY_CONTROL_PLANE is set (managed mode)")
		}
		if s.snapshot != "" {
			return errors.New("SPILLWAY_GATEWAY_SNAPSHOT: managed mode only; set SPILLWAY_GATEWAY_CONTROL_PLANE")
		}
		return nil
	}
	if s.configPath != "" || s.identity != "" {
		return errors.New("SPILLWAY_GATEWAY_CONFIG and SPILLWAY_GATEWAY_IDENTITY: not with SPILLWAY_GATEWAY_CONTROL_PLANE; a managed gateway takes config and identity from the control plane")
	}
	host, port, err := net.SplitHostPort(s.controlPlane)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("SPILLWAY_GATEWAY_CONTROL_PLANE: %q is not host:port", s.controlPlane)
	}
	if s.streamTLS.Cert == "" || s.streamTLS.Key == "" || s.streamTLS.CA == "" {
		return errors.New("SPILLWAY_GATEWAY_CP_CERT, SPILLWAY_GATEWAY_CP_KEY and SPILLWAY_GATEWAY_CP_CA: required with SPILLWAY_GATEWAY_CONTROL_PLANE; the config stream is mTLS only (SEC-02)")
	}
	s.cpServerName = cmp.Or(s.cpServerName, host)
	if s.snapshot == "" {
		s.snapshot = managed.DefaultSnapshotPath
	}
	return nil
}
