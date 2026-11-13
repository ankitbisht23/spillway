package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretValue = "s3cret-dev-only-not-secret" //nolint:gosec // G101: a fake password the test looks for in logs

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestLoadSettings(t *testing.T) {
	s, err := loadSettings(env(map[string]string{
		"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@db:5432/spillway?sslmode=disable",
		"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
		"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.httpAddr != ":8080" || s.adminAddr != "127.0.0.1:8081" || s.databasePassword != secretValue || len(s.allow) != 1 {
		t.Errorf("settings = %+v", s)
	}
}

func TestSettingsErrors(t *testing.T) {
	base := map[string]string{
		"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@db:5432/spillway",
		"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
		"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
	}
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
		{"bad allow-list", with("SPILLWAY_CP_ALLOWED_NETWORKS", "nope"), "SPILLWAY_CP_ALLOWED_NETWORKS"},
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

// TestSecretFromFile: a secret can come from a file named by X_FILE, with
// one trailing newline trimmed (SEC-09, ADR-0040).
func TestSecretFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := secret(env(map[string]string{"X_FILE": path}), "X")
	if err != nil || got != secretValue {
		t.Errorf("secret = %q, %v", got, err)
	}
	if _, err := secret(env(map[string]string{"X_FILE": filepath.Join(t.TempDir(), "missing")}), "X"); err == nil || !strings.Contains(err.Error(), "X_FILE: cannot read the file") {
		t.Errorf("missing file: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secret(env(map[string]string{"X_FILE": empty}), "X"); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Errorf("empty file: %v", err)
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
			return serve(log, env(map[string]string{
				"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app@127.0.0.1:1/spillway?sslmode=disable&connect_timeout=2",
				"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
				"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
			}))
		}, "127.0.0.1"},
		{"serve, password in the URL", func() error {
			return serve(log, env(map[string]string{
				"SPILLWAY_CP_DATABASE_URL":      "postgres://spillway_app:" + ownerSecret + "@db/spillway",
				"SPILLWAY_CP_DATABASE_PASSWORD": secretValue,
				"SPILLWAY_CP_ALLOWED_NETWORKS":  "172.29.0.0/16",
			}))
		}, "carries a password"},
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
