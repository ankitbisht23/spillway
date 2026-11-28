package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ankitbisht23/spillway/internal/testcert"
)

const secretValue = "s3cret-dev-only-not-secret" //nolint:gosec // G101: a fake password the test looks for in logs

const hitterSecret = "hh-dev-only-not-secret" //nolint:gosec // G101: a fake heavy-hitter secret the tests look for in logs

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// withOIDC adds the compose stack's bearer-token settings to kv, the
// heavy-hitter secret unless kv has one, and the stream's mTLS files,
// which need not exist for loadSettings.
func withOIDC(kv map[string]string) map[string]string {
	kv["SPILLWAY_CP_OIDC_ISSUER"] = "http://localhost:8180/auth/realms/spillway"
	kv["SPILLWAY_CP_OIDC_JWKS_URL"] = "http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/certs"
	kv["SPILLWAY_CP_DEV_MODE"] = "true"
	if _, ok := kv["SPILLWAY_CP_HEAVY_HITTER_SECRET"]; !ok {
		kv["SPILLWAY_CP_HEAVY_HITTER_SECRET"] = hitterSecret
	}
	for k, v := range map[string]string{"SPILLWAY_CP_GRPC_TLS_CERT": "cert.pem", "SPILLWAY_CP_GRPC_TLS_KEY": "key.pem", "SPILLWAY_CP_GRPC_CLIENT_CA": "ca.pem"} {
		if _, ok := kv[k]; !ok {
			kv[k] = "/etc/spillway-stream/" + v
		}
	}
	return kv
}

// withStreamCerts sets the stream's mTLS files to a server certificate of
// a new test CA.
func withStreamCerts(t *testing.T, kv map[string]string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	testcert.NewCA(t, "stream CA").Issue(t, dir, testcert.Leaf{DNSNames: []string{"controlplane"}})
	kv["SPILLWAY_CP_GRPC_TLS_CERT"] = filepath.Join(dir, "cert.pem")
	kv["SPILLWAY_CP_GRPC_TLS_KEY"] = filepath.Join(dir, "key.pem")
	kv["SPILLWAY_CP_GRPC_CLIENT_CA"] = filepath.Join(dir, "ca.pem")
	return kv
}

func TestLoadSettings(t *testing.T) {
	s, err := loadSettings(env(withOIDC(map[string]string{
		"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@db:5432/spillway?sslmode=disable",
		"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
		"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if s.httpAddr != ":8080" || s.adminAddr != "127.0.0.1:8081" || s.databasePassword.Reveal() != secretValue || s.hitterSecret.Reveal() != hitterSecret || len(s.allow) != 1 ||
		s.grpcTLS.Cert != "/etc/spillway-stream/cert.pem" || s.grpcTLS.Key != "/etc/spillway-stream/key.pem" || s.grpcTLS.CA != "/etc/spillway-stream/ca.pem" {
		t.Errorf("settings = %+v", s)
	}
	if s.oidcAudience != "spillway-controlplane" || !s.devMode || s.oidcIssuer != "http://localhost:8180/auth/realms/spillway" {
		t.Errorf("OIDC settings = %+v", s)
	}
	if s.prometheusURL != nil {
		t.Errorf("Prometheus URL = %v, want none when unset", s.prometheusURL)
	}
}

// TestTrustedProxiesSetting: SPILLWAY_CP_TRUSTED_PROXIES is read in every
// mode; unset it trusts nobody, and a value that is not a CIDR stops the
// control plane (SEC-05, ADR-0097 §2).
func TestTrustedProxiesSetting(t *testing.T) {
	base := func(v string) map[string]string {
		return withOIDC(map[string]string{
			"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@db:5432/spillway",
			"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
			"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
			"SPILLWAY_CP_TRUSTED_PROXIES":   v,
		})
	}
	if s, err := loadSettings(env(base(""))); err != nil || len(s.trustedProxies) != 0 {
		t.Errorf("unset: %v, %v; want no trusted proxy", s.trustedProxies, err)
	}
	if s, err := loadSettings(env(base("172.29.255.10/32, 2001:db8::/64"))); err != nil || fmt.Sprint(s.trustedProxies) != "[172.29.255.10/32 2001:db8::/64]" {
		t.Errorf("two CIDRs: %v, %v", s.trustedProxies, err)
	}
	for _, v := range []string{"172.29.255.10", "caddy", "172.29.255.10/33"} {
		if _, err := loadSettings(env(base(v))); err == nil || !strings.Contains(err.Error(), "SPILLWAY_CP_TRUSTED_PROXIES: trusted proxy") {
			t.Errorf("SPILLWAY_CP_TRUSTED_PROXIES=%q: %v, want an error naming the setting", v, err)
		}
	}
}

// TestDemoModeSetting: SPILLWAY_CP_DEMO_MODE is off unless set to a value
// strconv.ParseBool reads as true (SC-08, ADR-0095 §1); in demo mode
// SPILLWAY_CP_DEMO_TOXIPROXY_URL (SC-03) and SPILLWAY_CP_DEMO_GATEWAYS
// (UI-07) are required and checked, and with demo mode off they are
// ignored.
func TestDemoModeSetting(t *testing.T) {
	const gateways = "http://gateway-1:8000,http://gateway-2:8000,http://gateway-3:8000"
	baseGW := func(mode, toxiproxy, gw string) map[string]string {
		return withOIDC(map[string]string{
			"SPILLWAY_CP_DATABASE_URL":       "postgres://spillway_app@db:5432/spillway",
			"SPILLWAY_CP_DATABASE_PASSWORD":  secretValue,
			"SPILLWAY_CP_ALLOWED_NETWORKS":   "172.29.0.0/16",
			"SPILLWAY_CP_DEMO_MODE":          mode,
			"SPILLWAY_CP_DEMO_TOXIPROXY_URL": toxiproxy,
			"SPILLWAY_CP_DEMO_GATEWAYS":      gw,
		})
	}
	base := func(mode, toxiproxy string) map[string]string { return baseGW(mode, toxiproxy, gateways) }
	if s, err := loadSettings(env(base("true", "http://toxiproxy:8474"))); err != nil ||
		!slices.Equal(s.demoGateways, []string{"http://gateway-1:8000", "http://gateway-2:8000", "http://gateway-3:8000"}) {
		t.Errorf("demo gateways %q, %v", s.demoGateways, err)
	}
	if s, err := loadSettings(env(baseGW("false", "", "not a URL"))); err != nil || s.demoGateways != nil {
		t.Errorf("demo mode off: the gateways are ignored, got %q, %v", s.demoGateways, err)
	}
	for v, msg := range map[string]string{
		"":                          "SPILLWAY_CP_DEMO_GATEWAYS: required in demo mode",
		" , ":                       "SPILLWAY_CP_DEMO_GATEWAYS: required in demo mode",
		"gateway-1:8000":            "want an http or https URL",
		"http://u:p@gateway-1:8000": "user info is not allowed",
		"http://gateway-1:8000/v1":  "a path is not allowed",
		"http://gateway-1:8000,http://gateway-1:8000/": "is listed twice",
	} {
		if _, err := loadSettings(env(baseGW("true", "http://toxiproxy:8474", v))); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("SPILLWAY_CP_DEMO_GATEWAYS=%q: %v, want %q", v, err, msg)
		}
	}
	for v, want := range map[string]bool{"": false, "false": false, "0": false, "true": true, "1": true, "TRUE": true} {
		s, err := loadSettings(env(base(v, "http://toxiproxy:8474")))
		if err != nil || s.demoMode != want || want != (s.demoToxiproxyURL == "http://toxiproxy:8474") {
			t.Errorf("SPILLWAY_CP_DEMO_MODE=%q: demo mode %v, toxiproxy %q, %v; want %v", v, s.demoMode, s.demoToxiproxyURL, err, want)
		}
	}
	if s, err := loadSettings(env(base("false", "not a URL"))); err != nil || s.demoToxiproxyURL != "" {
		t.Errorf("demo mode off: a toxiproxy URL is ignored, got %q, %v", s.demoToxiproxyURL, err)
	}
	for v, msg := range map[string]string{
		"":                               "SPILLWAY_CP_DEMO_TOXIPROXY_URL is required in demo mode",
		"toxiproxy:8474":                 "SPILLWAY_CP_DEMO_TOXIPROXY_URL: want an http or https URL",
		"http://u:p@toxiproxy:8474":      "SPILLWAY_CP_DEMO_TOXIPROXY_URL: user info is not allowed",
		"http://toxiproxy:8474/?debug=1": "SPILLWAY_CP_DEMO_TOXIPROXY_URL: a query or fragment is not allowed",
	} {
		if _, err := loadSettings(env(base("true", v))); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("SPILLWAY_CP_DEMO_TOXIPROXY_URL=%q: %v, want %q", v, err, msg)
		}
	}
}

// TestFleetAndOIDCCA: SPILLWAY_CP_GRPC_FLEET_IDENTITIES lists the fleet
// certificates' names, none by default (ADR-0075 §D11);
// SPILLWAY_CP_OIDC_CA names the CA file the JWKS URL is trusted with, none
// by default (SEC-04).
func TestFleetAndOIDCCA(t *testing.T) {
	kv := withOIDC(map[string]string{
		"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@db:5432/spillway",
		"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
		"SPILLWAY_CP_ALLOWED_NETWORKS":  "10.244.0.0/16,10.96.0.0/16",
	})
	s, err := loadSettings(env(kv))
	if err != nil || len(s.fleet) != 0 || s.oidcCA != "" {
		t.Fatalf("defaults: fleet %v, OIDC CA %q, %v", s.fleet, s.oidcCA, err)
	}
	kv["SPILLWAY_CP_GRPC_FLEET_IDENTITIES"] = " spillway-gateway, edge ,"
	kv["SPILLWAY_CP_OIDC_CA"] = "/etc/spillway-ca/ca.pem"
	if s, err = loadSettings(env(kv)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.fleet, ",") != "spillway-gateway,edge" || s.oidcCA != "/etc/spillway-ca/ca.pem" {
		t.Errorf("fleet %v, OIDC CA %q", s.fleet, s.oidcCA)
	}
}

// TestRedisSettings: the quota flusher's Redis is optional (unset, the
// flusher is off); its seeds, AUTH and the dev address map (RL-10,
// ADR-0070, SEC-09).
func TestRedisSettings(t *testing.T) {
	kv := withOIDC(map[string]string{
		"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@db:5432/spillway",
		"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
		"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
	})
	s, err := loadSettings(env(kv))
	if err != nil || len(s.redis.Addrs) != 0 {
		t.Fatalf("unset: %+v, %v", s.redis, err)
	}
	kv["SPILLWAY_CP_REDIS_ADDRS"] = "redis-0:6379, redis-1:6379,"
	kv["SPILLWAY_CP_REDIS_USERNAME"] = "flusher"
	kv["SPILLWAY_CP_REDIS_PASSWORD"] = secretValue
	kv["SPILLWAY_REDIS_ADDR_MAP"] = "redis-0:6379=127.0.0.1:7000"
	if s, err = loadSettings(env(kv)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.redis.Addrs, ",") != "redis-0:6379,redis-1:6379" || s.redis.Username != "flusher" || s.redis.Password.Reveal() != secretValue ||
		s.redis.AddrMap["redis-0:6379"] != "127.0.0.1:7000" {
		t.Errorf("redis settings = %+v", s.redis)
	}
	if strings.Contains(fmt.Sprintf("%+v %v", s, s), secretValue) {
		t.Error("a settings dump shows the Redis password")
	}
}

// TestPrometheusURL: SPILLWAY_CP_PROMETHEUS_URL is optional; a value
// that is not an http(s) URL with a host stops the start (ADR-0059).
func TestPrometheusURL(t *testing.T) {
	kv := func(v string) map[string]string {
		return withOIDC(map[string]string{
			"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@db:5432/spillway",
			"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
			"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
			"SPILLWAY_CP_PROMETHEUS_URL":    v,
		})
	}
	s, err := loadSettings(env(kv("http://prometheus:9090")))
	if err != nil || s.prometheusURL == nil || s.prometheusURL.String() != "http://prometheus:9090" {
		t.Fatalf("settings = %v, %v", s.prometheusURL, err)
	}
	for _, bad := range []string{"prometheus:9090", "ftp://prometheus", "http://", "http://user:pw@prometheus:9090", "http://prometheus:9090/?x=1", "http://prom etheus"} {
		if _, err := loadSettings(env(kv(bad))); err == nil || !strings.Contains(err.Error(), "SPILLWAY_CP_PROMETHEUS_URL") {
			t.Errorf("%q: error = %v", bad, err)
		}
	}
}

func TestSettingsErrors(t *testing.T) {
	base := withOIDC(map[string]string{
		"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@db:5432/spillway",
		"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
		"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
	})
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no URL", with("SPILLWAY_CP_DATABASE_URL", ""), "SPILLWAY_CP_DATABASE_URL is required"},
		{"no password", with("SPILLWAY_CP_DATABASE_PASSWORD", ""), "SPILLWAY_CP_DATABASE_PASSWORD (or SPILLWAY_CP_DATABASE_PASSWORD_FILE) is required"},
		{"password and file", with("SPILLWAY_CP_DATABASE_PASSWORD_FILE", "/nope"), "not both"},
		{"no heavy-hitter secret", with("SPILLWAY_CP_HEAVY_HITTER_SECRET", ""), "SPILLWAY_CP_HEAVY_HITTER_SECRET (or SPILLWAY_CP_HEAVY_HITTER_SECRET_FILE) is required"},
		{"heavy-hitter secret and file", with("SPILLWAY_CP_HEAVY_HITTER_SECRET_FILE", "/nope"), "not both"},
		{"no allow-list", with("SPILLWAY_CP_ALLOWED_NETWORKS", ""), "SPILLWAY_CP_ALLOWED_NETWORKS is required"},
		{"no stream certificate", with("SPILLWAY_CP_GRPC_TLS_CERT", ""), "the config stream is mTLS only (SEC-02)"},
		{"no stream key", with("SPILLWAY_CP_GRPC_TLS_KEY", ""), "the config stream is mTLS only (SEC-02)"},
		{"no stream CA", with("SPILLWAY_CP_GRPC_CLIENT_CA", ""), "the config stream is mTLS only (SEC-02)"},
		{"bad allow-list", with("SPILLWAY_CP_ALLOWED_NETWORKS", "nope"), "SPILLWAY_CP_ALLOWED_NETWORKS"},
		{"no issuer", with("SPILLWAY_CP_OIDC_ISSUER", ""), "SPILLWAY_CP_OIDC_ISSUER: required"},
		{"no JWKS URL", with("SPILLWAY_CP_OIDC_JWKS_URL", ""), "SPILLWAY_CP_OIDC_JWKS_URL: required"},
		{"http:// without dev mode", with("SPILLWAY_CP_DEV_MODE", ""), "accepted only in dev mode (SEC-04)"},
		{"dev mode not a bool", with("SPILLWAY_CP_DEV_MODE", "yes"), `SPILLWAY_CP_DEV_MODE: "yes" is not true or false`},
		{"demo mode not a bool", with("SPILLWAY_CP_DEMO_MODE", "yes"), "SPILLWAY_CP_DEMO_MODE: want true or false"},
		{"fleet name not a DNS label", with("SPILLWAY_CP_GRPC_FLEET_IDENTITIES", "spillway-gateway,Fleet.1"), `SPILLWAY_CP_GRPC_FLEET_IDENTITIES: "Fleet.1" is not a DNS label`},
		{"bad Redis address", with("SPILLWAY_CP_REDIS_ADDRS", "redis-0"), `SPILLWAY_CP_REDIS_ADDRS: "redis-0" is not host:port`},
		{"Redis username without a password", with("SPILLWAY_CP_REDIS_USERNAME", "flusher"), "SPILLWAY_CP_REDIS_USERNAME: set with SPILLWAY_CP_REDIS_PASSWORD"},
		{"Redis password and file", func() map[string]string {
			m := with("SPILLWAY_CP_REDIS_PASSWORD_FILE", "/nope")
			m["SPILLWAY_CP_REDIS_PASSWORD"] = secretValue
			return m
		}(), "not both"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadSettings(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if err != nil && (strings.Contains(err.Error(), secretValue) || strings.Contains(err.Error(), hitterSecret)) {
				t.Errorf("the error shows a secret: %v", err)
			}
		})
	}
}

// TestSecretsFromFiles: each password can come from a file named by
// X_FILE (SEC-09, ADR-0040), and no settings dump, error or log line
// shows it.
func TestSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	file := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ownerSecret := "owner-" + secretValue
	s, err := loadSettings(env(withOIDC(map[string]string{
		"SPILLWAY_CP_DATABASE_URL":             "postgres://spillway_app@db:5432/spillway",
		"SPILLWAY_CP_DATABASE_PASSWORD_FILE":   file("app", secretValue),
		"SPILLWAY_CP_HEAVY_HITTER_SECRET":      "",
		"SPILLWAY_CP_HEAVY_HITTER_SECRET_FILE": file("hh", hitterSecret),
		"SPILLWAY_CP_ALLOWED_NETWORKS":         "172.29.0.0/16",
	})))
	if err != nil || s.databasePassword.Reveal() != secretValue || s.hitterSecret.Reveal() != hitterSecret {
		t.Fatalf("serve settings: %v", err)
	}
	m, err := loadMigrateSettings(env(map[string]string{
		"SPILLWAY_CP_MIGRATE_DATABASE_URL":   "postgres://spillway@db:5432/spillway",
		"SPILLWAY_CP_MIGRATE_PASSWORD_FILE":  file("owner", ownerSecret),
		"SPILLWAY_CP_DATABASE_PASSWORD_FILE": file("app", secretValue),
	}))
	if err != nil || m.password.Reveal() != ownerSecret || m.databasePassword.Reveal() != secretValue {
		t.Fatalf("migrate settings: %v", err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	log.Info("settings", "serve", s, "migrate", m)
	dump := strings.Join([]string{fmt.Sprintf("%v %+v %#v", s, s, s), fmt.Sprintf("%v %+v %#v", m, m, m), logs.String()}, "\n")
	for _, v := range []string{secretValue, ownerSecret, hitterSecret} {
		if strings.Contains(dump, v) {
			t.Errorf("a settings dump shows a secret:\n%s", dump)
		}
	}
	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing file", map[string]string{"SPILLWAY_CP_MIGRATE_PASSWORD_FILE": filepath.Join(dir, "missing")}, "SPILLWAY_CP_MIGRATE_PASSWORD_FILE: cannot read the file"},
		{"empty file", map[string]string{"SPILLWAY_CP_MIGRATE_PASSWORD_FILE": file("empty", "")}, "SPILLWAY_CP_MIGRATE_PASSWORD_FILE: the file"},
		{"both", map[string]string{"SPILLWAY_CP_MIGRATE_PASSWORD_FILE": file("owner", ownerSecret), "SPILLWAY_CP_MIGRATE_PASSWORD": ownerSecret}, "not both"},
	} {
		tt.env["SPILLWAY_CP_MIGRATE_DATABASE_URL"] = "postgres://spillway@db:5432/spillway"
		tt.env["SPILLWAY_CP_DATABASE_PASSWORD"] = secretValue
		_, err := loadMigrateSettings(env(tt.env))
		if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), secretValue) || strings.Contains(err.Error(), ownerSecret) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
}

// TestPasswordsNeverLogged runs migrate and serve against a database that
// cannot be reached, and a URL that carries a password, and checks that no
// log line or error shows either password (ADR-0040).
func TestPasswordsNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	ownerSecret := "owner-" + secretValue
	runs := []struct {
		name string
		run  func() error
		want string
	}{
		{"migrate, database down", func() error {
			return migrate(log, env(map[string]string{
				"SPILLWAY_CP_MIGRATE_DATABASE_URL": "postgres://spillway@127.0.0.1:1/spillway?sslmode=disable&connect_timeout=2",
				"SPILLWAY_CP_MIGRATE_PASSWORD":     ownerSecret,
				"SPILLWAY_CP_DATABASE_PASSWORD":    secretValue,
			}))
		}, "127.0.0.1"},
		{"serve, database down", func() error {
			return serve(log, env(withOIDC(withStreamCerts(t, map[string]string{
				"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@127.0.0.1:1/spillway?sslmode=disable&connect_timeout=2",
				"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
				"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
			}))))
		}, "127.0.0.1"},
		{"serve, password in the URL", func() error {
			return serve(log, env(withOIDC(withStreamCerts(t, map[string]string{
				"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app:" + ownerSecret + "@db/spillway",
				"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
				"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
			}))))
		}, "carries a password"},
		{"serve, stream certificate missing", func() error {
			return serve(log, env(withOIDC(map[string]string{
				"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@127.0.0.1:1/spillway",
				"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
				"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
			})))
		}, "config stream mTLS"},
	}
	for _, r := range runs {
		err := r.run()
		if err == nil || !strings.Contains(err.Error(), r.want) {
			t.Errorf("%s: error = %v, want %q", r.name, err, r.want)
			continue
		}
		log.Error("controlplane failed", "err", err)
	}
	for _, s := range []string{secretValue, ownerSecret, hitterSecret} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("a log line or error shows a secret:\n%s", logs.String())
		}
	}
}
