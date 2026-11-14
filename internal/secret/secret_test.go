package secret

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const value = "s3cret-dev-only-not-secret" //nolint:gosec // G101: a fake value the tests look for

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// TestValueNeverPrinted checks every way a Value could reach a log line, an
// error or a settings dump (ADR-0040): fmt verbs, a struct holding it in an
// exported or an unexported field, slog's JSON and text handlers, JSON.
func TestValueNeverPrinted(t *testing.T) {
	s := New(value)
	if s.Reveal() != value || !s.IsSet() {
		t.Fatalf("Reveal = %q", s.Reveal())
	}
	type exported struct{ Password Value }
	type unexported struct {
		name     string
		password Value
	}
	var out []string
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		out = append(out, fmt.Sprintf(f, s), fmt.Sprintf(f, &s), fmt.Sprintf(f, exported{s}), fmt.Sprintf(f, unexported{"db", s}), fmt.Sprintf(f, &unexported{"db", s}))
	}
	out = append(out, fmt.Sprint(s), fmt.Sprintln(s), fmt.Errorf("wrapped: %v", s).Error())
	b, err := json.Marshal(exported{s})
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, string(b))
	var logs bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&logs, nil), slog.NewTextHandler(&logs, nil)} {
		log := slog.New(h)
		log.Info("settings", "password", s, "settings", exported{s}, "group", slog.GroupValue(slog.Any("p", s)))
		log.Info("settings", slog.Any("unexported", unexported{"db", s}))
	}
	out = append(out, logs.String())
	for _, o := range out {
		if strings.Contains(o, value) {
			t.Errorf("the secret shows: %s", o)
		}
	}
	if !strings.Contains(logs.String(), `"password":"[redacted]"`) || !strings.Contains(string(b), `"[redacted]"`) {
		t.Errorf("want [redacted]: %s %s", logs.String(), b)
	}
	if New("").IsSet() || (Value{}).Reveal() != "" {
		t.Error("an empty value is set")
	}
}

// TestFromEnv checks the X / X_FILE convention (SEC-09, ADR-0040).
func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	file := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	withNL, withCRLF, raw := file("nl", value+"\n"), file("crlf", value+"\r\n"), file("raw", value)
	empty, newline := file("empty", ""), file("newline", "\n")

	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"env", map[string]string{"X": value}, value},
		{"file with a newline", map[string]string{"X_FILE": withNL}, value},
		{"file with CRLF", map[string]string{"X_FILE": withCRLF}, value},
		{"file without a newline", map[string]string{"X_FILE": raw}, value},
		{"neither", map[string]string{}, ""},
	} {
		got, err := FromEnv(env(tt.env), "X")
		if err != nil || got.Reveal() != tt.want {
			t.Errorf("%s: %q, %v", tt.name, got.Reveal(), err)
		}
	}
	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"both", map[string]string{"X": value, "X_FILE": withNL}, "set X or X_FILE, not both"},
		{"missing file", map[string]string{"X_FILE": filepath.Join(dir, "missing")}, "X_FILE: cannot read the file " + filepath.Join(dir, "missing") + ": no such file or directory"},
		{"empty file", map[string]string{"X_FILE": empty}, "X_FILE: the file " + empty + " is empty"},
		{"a newline only", map[string]string{"X_FILE": newline}, "is empty"},
		{"a directory", map[string]string{"X_FILE": dir}, "X_FILE: cannot read the file"},
	} {
		_, err := FromEnv(env(tt.env), "X")
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), value) {
			t.Errorf("%s: the error shows the secret: %v", tt.name, err)
		}
	}
	if _, err := Required(env(nil), "X"); err == nil || err.Error() != "X (or X_FILE) is required" {
		t.Errorf("Required, unset: %v", err)
	}
	if s, err := Required(env(map[string]string{"X_FILE": withNL}), "X"); err != nil || s.Reveal() != value {
		t.Errorf("Required from a file: %q, %v", s.Reveal(), err)
	}
	if _, err := Required(env(map[string]string{"X_FILE": empty}), "X"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("Required, empty file: %v", err)
	}
}
