package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// fakeDemoCP is a token endpoint and a control plane that answers GET
// /api/v1/demo with demo, and records each setDemoFault.
type fakeDemoCP struct {
	mu    sync.Mutex
	demo  int // GET /demo's status
	fault int // setDemoFault's status
	puts  []string
	auth  []string
}

const fakeToken = "eyJ.fake-token" //nolint:gosec // G101: a test value

func (f *fakeDemoCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/token":
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fakeToken})
	case r.Method == "GET" && r.URL.Path == "/api/v1/demo":
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		w.WriteHeader(f.demo)
		_, _ = w.Write([]byte(`{"error": "no such path", "code": "not_found"}`))
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/v1/demo/faults/"):
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		var body struct {
			Active *bool  `json:"active"`
			Note   string `json:"note"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/demo/faults/")
		if body.Active == nil {
			w.WriteHeader(400)
			return
		}
		f.puts = append(f.puts, fmt.Sprintf("%s %v %s", name, *body.Active, body.Note))
		w.WriteHeader(f.fault)
		if f.fault == 200 {
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "active": *body.Active})
		} else {
			_, _ = w.Write([]byte(`{"error": "forbidden: needs the role admin", "code": "forbidden"}`))
		}
	default:
		w.WriteHeader(404)
	}
}

// TestFault: demo-traffic fault switches one fault, or both off with
// clear, through setDemoFault with the service token and a note, and
// prints each new state; with demo mode off (GET /api/v1/demo 404) it
// stops with the demo-mode-off message before any switch; a refused switch
// fails; bad arguments exit 2. No secret or token is printed (SEC-09).
func TestFault(t *testing.T) {
	f := &fakeDemoCP{demo: 200, fault: 200}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	const sec = "dev-only-fault-secret" //nolint:gosec // G101: a test value
	getenv := env(map[string]string{"SPILLWAY_DEMO_CP_URL": srv.URL, "SPILLWAY_DEMO_TOKEN_URL": srv.URL + "/token", "SPILLWAY_DEMO_CLIENT_SECRET": sec})
	var all bytes.Buffer
	runFault := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(t.Context(), append([]string{"fault"}, args...), getenv, &out, &errOut)
		all.WriteString(out.String() + errOut.String())
		return code, out.String(), errOut.String()
	}
	if code, out, errOut := runFault("redis-cut", "on"); code != 0 || out != "redis-cut: on\n" {
		t.Errorf("redis-cut on: %d %q %q", code, out, errOut)
	}
	if code, out, _ := runFault("-note", "D6 by hand", "upstream-slow", "off"); code != 0 || out != "upstream-slow: off\n" {
		t.Errorf("upstream-slow off: %d %q", code, out)
	}
	if code, out, _ := runFault("clear"); code != 0 || out != "redis-cut: off\nupstream-slow: off\n" {
		t.Errorf("clear: %d %q", code, out)
	}
	want := []string{"redis-cut true demo-traffic fault redis-cut on", "upstream-slow false D6 by hand",
		"redis-cut false demo-traffic fault clear", "upstream-slow false demo-traffic fault clear"}
	if !slices.Equal(f.puts, want) {
		t.Errorf("switches %q, want %q", f.puts, want)
	}
	for _, a := range f.auth {
		if a != "Bearer "+fakeToken {
			t.Errorf("a call without the service token: %q", a)
		}
	}

	f.puts, f.demo = nil, 404
	if code, out, errOut := runFault("redis-cut", "on"); code != 1 || out != "" || errOut != "demo-traffic fault: demo mode is off (GET /api/v1/demo answered 404)\n" || len(f.puts) != 0 {
		t.Errorf("demo mode off: %d %q %q, %d switches", code, out, errOut, len(f.puts))
	}
	f.demo, f.fault = 200, 403
	if code, _, errOut := runFault("upstream-slow", "on"); code != 1 || !strings.Contains(errOut, "status 403, want 200: forbidden: needs the role admin") {
		t.Errorf("refused: %d %q", code, errOut)
	}
	for _, args := range [][]string{nil, {"redis-cut"}, {"redis-cut", "1"}, {"clear", "on"}, {"redis", "on"}, {"redis-cut", "on", "x"}, {"-bad"}} {
		if code, _, _ := runFault(args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	var noSecret bytes.Buffer
	if code := run(t.Context(), []string{"fault", "clear"}, env(map[string]string{"SPILLWAY_DEMO_CP_URL": srv.URL}), &noSecret, &noSecret); code != 2 ||
		!strings.Contains(noSecret.String(), "SPILLWAY_DEMO_CLIENT_SECRET") {
		t.Errorf("no secret: %d %q", code, noSecret.String())
	}
	if s := all.String(); strings.Contains(s, sec) || strings.Contains(s, fakeToken) || strings.Contains(s, "eyJ") || strings.Contains(s, "Bearer") {
		t.Errorf("a secret or token was printed:\n%s", s)
	}
}
