package demo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/demo"
)

// fakeToxiproxy serves toxiproxy's proxy list with toxics and its toxic
// create and delete, and counts the changes.
type fakeToxiproxy struct {
	mu      sync.Mutex
	proxies map[string][]demo.Toxic
	changes int
	failOn  string // a path whose POST or DELETE answers 500
	down    bool   // every request answers 503
}

func newFakeToxiproxy(t *testing.T) (*fakeToxiproxy, *httptest.Server) {
	t.Helper()
	f := &fakeToxiproxy{proxies: map[string][]demo.Toxic{}}
	for _, p := range demo.DemoProxies() {
		f.proxies[p.Name] = nil
	}
	f.proxies["redis-0"] = []demo.Toxic{{Name: "latency_downstream", Type: "latency"}} // ADR-0074's, never touched
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeToxiproxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(503)
		return
	}
	if r.Method != "GET" && r.URL.Path == f.failOn {
		w.WriteHeader(500)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == "GET" && r.URL.Path == "/proxies":
		out := map[string]any{}
		for name, toxics := range f.proxies {
			out[name] = map[string]any{"name": name, "enabled": true, "toxics": append([]demo.Toxic{}, toxics...)}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && len(parts) == 3 && parts[2] == "toxics":
		toxics, ok := f.proxies[parts[1]]
		var x demo.Toxic
		if !ok {
			w.WriteHeader(404)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&x); err != nil {
			w.WriteHeader(400)
			return
		}
		if slices.ContainsFunc(toxics, func(t demo.Toxic) bool { return t.Name == x.Name }) {
			w.WriteHeader(409)
			return
		}
		f.proxies[parts[1]] = append(toxics, x)
		f.changes++
		_ = json.NewEncoder(w).Encode(x)
	case r.Method == "DELETE" && len(parts) == 4 && parts[2] == "toxics":
		toxics, ok := f.proxies[parts[1]]
		i := slices.IndexFunc(toxics, func(t demo.Toxic) bool { return t.Name == parts[3] })
		if !ok || i < 0 {
			w.WriteHeader(404)
			return
		}
		f.proxies[parts[1]] = slices.Delete(toxics, i, i+1)
		f.changes++
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}

// toxics returns the toxics on proxy, by name.
func (f *fakeToxiproxy) toxics(proxy string) map[string]demo.Toxic {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]demo.Toxic{}
	for _, x := range f.proxies[proxy] {
		out[x.Name] = x
	}
	return out
}

func state(t *testing.T, f *Faults, name string) string {
	t.Helper()
	st := f.States(t.Context())[name]
	if st == nil {
		return "unknown"
	}
	return fmt.Sprint(*st)
}

// TestFaultsOnOff: redis-cut on puts demo-cut-up and demo-cut-down (timeout
// 0, upstream and downstream) on all six demo-redis proxies and nothing
// elsewhere; upstream-slow on puts demo-slow (latency 4,000 ms downstream)
// on the three demo-upstream proxies; switching a fault to the state it
// has changes nothing in toxiproxy; off removes exactly the fault's toxics;
// states are read back from toxiproxy (ADR-0095 §5).
func TestFaultsOnOff(t *testing.T) {
	fake, srv := newFakeToxiproxy(t)
	f := NewFaults(srv.URL)
	if s := state(t, f, RedisCut) + " " + state(t, f, UpstreamSlow); s != "false false" {
		t.Fatalf("states before: %s", s)
	}
	before, err := f.Set(t.Context(), RedisCut, true)
	if err != nil || before {
		t.Fatalf("redis-cut on: before %v, %v", before, err)
	}
	for n := range 6 {
		got := fake.toxics(fmt.Sprintf("demo-redis-%d", n))
		up, down := got["demo-cut-up"], got["demo-cut-down"]
		if len(got) != 2 || up.Type != "timeout" || up.Stream != "upstream" || up.Toxicity != 1 || up.Attributes["timeout"] != 0 ||
			down.Type != "timeout" || down.Stream != "downstream" || down.Toxicity != 1 {
			t.Errorf("demo-redis-%d: %+v", n, got)
		}
	}
	for _, p := range []string{"demo-upstream-1", "demo-upstream-2", "demo-upstream-3"} {
		if len(fake.toxics(p)) != 0 {
			t.Errorf("%s got toxics from redis-cut", p)
		}
	}
	if fake.changes != 12 || state(t, f, RedisCut) != "true" || state(t, f, UpstreamSlow) != "false" {
		t.Errorf("after redis-cut on: %d changes, states %s %s", fake.changes, state(t, f, RedisCut), state(t, f, UpstreamSlow))
	}
	// Idempotent.
	if before, err := f.Set(t.Context(), RedisCut, true); err != nil || !before || fake.changes != 12 {
		t.Errorf("redis-cut on again: before %v, %v, %d changes", before, err, fake.changes)
	}
	if _, err := f.Set(t.Context(), UpstreamSlow, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"demo-upstream-1", "demo-upstream-2", "demo-upstream-3"} {
		x := fake.toxics(p)["demo-slow"]
		if len(fake.toxics(p)) != 1 || x.Type != "latency" || x.Stream != "downstream" || x.Attributes["latency"] != 4000 || x.Attributes["jitter"] != 0 {
			t.Errorf("%s: %+v", p, fake.toxics(p))
		}
	}
	if before, err := f.Set(t.Context(), RedisCut, false); err != nil || !before {
		t.Fatalf("redis-cut off: %v %v", before, err)
	}
	for n := range 6 {
		if got := fake.toxics(fmt.Sprintf("demo-redis-%d", n)); len(got) != 0 {
			t.Errorf("demo-redis-%d after off: %+v", n, got)
		}
	}
	if state(t, f, RedisCut) != "false" || state(t, f, UpstreamSlow) != "true" {
		t.Errorf("states %s %s", state(t, f, RedisCut), state(t, f, UpstreamSlow))
	}
	changes := fake.changes
	if before, err := f.Set(t.Context(), RedisCut, false); err != nil || before || fake.changes != changes {
		t.Errorf("redis-cut off again: %v %v, %d changes", before, err, fake.changes-changes)
	}
	if _, err := f.Set(t.Context(), UpstreamSlow, false); err != nil || state(t, f, UpstreamSlow) != "false" {
		t.Errorf("upstream-slow off: %v", err)
	}
	if got := fake.toxics("redis-0"); len(got) != 1 {
		t.Errorf("ADR-0074's redis-0 proxy was touched: %+v", got)
	}
	if _, err := f.Set(t.Context(), "nope", true); !errors.Is(err, ErrNoFault) {
		t.Errorf("an unknown fault: %v", err)
	}
}

// TestFaultsFailures: a missing proxy, a failed toxic change and toxiproxy
// down each fail with a FaultError naming the proxy and the toxic, never a
// value, leave what already changed in place, and a second call completes
// the change; a partly applied fault reads as active, so it shows on until
// it is switched off; states are unknown (nil) where toxiproxy cannot say.
func TestFaultsFailures(t *testing.T) {
	fake, srv := newFakeToxiproxy(t)
	f := NewFaults(srv.URL)

	fake.mu.Lock()
	delete(fake.proxies, "demo-upstream-2")
	fake.mu.Unlock()
	_, err := f.Set(t.Context(), UpstreamSlow, true)
	var fe *FaultError
	if !errors.As(err, &fe) || fe.Error() != "toxiproxy has no proxy demo-upstream-2 (make demo creates it)" || fake.changes != 0 {
		t.Errorf("a missing proxy: %v, %d changes", err, fake.changes)
	}
	if state(t, f, UpstreamSlow) != "unknown" || state(t, f, RedisCut) != "false" {
		t.Errorf("states with a missing proxy: %s %s", state(t, f, UpstreamSlow), state(t, f, RedisCut))
	}

	fake.mu.Lock()
	fake.failOn = "/proxies/demo-redis-3/toxics"
	fake.mu.Unlock()
	_, err = f.Set(t.Context(), RedisCut, true)
	if !errors.As(err, &fe) || fe.Error() != "toxiproxy could not change demo-cut-up on demo-redis-3" || fe.Cause == nil {
		t.Fatalf("a failed change: %v", err)
	}
	if fake.changes != 6 || state(t, f, RedisCut) != "true" {
		t.Errorf("after a failed change: %d changes kept, state %s; want 6 and true (partly on reads as on)", fake.changes, state(t, f, RedisCut))
	}
	fake.mu.Lock()
	fake.failOn = ""
	fake.mu.Unlock()
	if before, err := f.Set(t.Context(), RedisCut, true); err != nil || !before || fake.changes != 12 {
		t.Errorf("the second call: before %v, %v, %d changes", before, err, fake.changes)
	}

	fake.mu.Lock()
	fake.down = true
	fake.mu.Unlock()
	time.Sleep(stateMaxAge) // past the cache
	if _, err := f.Set(t.Context(), RedisCut, false); !errors.As(err, &fe) || fe.Error() != "toxiproxy could not be read" {
		t.Errorf("toxiproxy down: %v", err)
	}
	if state(t, f, RedisCut) != "unknown" || state(t, f, UpstreamSlow) != "unknown" {
		t.Error("toxiproxy down: states not unknown")
	}
}

// TestFaultStatesCached: States reads toxiproxy at most once a second, and
// a change reads it again at once.
func TestFaultStatesCached(t *testing.T) {
	var reads int
	var mu sync.Mutex
	fake, _ := newFakeToxiproxy(t)
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			mu.Lock()
			reads++
			mu.Unlock()
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(counting.Close)
	now := time.Unix(1_000_000, 0)
	f := NewFaults(counting.URL)
	f.now = func() time.Time { return now }
	for range 5 {
		f.States(t.Context())
	}
	now = now.Add(999 * time.Millisecond)
	f.States(t.Context())
	if reads != 1 {
		t.Errorf("%d reads within a second, want 1", reads)
	}
	now = now.Add(time.Millisecond)
	f.States(t.Context())
	if reads != 2 {
		t.Errorf("%d reads after a second, want 2", reads)
	}
	if _, err := f.Set(t.Context(), UpstreamSlow, true); err != nil {
		t.Fatal(err)
	}
	if state(t, f, UpstreamSlow) != "true" {
		t.Error("the state after a change is the cached one")
	}
}
