package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/heavyhitters"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// hitterPolicies have every kind of event of ADR-0041: local enforce and
// shadow policies before authentication, and Redis enforce (degraded),
// fail_open and shadow ones per consumer.
const hitterPolicies = `
policies:
  - {id: 41, name: per-client, level: global, match: {routes: ['*']}, key_template: '{header:x-client}', algorithm: gcra, limits: [{rate: 2, period: 1s}], local: true}
  - {id: 42, name: shadow-client, level: global, match: {routes: ['*']}, key_template: '{header:x-client}', algorithm: gcra, limits: [{rate: 1, period: 1s}], mode: shadow, local: true}
  - {id: 43, name: redis-consumer, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, limits: [{rate: 3, period: 1s}], failure_mode: degraded}
  - {id: 44, name: redis-open, level: consumer, match: {routes: ['*']}, key_template: '{consumer}:{route}', algorithm: gcra, limits: [{rate: 100, period: 1s}], failure_mode: fail_open}
  - {id: 45, name: redis-shadow, level: consumer, match: {routes: ['*']}, key_template: '{route}:{consumer}', algorithm: gcra, limits: [{rate: 1, period: 1s}], mode: shadow, failure_mode: fail_open}
`

// TestHeavyHitterEvents checks what the handler counts in the heavy
// hitters (RL-21, ADR-0041): one event per policy that denies, or would
// deny in shadow mode, from local, Redis and degraded limiters, keyed by
// the policy's ID and the digest of its filled template (the Redis key's
// digest, never the raw key), with the request's consumer and API key IDs
// once it is authenticated; not a failure mode's outcome, nor an exempt
// request (ADR-0050).
func TestHeavyHitterEvents(t *testing.T) {
	w := newPlanWorld(t, "127.0.0.1/32")
	w.g.swap(t, managedSnapshot(t, w.routes+hitterPolicies))
	hitters := heavyhitters.New()
	w.g.h.SetHeavyHitters(hitters)
	send := func(consumer, client, ip string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, w.g.URL+"/orders", nil)
		if consumer != "" {
			req.Header.Set("X-Api-Key", w.keys[consumer])
		}
		req.Header.Set("X-Client", client)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := w.g.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	const ip = "192.0.2.10"

	// Phase 1, anonymous: per-client denies the third, shadow-client would
	// deny the second and third.
	var got []int
	for range 3 {
		got = append(got, send("", "client-secret-c1", ip))
	}
	// Redis, as app (consumer 5, key 72): redis-shadow would deny from the
	// second, redis-consumer denies the fourth and fifth.
	for i := range 5 {
		got = append(got, send("app", "client-"+string(rune('a'+i)), ip))
	}
	// Redis down: redis-consumer's degraded limiter (3 per replica) denies
	// the fourth; fail_open outcomes are not counted.
	w.redis.setFail("{t:2}", true)
	for i := range 4 {
		got = append(got, send("app", "client-"+string(rune('k'+i)), ip))
	}
	// Exempt: a CIDR allow skips every policy, so no count.
	w.setIdentity(t, []*configpb.AccessListEntry{{Id: 1, Action: "allow", SubjectType: "cidr", Cidr: "192.0.2.0/24"}}, nil)
	got = append(got, send("", "client-secret-c1", ip), send("app", "client-z", ip))
	if want := []int{200, 200, 429, 200, 200, 200, 429, 429, 200, 200, 200, 429, 200, 200}; !slicesEqual(got, want) {
		t.Fatalf("statuses %v, want %v", got, want)
	}

	entries, _ := hitters.Take()
	by := map[int64]heavyhitters.Entry{}
	for _, e := range entries {
		if _, dup := by[e.Policy]; dup {
			t.Errorf("two entries for policy %d", e.Policy)
		}
		by[e.Policy] = e
	}
	type counts struct{ denied, shadow, consumer, key int64 }
	for policy, want := range map[int64]counts{
		41: {1, 0, 0, 0},  // before authentication: no IDs
		42: {0, 2, 0, 0},  //
		43: {3, 0, 5, 72}, // two in Redis, one degraded
		45: {0, 4, 5, 72},
	} {
		e := by[policy]
		if g := (counts{e.Denied, e.ShadowDenied, e.Consumer, e.APIKey}); g != want {
			t.Errorf("policy %d: %+v, want %+v", policy, g, want)
		}
	}
	if len(entries) != 4 {
		t.Errorf("%d entries, want 4 (none for fail_open redis-open): %+v", len(entries), entries)
	}
	if d := digestOf(by[41]); d != rlredis.Digest("client-secret-c1") {
		t.Errorf("per-client digest %s, want the filled template's", d)
	}
	// redis-consumer's digest is its Redis key's, degraded events included.
	w.redis.mu.Lock()
	calls := strings.Join(w.redis.calls, "\n")
	w.redis.mu.Unlock()
	if d := digestOf(by[43]); !strings.Contains(calls, "rl:v1:{t:2}:43:1000:"+d+" ") {
		t.Errorf("redis-consumer digest %s is not its Redis key's:\n%s", d, calls)
	}
	for _, e := range entries {
		if strings.Contains(digestOf(e), "c1") || strings.Contains(string(e.Digest[:]), "client") {
			t.Errorf("entry %+v carries a raw key", e)
		}
	}
	if logs := w.g.logs.String() + w.g.events.String(); strings.Contains(logs, "client-secret-c1") || strings.Contains(logs, w.keys["app"]) {
		t.Error("a raw key appears in the logs")
	}
}

func slicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// denyHandler is a managed handler with one local policy that allows a
// single request per client IP a minute, so every later request is a deny.
func denyHandler(tb testing.TB, hitters *heavyhitters.Sketch) *Handler {
	tb.Helper()
	discard := slog.New(slog.DiscardHandler)
	reg := NewRegistry(loopback, &fakeResolver{hosts: map[string][]netip.Addr{}}, discard)
	tb.Cleanup(reg.Close)
	c, err := config.Load([]byte(`
upstreams: [{name: u, endpoints: [{address: '127.0.0.1:1'}]}]
routes: [{name: r, upstream: u, match: {path_prefix: /}}]
policies: [{id: 7, name: p, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1, period: 1m}], local: true}]
`))
	if err != nil {
		tb.Fatal(err)
	}
	s, err := config.CompileManaged(c)
	if err != nil {
		tb.Fatal(err)
	}
	if err := reg.Apply(s); err != nil {
		tb.Fatal(err)
	}
	limiters := local.NewRegistry(ratelimit.SystemClock)
	limiters.Apply(s)
	var current atomic.Pointer[config.Snapshot]
	current.Store(s)
	var ids atomic.Pointer[config.Identity]
	h := NewHandler(current.Load, ids.Load, nil, reg, limiters, nil, false, 4096, nil, telemetry.NewRecorder(prometheus.NewRegistry(), discard, 0), discard)
	h.SetHeavyHitters(hitters)
	return h
}

func serveDenied(tb testing.TB, h *Handler) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if w.Code != http.StatusTooManyRequests {
		tb.Fatalf("status %d", w.Code)
	}
}

// TestHeavyHitterAllocations: counting a deny in the sketch adds no
// allocation to the deny path (ADR-0041).
func TestHeavyHitterAllocations(t *testing.T) {
	without, with := denyHandler(t, nil), denyHandler(t, heavyhitters.New())
	for _, h := range []*Handler{without, with} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)) // the one allowed
	}
	serveDenied(t, without)
	serveDenied(t, with)
	a := testing.AllocsPerRun(200, func() { serveDenied(t, without) })
	b := testing.AllocsPerRun(200, func() { serveDenied(t, with) })
	if b > a {
		t.Errorf("a denied request: %.0f allocations with the sketch, %.0f without", b, a)
	}
}

// BenchmarkDenied is one denied request through the handler, without and
// with the heavy-hitter sketch (RL-21): the sketch's cost on the hot path.
func BenchmarkDenied(b *testing.B) {
	for _, bc := range []struct {
		name    string
		hitters *heavyhitters.Sketch
	}{{"without", nil}, {"with", heavyhitters.New()}} {
		b.Run(bc.name, func(b *testing.B) {
			h := denyHandler(b, bc.hitters)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
			b.ReportAllocs()
			for b.Loop() {
				serveDenied(b, h)
			}
		})
	}
}

// digestOf is an entry's limiter key digest, as the Redis key carries it.
func digestOf(e heavyhitters.Entry) string { return rlredis.KeyDigest(string(e.Digest[:])) }
