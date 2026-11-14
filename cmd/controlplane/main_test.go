package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ankitbisht23/spillway/internal/testcert"
)

const secretValue = "s3cret-dev-only-not-secret" //nolint:gosec // G101: a fake password the test looks for in logs

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// withOIDC adds the compose stack's bearer-token settings to kv, and the
// stream's mTLS files, which need not exist for loadSettings.
func withOIDC(kv map[string]string) map[string]string {
	kv["SPILLWAY_CP_OIDC_ISSUER"] = "http://localhost:8180/auth/realms/spillway"
	kv["SPILLWAY_CP_OIDC_JWKS_URL"] = "http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/certs"
	kv["SPILLWAY_CP_DEV_MODE"] = "true"
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
	if s.httpAddr != ":8080" || s.adminAddr != "127.0.0.1:8081" || s.databasePassword.Reveal() != secretValue || len(s.allow) != 1 ||
		s.grpcTLS.Cert != "/etc/spillway-stream/cert.pem" || s.grpcTLS.Key != "/etc/spillway-stream/key.pem" || s.grpcTLS.CA != "/etc/spillway-stream/ca.pem" {
		t.Errorf("settings = %+v", s)
	}
	if s.oidcAudience != "spillway-controlplane" || !s.devMode || s.oidcIssuer != "http://localhost:8180/auth/realms/spillway" {
		t.Errorf("OIDC settings = %+v", s)
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
		{"no allow-list", with("SPILLWAY_CP_ALLOWED_NETWORKS", ""), "SPILLWAY_CP_ALLOWED_NETWORKS is required"},
		{"no stream certificate", with("SPILLWAY_CP_GRPC_TLS_CERT", ""), "the config stream is mTLS only (SEC-02)"},
		{"no stream key", with("SPILLWAY_CP_GRPC_TLS_KEY", ""), "the config stream is mTLS only (SEC-02)"},
		{"no stream CA", with("SPILLWAY_CP_GRPC_CLIENT_CA", ""), "the config stream is mTLS only (SEC-02)"},
		{"bad allow-list", with("SPILLWAY_CP_ALLOWED_NETWORKS", "nope"), "SPILLWAY_CP_ALLOWED_NETWORKS"},
		{"no issuer", with("SPILLWAY_CP_OIDC_ISSUER", ""), "SPILLWAY_CP_OIDC_ISSUER: required"},
		{"no JWKS URL", with("SPILLWAY_CP_OIDC_JWKS_URL", ""), "SPILLWAY_CP_OIDC_JWKS_URL: required"},
		{"http:// without dev mode", with("SPILLWAY_CP_DEV_MODE", ""), "accepted only in dev mode (SEC-04)"},
		{"dev mode not a bool", with("SPILLWAY_CP_DEV_MODE", "yes"), `SPILLWAY_CP_DEV_MODE: "yes" is not true or false`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadSettings(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), secretValue) {
				t.Errorf("the error shows the password: %v", err)
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
		"SPILLWAY_CP_DATABASE_URL":           "postgres://spillway_app@db:5432/spillway",
		"SPILLWAY_CP_DATABASE_PASSWORD_FILE": file("app", secretValue),
		"SPILLWAY_CP_ALLOWED_NETWORKS":       "172.29.0.0/16",
	})))
	if err != nil || s.databasePassword.Reveal() != secretValue {
		t.Fatalf("serve settings: %q, %v", s.databasePassword.Reveal(), err)
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
	for _, v := range []string{secretValue, ownerSecret} {
		if strings.Contains(dump, v) {
			t.Errorf("a settings dump shows a password:\n%s", dump)
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
	for _, s := range []string{secretValue, ownerSecret} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("a log line or error shows a password:\n%s", logs.String())
		}
	}
}
