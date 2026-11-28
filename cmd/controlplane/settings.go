package main

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ankitbisht23/spillway/internal/auth"
	"github.com/ankitbisht23/spillway/internal/controlplane/live"
	"github.com/ankitbisht23/spillway/internal/controlplane/quotaflush"
	"github.com/ankitbisht23/spillway/internal/mtls"
	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// settings are the control plane's settings, from the environment only
// (ADR-0008, ADR-0036). Each arrives with the M5 task that uses it.
type settings struct {
	databaseURL      string          // SPILLWAY_CP_DATABASE_URL: postgres://spillway_app@host:5432/spillway?sslmode=..., without a password
	databasePassword secret.Value    // SPILLWAY_CP_DATABASE_PASSWORD or _FILE: the app role's password (SEC-09)
	hitterSecret     secret.Value    // SPILLWAY_CP_HEAVY_HITTER_SECRET or _FILE: the gateways' heavy-hitter secret, for lookups (SEC-09, ADR-0053)
	httpAddr         string          // SPILLWAY_CP_HTTP_ADDR: REST listener, default :8080
	grpcAddr         string          // SPILLWAY_CP_GRPC_ADDR: ConfigService listener, active replica only, default :9090 (ADR-0038)
	adminAddr        string          // SPILLWAY_CP_ADMIN_ADDR: /metrics, pprof, /healthz, /readyz, default 127.0.0.1:8081 (SEC-02)
	allow            proxy.Allowlist // SPILLWAY_CP_ALLOWED_NETWORKS: upstream allow-list, the gateways' (SEC-06)

	// The config stream's mTLS (SEC-02, ADR-0040): PEM files, reloaded
	// when they change; there is no plaintext gRPC listener.
	grpcTLS mtls.Files // SPILLWAY_CP_GRPC_TLS_CERT, SPILLWAY_CP_GRPC_TLS_KEY: the server certificate and key; SPILLWAY_CP_GRPC_CLIENT_CA: the stream CA that gateway certificates must chain to
	// fleet are the client certificate names that stand for a fleet of
	// gateways (ADR-0075 §D11): each replica of a fleet F names itself in
	// replica_id as F-<suffix>, a DNS label; in Kubernetes F is the
	// Deployment's name and the replica a pod's. Empty by default, so
	// every certificate names one replica (D7).
	fleet []string // SPILLWAY_CP_GRPC_FLEET_IDENTITIES: comma-separated names

	// Bearer tokens (CP-11, ADR-0040)
	oidcIssuer   string // SPILLWAY_CP_OIDC_ISSUER: the browser-facing issuer every token's iss must equal
	oidcJWKSURL  string // SPILLWAY_CP_OIDC_JWKS_URL: where the issuer's keys are fetched (in-network in dev, ADR-0004)
	oidcAudience string // SPILLWAY_CP_OIDC_AUDIENCE: a value every token's aud must contain, default spillway-controlplane
	oidcCA       string // SPILLWAY_CP_OIDC_CA: PEM certificates trusted for the JWKS URL besides the system roots, optional (SEC-04)
	devMode      bool   // SPILLWAY_CP_DEV_MODE: accept http:// issuer and JWKS URLs (SEC-04), default false

	// SPILLWAY_CP_DEMO_MODE: demo mode (SC-08, ADR-0095), default false;
	// only the demo stack sets it, from DEMO_MODE. Off, everything under
	// /api/v1/demo is 404.
	demoMode bool
	// SPILLWAY_CP_DEMO_TOXIPROXY_URL: the toxiproxy API the demo's fault
	// switches use (SC-03, ADR-0095 §1, §5), http or https, no user info;
	// required in demo mode, ignored otherwise. The demo stack sets
	// http://toxiproxy:8474.
	demoToxiproxyURL string
	// SPILLWAY_CP_DEMO_GATEWAYS: the demo gateways' base URLs, comma
	// separated, that the Playground sends to round robin (UI-07,
	// ADR-0095 §1); required in demo mode, ignored otherwise. The demo
	// stack sets http://gateway-1:8000,http://gateway-2:8000,http://gateway-3:8000.
	demoGateways []string
	// SPILLWAY_CP_TRUSTED_PROXIES: comma-separated CIDRs of the proxies
	// whose CF-Connecting-IP names the client, read in every mode (SEC-05,
	// ADR-0097 §2); empty by default (trust nobody). The demo stack sets
	// the demo proxy's address, 172.29.255.10/32.
	trustedProxies proxy.TrustedProxies

	// SPILLWAY_CP_PROMETHEUS_URL: the Prometheus the live-data stream's
	// named queries and the shadow report's counts run against (UI-01,
	// UI-03, ADR-0059, ADR-0060), http or https, no user info; not a
	// secret. Unset (nil), those queries answer error events and the
	// stream still serves the Postgres-backed ones; the report's counts
	// are null.
	prometheusURL *url.URL

	// The quota flusher's Redis Cluster (RL-10, ADR-0070):
	// SPILLWAY_CP_REDIS_ADDRS, comma-separated host:port seeds, none
	// disables the flusher; SPILLWAY_CP_REDIS_USERNAME, and
	// SPILLWAY_CP_REDIS_PASSWORD or _FILE (SEC-09); SPILLWAY_REDIS_ADDR_MAP,
	// dev and tests only (ADR-0004).
	redis quotaflush.Redis
}

// migrateSettings are what `controlplane migrate` needs (CP-19).
type migrateSettings struct {
	databaseURL      string       // SPILLWAY_CP_MIGRATE_DATABASE_URL: the owner role's URL, without a password
	password         secret.Value // SPILLWAY_CP_MIGRATE_PASSWORD or _FILE: the owner role's password
	databasePassword secret.Value // SPILLWAY_CP_DATABASE_PASSWORD or _FILE: the app role's password, which migrate sets
}

const defaultAdminAddr = "127.0.0.1:8081"

func loadSettings(getenv func(string) string) (settings, error) {
	s := settings{
		databaseURL: getenv("SPILLWAY_CP_DATABASE_URL"),
		httpAddr:    cmp.Or(getenv("SPILLWAY_CP_HTTP_ADDR"), ":8080"),
		grpcAddr:    cmp.Or(getenv("SPILLWAY_CP_GRPC_ADDR"), ":9090"),
		adminAddr:   adminAddr(getenv),
	}
	if s.databaseURL == "" {
		return s, errors.New("SPILLWAY_CP_DATABASE_URL is required")
	}
	var err error
	if s.databasePassword, err = secret.Required(getenv, "SPILLWAY_CP_DATABASE_PASSWORD"); err != nil {
		return s, err
	}
	if s.hitterSecret, err = secret.Required(getenv, "SPILLWAY_CP_HEAVY_HITTER_SECRET"); err != nil {
		return s, err
	}
	s.grpcTLS = mtls.Files{Cert: getenv("SPILLWAY_CP_GRPC_TLS_CERT"), Key: getenv("SPILLWAY_CP_GRPC_TLS_KEY"), CA: getenv("SPILLWAY_CP_GRPC_CLIENT_CA")}
	if s.grpcTLS.Cert == "" || s.grpcTLS.Key == "" || s.grpcTLS.CA == "" {
		return s, errors.New("SPILLWAY_CP_GRPC_TLS_CERT, SPILLWAY_CP_GRPC_TLS_KEY and SPILLWAY_CP_GRPC_CLIENT_CA are required: the config stream is mTLS only (SEC-02)")
	}
	for f := range strings.SplitSeq(getenv("SPILLWAY_CP_GRPC_FLEET_IDENTITIES"), ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if !mtls.ValidReplicaName(f) {
			return s, fmt.Errorf("SPILLWAY_CP_GRPC_FLEET_IDENTITIES: %q is not a DNS label (lower-case letters, digits and '-', at most 63)", f)
		}
		s.fleet = append(s.fleet, f)
	}
	nets := getenv("SPILLWAY_CP_ALLOWED_NETWORKS")
	if nets == "" {
		return s, errors.New("SPILLWAY_CP_ALLOWED_NETWORKS is required: the gateways' upstream allow-list (SEC-06)")
	}
	if s.allow, err = proxy.ParseAllowlist(nets); err != nil {
		return s, fmt.Errorf("SPILLWAY_CP_ALLOWED_NETWORKS: %w", err)
	}
	if v := getenv("SPILLWAY_CP_DEV_MODE"); v != "" {
		if s.devMode, err = strconv.ParseBool(v); err != nil {
			return s, fmt.Errorf("SPILLWAY_CP_DEV_MODE: %q is not true or false", v)
		}
	}
	if s.trustedProxies, err = proxy.ParseTrustedProxies(getenv("SPILLWAY_CP_TRUSTED_PROXIES")); err != nil {
		return s, fmt.Errorf("SPILLWAY_CP_TRUSTED_PROXIES: %w", err)
	}
	if v := getenv("SPILLWAY_CP_DEMO_MODE"); v != "" {
		if s.demoMode, err = strconv.ParseBool(v); err != nil {
			return s, errors.New("SPILLWAY_CP_DEMO_MODE: want true or false")
		}
	}
	if s.demoMode {
		// Any http or https URL without user info, query or fragment, as
		// for Prometheus.
		v := getenv("SPILLWAY_CP_DEMO_TOXIPROXY_URL")
		if v == "" {
			return s, errors.New("SPILLWAY_CP_DEMO_TOXIPROXY_URL is required in demo mode: the toxiproxy API of the fault switches (SC-03)")
		}
		if _, err := live.ParsePrometheusURL(v); err != nil {
			return s, fmt.Errorf("SPILLWAY_CP_DEMO_TOXIPROXY_URL: %w", err)
		}
		s.demoToxiproxyURL = v
		if s.demoGateways, err = parseGateways(getenv("SPILLWAY_CP_DEMO_GATEWAYS")); err != nil {
			return s, fmt.Errorf("SPILLWAY_CP_DEMO_GATEWAYS: %w", err)
		}
	}
	s.oidcIssuer, s.oidcJWKSURL = getenv("SPILLWAY_CP_OIDC_ISSUER"), getenv("SPILLWAY_CP_OIDC_JWKS_URL")
	s.oidcAudience = cmp.Or(getenv("SPILLWAY_CP_OIDC_AUDIENCE"), "spillway-controlplane")
	s.oidcCA = getenv("SPILLWAY_CP_OIDC_CA")
	for name, u := range map[string]string{"SPILLWAY_CP_OIDC_ISSUER": s.oidcIssuer, "SPILLWAY_CP_OIDC_JWKS_URL": s.oidcJWKSURL} {
		if err := auth.CheckURL(u, s.devMode); err != nil {
			return s, fmt.Errorf("%s: %w", name, err)
		}
	}
	if v := getenv("SPILLWAY_CP_PROMETHEUS_URL"); v != "" {
		if s.prometheusURL, err = live.ParsePrometheusURL(v); err != nil {
			return s, fmt.Errorf("SPILLWAY_CP_PROMETHEUS_URL: %w", err)
		}
	}
	return s, s.loadRedis(getenv)
}

func (s *settings) loadRedis(getenv func(string) string) error {
	for _, a := range strings.Split(getenv("SPILLWAY_CP_REDIS_ADDRS"), ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if host, port, err := net.SplitHostPort(a); err != nil || host == "" || port == "" {
			return fmt.Errorf("SPILLWAY_CP_REDIS_ADDRS: %q is not host:port", a)
		}
		s.redis.Addrs = append(s.redis.Addrs, a)
	}
	var err error
	if s.redis.AddrMap, err = redis.ParseAddrMap(getenv(redis.AddrMapEnv)); err != nil {
		return err
	}
	s.redis.Username = getenv("SPILLWAY_CP_REDIS_USERNAME")
	if s.redis.Password, err = secret.FromEnv(getenv, "SPILLWAY_CP_REDIS_PASSWORD"); err != nil {
		return err
	}
	if s.redis.Username != "" && !s.redis.Password.IsSet() {
		return errors.New("SPILLWAY_CP_REDIS_USERNAME: set with SPILLWAY_CP_REDIS_PASSWORD (or _FILE) only")
	}
	return nil
}

func loadMigrateSettings(getenv func(string) string) (migrateSettings, error) {
	s := migrateSettings{databaseURL: getenv("SPILLWAY_CP_MIGRATE_DATABASE_URL")}
	if s.databaseURL == "" {
		return s, errors.New("SPILLWAY_CP_MIGRATE_DATABASE_URL is required")
	}
	var err error
	if s.password, err = secret.Required(getenv, "SPILLWAY_CP_MIGRATE_PASSWORD"); err != nil {
		return s, err
	}
	s.databasePassword, err = secret.Required(getenv, "SPILLWAY_CP_DATABASE_PASSWORD")
	return s, err
}

func adminAddr(getenv func(string) string) string {
	return cmp.Or(getenv("SPILLWAY_CP_ADMIN_ADDR"), defaultAdminAddr)
}

// parseGateways reads SPILLWAY_CP_DEMO_GATEWAYS: one or more distinct http
// or https base URLs, comma separated, each with a host and no user info,
// path, query or fragment.
func parseGateways(v string) ([]string, error) {
	var out []string
	for g := range strings.SplitSeq(v, ",") {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		u, err := live.ParsePrometheusURL(g)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", g, err)
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("%q: a path is not allowed", g)
		}
		g = strings.TrimSuffix(g, "/")
		if slices.Contains(out, g) {
			return nil, fmt.Errorf("%q is listed twice", g)
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil, errors.New("required in demo mode: the gateways the Playground sends to (UI-07)")
	}
	return out, nil
}
