package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestKindseed: kindseed waits while the control plane answers 503, then
// imports and publishes the file; a second run with nothing new to publish
// is "already current"; neither the client secret nor the token is printed
// (ADR-0086).
func TestKindseed(t *testing.T) {
	f := newFakeCP(t)
	f.mu.Lock()
	f.down = true
	f.mu.Unlock()
	go func() {
		time.Sleep(2500 * time.Millisecond)
		f.mu.Lock()
		f.down = false
		f.mu.Unlock()
	}()
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("routes: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == ciSecretEnv {
			return testSecret
		}
		return ""
	}
	args := []string{"-config", file, "-controlplane", f.srv.URL + "/api/v1", "-token-url", f.srv.URL + "/token", "-wait", "20s"}
	var out, errOut bytes.Buffer
	if code := runKindseed(t.Context(), args, getenv, &out, &errOut); code != 0 || !strings.Contains(out.String(), "PASS  config published") || !strings.Contains(out.String(), "version 101") {
		t.Fatalf("exit %d:\n%s\n%s", code, out.String(), errOut.String())
	}
	if string(f.imported) != "routes: []\n" || !strings.Contains(errOut.String(), "not yet") {
		t.Errorf("imported %q; stderr %q", f.imported, errOut.String())
	}
	f.nothing = true
	out.Reset()
	if code := runKindseed(t.Context(), args, getenv, &out, &errOut); code != 0 || !strings.Contains(out.String(), "version 101") {
		t.Errorf("second run: exit %d:\n%s", code, out.String())
	}
	if all := out.String() + errOut.String(); strings.Contains(all, testSecret) || strings.Contains(all, testToken) {
		t.Errorf("a secret or token was printed:\n%s", all)
	}

	// The wrong secret: FAIL once -wait has passed, without the secret.
	out.Reset()
	errOut.Reset()
	bad := func(k string) string {
		if k == ciSecretEnv {
			return "wrong-dev-only-value"
		}
		return ""
	}
	if code := runKindseed(t.Context(), append(slices.Clone(args[:len(args)-1]), "1s"), bad, &out, &errOut); code != 1 || !strings.Contains(out.String(), "FAIL  config published") {
		t.Errorf("wrong secret: exit %d:\n%s", code, out.String())
	}
	if strings.Contains(out.String()+errOut.String(), "wrong-dev-only-value") {
		t.Error("the secret was printed")
	}
	for _, a := range [][]string{nil, {"-config", filepath.Join(t.TempDir(), "missing.yaml")}, {"-config", file, "extra"}, {"-config", file, "-ca", file}} {
		if code := runKindseed(t.Context(), a, getenv, &out, &errOut); code != 2 {
			t.Errorf("%q: exit %d, want 2", a, code)
		}
	}
}
