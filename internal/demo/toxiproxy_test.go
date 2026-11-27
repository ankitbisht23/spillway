package demo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeToxiproxy serves toxiproxy's /proxies list, create and delete.
type fakeToxiproxy struct {
	mu      sync.Mutex
	proxies map[string]Proxy
	deletes []string
}

func (f *fakeToxiproxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/proxies":
		_ = json.NewEncoder(w).Encode(f.proxies)
	case r.Method == "POST" && r.URL.Path == "/proxies":
		var p struct {
			Proxy
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || !p.Enabled {
			w.WriteHeader(400)
			return
		}
		if _, ok := f.proxies[p.Name]; ok {
			w.WriteHeader(409)
			return
		}
		f.proxies[p.Name] = p.Proxy
		w.WriteHeader(201)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/proxies/"):
		name := strings.TrimPrefix(r.URL.Path, "/proxies/")
		f.deletes = append(f.deletes, name)
		if _, ok := f.proxies[name]; !ok {
			w.WriteHeader(404)
			return
		}
		delete(f.proxies, name)
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}

// TestDemoProxies: six Redis proxies on 26380-26385 and three upstream
// proxies on 26391-26393 (docs/ports.md), all named demo-*.
func TestDemoProxies(t *testing.T) {
	ps := DemoProxies()
	if len(ps) != 9 {
		t.Fatalf("%d proxies", len(ps))
	}
	want := map[string]string{
		"demo-redis-0": "0.0.0.0:26380>redis-0:6379", "demo-redis-5": "0.0.0.0:26385>redis-5:6379",
		"demo-upstream-1": "0.0.0.0:26391>upstream-1:8080", "demo-upstream-3": "0.0.0.0:26393>upstream-3:8080",
	}
	for _, p := range ps {
		if !strings.HasPrefix(p.Name, DemoPrefix) {
			t.Errorf("%s", p.Name)
		}
		if w, ok := want[p.Name]; ok && w != p.Listen+">"+p.Upstream {
			t.Errorf("%s: %s > %s", p.Name, p.Listen, p.Upstream)
		}
	}
}

// TestToxiproxyCreateDelete: Create replaces a proxy of the same name;
// Delete of a missing proxy is fine.
func TestToxiproxyCreateDelete(t *testing.T) {
	f := &fakeToxiproxy{proxies: map[string]Proxy{"demo-redis-0": {Name: "demo-redis-0", Listen: "0.0.0.0:1", Upstream: "old:1"}, "redis-0": {Name: "redis-0"}}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tp := NewToxiproxy(srv.URL, 5*time.Second)
	for _, p := range DemoProxies() {
		if err := tp.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	names, err := tp.Names(t.Context())
	if err != nil || len(names) != 10 || f.proxies["demo-redis-0"].Upstream != "redis-0:6379" {
		t.Fatalf("%v %v %v", names, err, f.proxies["demo-redis-0"])
	}
	if err := tp.Delete(t.Context(), "demo-redis-0"); err != nil {
		t.Fatal(err)
	}
	if err := tp.Delete(t.Context(), "demo-redis-0"); err != nil {
		t.Errorf("second delete: %v", err)
	}
	if names, _ = tp.Names(t.Context()); slices.Contains(names, "demo-redis-0") || !slices.Contains(names, "redis-0") {
		t.Errorf("%v", names)
	}
}
