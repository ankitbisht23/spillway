package main

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"

	"github.com/ankitbisht23/spillway/internal/auth"
	"github.com/ankitbisht23/spillway/internal/mtls"
	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// settings are the control plane's settings, from the environment only
// (ADR-0008, ADR-0036). Each arrives with the M5 task that uses it.
type settings struct {
	databaseURL      string          // SPILLWAY_CP_DATABASE_URL: postgres://spillway_app@host:5432/spillway?sslmode=..., without a password
	databasePassword secret.Value    // SPILLWAY_CP_DATABASE_PASSWORD or _FILE: the app role's password (SEC-09)
	httpAddr         string          // SPILLWAY_CP_HTTP_ADDR: REST listener, default :8080
	grpcAddr         string          // SPILLWAY_CP_GRPC_ADDR: ConfigService listener, active replica only, default :9090 (ADR-0038)
	adminAddr        string          // SPILLWAY_CP_ADMIN_ADDR: /metrics, pprof, /healthz, /readyz, default 127.0.0.1:8081 (SEC-02)
	allow            proxy.Allowlist // SPILLWAY_CP_ALLOWED_NETWORKS: upstream allow-list, the gateways' (SEC-06)

	// The config stream's mTLS (SEC-02, ADR-0040): PEM files, reloaded
	// when they change; there is no plaintext gRPC listener.
	grpcTLS mtls.Files // SPILLWAY_CP_GRPC_TLS_CERT, SPILLWAY_CP_GRPC_TLS_KEY: the server certificate and key; SPILLWAY_CP_GRPC_CLIENT_CA: the stream CA that gateway certificates must chain to

	// Bearer tokens (CP-11, ADR-0040)
	oidcIssuer   string // SPILLWAY_CP_OIDC_ISSUER: the browser-facing issuer every token's iss must equal
	oidcJWKSURL  string // SPILLWAY_CP_OIDC_JWKS_URL: where the issuer's keys are fetched (in-network in dev, ADR-0004)
	oidcAudience string // SPILLWAY_CP_OIDC_AUDIENCE: a value every token's aud must contain, default spillway-controlplane
	devMode      bool   // SPILLWAY_CP_DEV_MODE: accept http:// issuer and JWKS URLs (SEC-04), default false
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
	s.grpcTLS = mtls.Files{Cert: getenv("SPILLWAY_CP_GRPC_TLS_CERT"), Key: getenv("SPILLWAY_CP_GRPC_TLS_KEY"), CA: getenv("SPILLWAY_CP_GRPC_CLIENT_CA")}
	if s.grpcTLS.Cert == "" || s.grpcTLS.Key == "" || s.grpcTLS.CA == "" {
		return s, errors.New("SPILLWAY_CP_GRPC_TLS_CERT, SPILLWAY_CP_GRPC_TLS_KEY and SPILLWAY_CP_GRPC_CLIENT_CA are required: the config stream is mTLS only (SEC-02)")
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
	s.oidcIssuer, s.oidcJWKSURL = getenv("SPILLWAY_CP_OIDC_ISSUER"), getenv("SPILLWAY_CP_OIDC_JWKS_URL")
	s.oidcAudience = cmp.Or(getenv("SPILLWAY_CP_OIDC_AUDIENCE"), "spillway-controlplane")
	for name, u := range map[string]string{"SPILLWAY_CP_OIDC_ISSUER": s.oidcIssuer, "SPILLWAY_CP_OIDC_JWKS_URL": s.oidcJWKSURL} {
		if err := auth.CheckURL(u, s.devMode); err != nil {
			return s, fmt.Errorf("%s: %w", name, err)
		}
	}
	return s, nil
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
