package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/proxy"
)

func TestLoadSettings(t *testing.T) {
	base := map[string]string{
		"SPILLWAY_GATEWAY_CONFIG":           "/etc/spillway/config.yaml",
		"SPILLWAY_GATEWAY_HTTP_ADDR":        ":8000",
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": "172.29.0.0/16",
	}
	env := func(over map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := over[k]; ok {
				return v
			}
			return base[k]
		}
	}

	s, err := loadSettings(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.configPath != "/etc/spillway/config.yaml" || s.httpAddr != ":8000" || s.adminAddr != "127.0.0.1:8001" || len(s.allow) != 1 || s.dnsRefresh != 30*time.Second {
		t.Errorf("settings = %+v", s)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_DNS_REFRESH": "5s"})); err != nil || s.dnsRefresh != 5*time.Second {
		t.Errorf("DNS refresh 5s: %+v, %v", s, err)
	}
	if s, err := loadSettings(env(map[string]string{"SPILLWAY_GATEWAY_ADMIN_ADDR": ":8001"})); err != nil || s.adminAddr != ":8001" {
		t.Errorf("admin address :8001: %+v, %v", s, err)
	}

	for name, over := range map[string]map[string]string{
		"SPILLWAY_GATEWAY_CONFIG":           {"SPILLWAY_GATEWAY_CONFIG": ""},
		"SPILLWAY_GATEWAY_HTTP_ADDR":        {"SPILLWAY_GATEWAY_HTTP_ADDR": ""},
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": {"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": ""},
		"SPILLWAY_GATEWAY_DNS_REFRESH":      {"SPILLWAY_GATEWAY_DNS_REFRESH": "0s"},
	} {
		if _, err := loadSettings(env(over)); err == nil || !strings.HasPrefix(err.Error(), name) {
			t.Errorf("%v: error = %v, want one naming %s", over, err, name)
		}
	}
}

func TestParseMemoryMax(t *testing.T) {
	for in, want := range map[string]int64{
		"536870912\n": 536870912,
		"max\n":       0,
		"":            0,
		"0\n":         0,
		"-1\n":        0,
	} {
		got, ok := parseMemoryMax([]byte(in))
		if ok != (want > 0) || ok && got != want {
			t.Errorf("parseMemoryMax(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
}

// TestServe runs the gateway against a local upstream: traffic is proxied,
// the admin port answers /readyz, /metrics and the healthcheck, and on
// shutdown readiness fails while an in-flight request still completes.
func TestServe(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(arrived)
			<-release
		}
		_, _ = io.WriteString(w, "upstream")
	}))
	t.Cleanup(up.Close)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "version: 7\nupstreams: [{ name: up, endpoints: [{ address: '" + up.Listener.Addr().String() + "' }] }]\n" +
		"routes: [{ name: all, match: { path_prefix: / }, upstream: up }]\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	s := settings{
		configPath: cfg,
		httpAddr:   freeAddr(t),
		adminAddr:  freeAddr(t),
		allow:      proxy.Allowlist{netip.MustParsePrefix("127.0.0.0/8")},
		dnsRefresh: time.Minute,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, slog.New(slog.DiscardHandler)) }()

	traffic, admin := "http://"+s.httpAddr, "http://"+s.adminAddr
	waitFor(t, "/readyz 200", func() bool { code, _ := get(t, admin+"/readyz"); return code == http.StatusOK })
	_, port, _ := net.SplitHostPort(s.adminAddr)
	for _, addr := range []string{s.adminAddr, ":" + port} {
		if err := healthcheck(t.Context(), adminEnv(addr)); err != nil {
			t.Errorf("healthcheck with admin address %s: %v", addr, err)
		}
	}
	if _, body := get(t, admin+"/metrics"); !strings.Contains(body, "\ngw_config_version 7\n") || !strings.Contains(body, "\ngo_goroutines ") {
		t.Errorf("/metrics lacks gw_config_version 7 or go_goroutines:\n%s", body)
	}
	if code, body := get(t, traffic+"/x"); code != http.StatusOK || body != "upstream" {
		t.Errorf("GET /x = %d %q, want 200 from the upstream", code, body)
	}
	for _, p := range []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/"} {
		if _, body := get(t, traffic+p); body != "upstream" {
			t.Errorf("traffic port %s = %q, want it proxied like any path", p, body)
		}
	}

	slow := make(chan int)
	go func() { code, _ := get(t, traffic+"/slow"); slow <- code }()
	<-arrived
	cancel()
	waitFor(t, "/readyz 503", func() bool { code, _ := get(t, admin+"/readyz"); return code == http.StatusServiceUnavailable })
	if err := healthcheck(t.Context(), adminEnv(s.adminAddr)); err == nil {
		t.Error("healthcheck passed while draining")
	}
	close(release)
	if code := <-slow; code != http.StatusOK {
		t.Errorf("in-flight request = %d, want 200", code)
	}
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
	if err := healthcheck(t.Context(), adminEnv(s.adminAddr)); err == nil {
		t.Error("healthcheck passed after shutdown")
	}
}

// adminEnv returns a getenv with only the admin address set.
func adminEnv(adminAddr string) func(string) string {
	return func(k string) string {
		if k == "SPILLWAY_GATEWAY_ADMIN_ADDR" {
			return adminAddr
		}
		return ""
	}
}

func freeAddr(t *testing.T) string {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// get returns the status and body, or 0 if the request failed.
func get(t *testing.T, url string) (int, string) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return 0, ""
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
