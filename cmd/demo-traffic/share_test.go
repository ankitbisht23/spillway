package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/secret"
)

const (
	testKCPassword    = "kc-admin-dev-only-not-secret-x"   //nolint:gosec // G101: test values the tests look for in output
	testAdminPassword = "demo-admin-dev-only-not-secret-y" //nolint:gosec // G101: as above
)

// TestStandInTunnel: the stand-in prints cloudflared's quick-tunnel lines
// for a URL under .example, as share.sh parses them, and waits until
// stopped; any other URL is refused (ADR-0097 §4).
func TestStandInTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var out, errOut bytes.Buffer
	done := make(chan int)
	go func() {
		done <- run(ctx, []string{"stand-in-tunnel", "--url", "https://stand-in.spillway.example"}, env(nil), &out, &errOut)
	}()
	select {
	case code := <-done:
		t.Fatalf("returned %d before it was stopped", code)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	visit := -1
	for i, l := range lines {
		if strings.Contains(l, "Your quick Tunnel has been created! Visit it at") {
			visit = i
		}
	}
	if visit < 0 || visit+1 >= len(lines) || !strings.Contains(lines[visit+1], "|  https://stand-in.spillway.example ") {
		t.Errorf("output:\n%s", out.String())
	}
	for _, u := range []string{"https://abc.trycloudflare.com", "https://example.com", "http://stand-in.spillway.example", "https://x.example/path"} {
		errOut.Reset()
		if code := run(t.Context(), []string{"stand-in-tunnel", "--url", u}, env(nil), &out, &errOut); code != 2 {
			t.Errorf("%s: exit %d, want 2", u, code)
		}
	}
}

// TestShareConfigSettings: share-config needs the share URL, the Keycloak
// admin password and a non-dev DEMO_ADMIN_PASSWORD of 16 characters or
// more, and both GitHub settings or neither; each refusal names the
// setting and never prints a value.
func TestShareConfigSettings(t *testing.T) {
	good := map[string]string{"KEYCLOAK_ADMIN_PASSWORD": testKCPassword, "DEMO_ADMIN_PASSWORD": testAdminPassword}
	with := func(kv map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	for _, c := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{[]string{"share-config"}, good, "want https://<host> with no path"},
		{[]string{"share-config", "--url", "http://x.example"}, good, "want https://<host> with no path"},
		{[]string{"share-config", "--url", "https://x.example"}, with(map[string]string{"KEYCLOAK_ADMIN_PASSWORD": ""}), "KEYCLOAK_ADMIN_PASSWORD (or KEYCLOAK_ADMIN_PASSWORD_FILE) is required"},
		{[]string{"share-config", "--url", "https://x.example"}, with(map[string]string{"DEMO_ADMIN_PASSWORD": ""}), "DEMO_ADMIN_PASSWORD is not set"},
		{[]string{"share-config", "--url", "https://x.example"}, with(map[string]string{"DEMO_ADMIN_PASSWORD": "short-one"}), "DEMO_ADMIN_PASSWORD is shorter than 16"},
		{[]string{"share-config", "--url", "https://x.example"}, with(map[string]string{"DEMO_ADMIN_PASSWORD": devValue}), "DEMO_ADMIN_PASSWORD is the public dev value"},
		{[]string{"share-config", "--url", "https://x.example"}, with(map[string]string{"GITHUB_OAUTH_CLIENT_ID": "id"}), "GITHUB_OAUTH_CLIENT_ID and GITHUB_OAUTH_CLIENT_SECRET together"},
		{[]string{"share-config", "--url", "https://x.example"}, with(map[string]string{"GITHUB_OAUTH_CLIENT_SECRET": "gh-dev-only-not-secret"}), "together, or neither"}, //nolint:gosec // G101: a test value
		{[]string{"share-check", "--url", "https://x.example"}, map[string]string{"KEYCLOAK_ADMIN_PASSWORD": testKCPassword}, "DEMO_ADMIN_PASSWORD (or DEMO_ADMIN_PASSWORD_FILE) is required"},
		{[]string{"share-check", "--url", "https://x.example", "--only", "s1"}, good, "only s9b"},
		{[]string{"share-check", "--url", "https://x.example", "--after-stop"}, nil, "SPILLWAY_DEMO_CLIENT_SECRET (or SPILLWAY_DEMO_CLIENT_SECRET_FILE) is required"},
	} {
		var out, errOut bytes.Buffer
		code := run(t.Context(), c.args, env(c.env), &out, &errOut)
		if code != 2 || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%v: exit %d %q, want 2 and %q", c.args, code, errOut.String(), c.want)
		}
		for _, bad := range []string{testKCPassword, testAdminPassword, "short-one", "gh-dev-only-not-secret"} {
			if strings.Contains(out.String()+errOut.String(), bad) {
				t.Errorf("%v printed %q", c.args, bad)
			}
		}
	}
}

// TestShareConfigWritesSecrets: share-config against a fake Keycloak
// writes each rotated client secret to a 0600 file in --secrets-dir and
// prints none of them, nor any password or token.
func TestShareConfigWritesSecrets(t *testing.T) {
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/auth/admin/realms/spillway")
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch {
		case r.URL.Path == "/auth/realms/master/protocol/openid-connect/token":
			_ = r.ParseForm()
			if r.PostForm.Get("password") != testKCPassword {
				w.WriteHeader(401)
				return
			}
			write(map[string]string{"access_token": "admin.tok.en"}) //nolint:gosec // G101: a test value
		case r.Method == "GET" && p == "/clients":
			id := r.URL.Query().Get("clientId")
			write([]map[string]any{{"id": "uuid-" + id, "clientId": id, "enabled": true}})
		case r.Method == "GET" && p == "/users":
			u := r.URL.Query().Get("username")
			write([]map[string]any{{"id": "uuid-" + u, "username": u, "enabled": true}})
		case r.Method == "POST" && strings.HasSuffix(p, "/client-secret"):
			write(map[string]string{"value": "rotated-" + strings.TrimSuffix(strings.TrimPrefix(p, "/clients/uuid-"), "/client-secret")})
		default:
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	kv := map[string]string{"KEYCLOAK_ADMIN_PASSWORD": testKCPassword, "DEMO_ADMIN_PASSWORD": testAdminPassword, "SPILLWAY_DEMO_KEYCLOAK_URL": srv.URL + "/auth"}
	if code := run(t.Context(), []string{"share-config", "--url", "https://stand-in.spillway.example", "--secrets-dir", dir}, env(kv), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, c := range []string{"spillway-ci", "gateway-tests"} {
		name := filepath.Join(dir, c+"-secret")
		st, err := os.Stat(name)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, st, err)
		}
		v, err := readSecret(dir, c)
		if err != nil || v.Reveal() != "rotated-"+c {
			t.Errorf("%s: the file does not hold the new secret (%v)", c, err)
		}
	}
	for _, bad := range []string{testKCPassword, testAdminPassword, "rotated-", "admin.tok.en"} {
		if strings.Contains(out.String()+errOut.String(), bad) {
			t.Errorf("the output has %q", bad)
		}
	}
	if _, err := readSecret(dir, "rbac-tests"); err == nil {
		t.Error("rbac-tests got a secret file")
	}
	if err := writeSecret(filepath.Join(dir, "none"), "x", secret.New("v")); err == nil {
		t.Error("writing into a missing directory: no error")
	}
}
