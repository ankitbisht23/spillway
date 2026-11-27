package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"-h"}, env(nil), &out, &errOut); code != 0 || !strings.Contains(out.String(), "seed") {
		t.Errorf("-h: %d %q", code, out.String())
	}
	for _, args := range [][]string{nil, {"nope"}, {"seed", "extra"}, {"proxies", "-bad"}} {
		if code := run(t.Context(), args, env(nil), &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// TestSeedSettings: the seed needs its client secret and readable files,
// and says so without printing a value.
func TestSeedSettings(t *testing.T) {
	dir := t.TempDir()
	cfg, seed := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "seed.yaml")
	_ = os.WriteFile(cfg, []byte("routes: []\n"), 0o600)
	_ = os.WriteFile(seed, []byte("tenants: [{name: a, plan: p}]\n"), 0o600)
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"seed", "-config", cfg, "-seed", seed}, env(nil), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "SPILLWAY_DEMO_CLIENT_SECRET") {
		t.Errorf("no secret: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run(t.Context(), []string{"seed", "-config", filepath.Join(dir, "none"), "-seed", seed}, env(nil), &out, &errOut); code != 2 {
		t.Errorf("no config: %d %q", code, errOut.String())
	}
	_ = os.WriteFile(seed, []byte("tenants: [{name: a}]\n"), 0o600)
	withSecret := env(map[string]string{"SPILLWAY_DEMO_CLIENT_SECRET": "dev-only-x"}) //nolint:gosec // G101: a test value
	if code := run(t.Context(), []string{"seed", "-config", cfg, "-seed", seed}, withSecret, &out, &errOut); code != 2 {
		t.Errorf("bad seed file: %d", code)
	}
}

// TestProxiesDelete: -delete removes the demo-* proxies only, the chaos
// test's redis-N proxies stay.
func TestProxiesDelete(t *testing.T) {
	var mu sync.Mutex
	proxies := map[string]any{"demo-redis-0": map[string]any{}, "demo-upstream-1": map[string]any{}, "redis-0": map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "GET":
			_ = json.NewEncoder(w).Encode(proxies)
		case "DELETE":
			delete(proxies, strings.TrimPrefix(r.URL.Path, "/proxies/"))
			w.WriteHeader(204)
		case "POST":
			w.WriteHeader(201)
		}
	}))
	t.Cleanup(srv.Close)
	var out, errOut bytes.Buffer
	getenv := env(map[string]string{"SPILLWAY_DEMO_TOXIPROXY_URL": srv.URL})
	if code := run(t.Context(), []string{"proxies", "-delete"}, getenv, &out, &errOut); code != 0 || !strings.Contains(out.String(), "deleted 2 demo proxies") {
		t.Fatalf("exit %d: %s %s", code, out.String(), errOut.String())
	}
	if _, ok := proxies["redis-0"]; !ok || len(proxies) != 1 {
		t.Errorf("left %v", proxies)
	}
	out.Reset()
	if code := run(t.Context(), []string{"proxies"}, getenv, &out, &errOut); code != 0 || strings.Count(out.String(), "\n") != 9 {
		t.Errorf("create: exit %d: %s %s", code, out.String(), errOut.String())
	}
}
