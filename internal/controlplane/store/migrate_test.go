package store

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestMigrationsExpandOnly checks the migration rules of CP-19 and
// ADR-0036 on every file: forward-only (no Down section), numbered in order
// from 1, and expand-only unless the file says which earlier release it
// contracts for: a statement that drops, renames or narrows what the
// previous release's binary may still use needs a "-- contract: <why the
// previous release no longer uses it>" line.
func TestMigrationsExpandOnly(t *testing.T) {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("migrations: %v, %v", names, err)
	}
	contracting := regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|CONSTRAINT|INDEX|TYPE|VIEW|FUNCTION|TRIGGER)|RENAME\b|ALTER\s+COLUMN\s+\S+\s+(SET\s+NOT\s+NULL|TYPE)|TRUNCATE\b|DELETE\s+FROM|REVOKE\b)`)
	for i, name := range names {
		b, err := fs.ReadFile(migrations, name)
		if err != nil {
			t.Fatal(err)
		}
		sql := string(b)
		base := strings.TrimPrefix(name, "migrations/")
		if want := i + 1; !strings.HasPrefix(base, strconv.Itoa(100000 + want)[1:]+"_") {
			t.Errorf("%s: want migration number %05d", base, want)
		}
		if strings.Contains(sql, "+goose Down") {
			t.Errorf("%s: has a Down section; migrations are forward-only (ADR-0036)", base)
		}
		if !strings.Contains(sql, "-- +goose Up") {
			t.Errorf("%s: no Up section", base)
		}
		var code []string
		for line := range strings.SplitSeq(sql, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				code = append(code, line)
			}
		}
		if m := contracting.FindString(strings.Join(code, "\n")); m != "" && !strings.Contains(sql, "-- contract:") {
			t.Errorf("%s: %q contracts the schema without a -- contract: line naming why the previous release no longer needs it (CP-19)", base, m)
		}
	}
	if SchemaVersion() != int64(len(names)) {
		t.Errorf("SchemaVersion() = %d with %d migrations", SchemaVersion(), len(names))
	}
}

// TestScramVerifier checks the verifier against RFC 7677's construction, so
// Postgres accepts the password it was made from (ADR-0040).
func TestScramVerifier(t *testing.T) {
	const password = "dev-only-not-secret"
	v, err := scramVerifier(password)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^SCRAM-SHA-256\$4096:([A-Za-z0-9+/=]+)\$([A-Za-z0-9+/=]+):([A-Za-z0-9+/=]+)$`).FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("verifier %q is not in Postgres's form", v)
	}
	if strings.Contains(v, password) {
		t.Fatal("the verifier contains the password")
	}
	salt, _ := base64.StdEncoding.DecodeString(m[1])
	if len(salt) != 16 {
		t.Errorf("salt of %d bytes, want 16", len(salt))
	}
	salted, _ := pbkdf2.Key(sha256.New, password, salt, 4096, sha256.Size)
	h := hmac.New(sha256.New, salted)
	h.Write([]byte("Server Key"))
	if got := base64.StdEncoding.EncodeToString(h.Sum(nil)); got != m[3] {
		t.Errorf("server key %s, want %s", m[3], got)
	}
	if v2, _ := scramVerifier(password); v2 == v {
		t.Error("two verifiers share a salt")
	}
}

func TestPoolConfigRejectsPasswordInURL(t *testing.T) {
	_, err := poolConfig("postgres://u:hunter2-dev-only@db/x", "p")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error = %v: want a refusal that does not show the password", err)
	}
	if _, err := poolConfig("mysql://u@db/x", "p"); err == nil {
		t.Error("a mysql URL was accepted")
	}
	cfg, err := poolConfig("postgres://u@db:5432/x?sslmode=disable", "p")
	if err != nil || cfg.ConnConfig.Password != "p" {
		t.Errorf("config = %v, %v", cfg, err)
	}
}
