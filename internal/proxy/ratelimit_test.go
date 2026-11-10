package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
)

// exampleRoutes are ADR-0017's example routes, to an upstream named httpbin.
func exampleRoutes(u *upstream) string {
	return fmt.Sprintf(`
upstreams: [{name: httpbin, endpoints: [{address: 'httpbin:%s'}]}]
routes:
  - {name: bytes, match: {path_prefix: /bytes}, upstream: httpbin}
  - {name: anything, match: {path_prefix: /anything}, upstream: httpbin, cost: 5}
  - {name: delay, match: {path_prefix: /delay}, upstream: httpbin}
  - {name: default, match: {path_prefix: /}, upstream: httpbin}
`, u.port())
}

// examplePolicies are ADR-0017's example policies.
const examplePolicies = `
policies:
  - {name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 200, period: 1s}], local: true}
  - name: per-consumer
    level: consumer
    match: {routes: ['*']}
    key_template: '{consumer}'
    algorithm: gcra
    limits: [{rate: 100, period: 1m}, {rate: 10000, period: 1h, burst: 500}]
    local: true
  - {name: tenant-writes, level: tenant, match: {routes: [anything], methods: [POST, PUT, PATCH, DELETE]}, key_template: '{tenant}:{route}', algorithm: sliding_window, limits: [{rate: 600, period: 1m}], local: true}
  - {name: per-key-daily, level: consumer, match: {routes: ['*']}, key_template: '{api_key}', algorithm: fixed_window, limits: [{rate: 100000, period: 24h}], local: true}
  - {name: delay-inflight, level: route, match: {routes: [delay]}, key_template: '{route}', algorithm: concurrency, limits: [{rate: 20}]}
  - {name: bytes-per-agent, level: global, match: {routes: [bytes], methods: [GET]}, key_template: '{ip}:{header:User-Agent}', algorithm: gcra, limits: [{rate: 5, period: 1s}], mode: shadow, local: true}
  - {name: retired, level: global, match: {routes: ['*']}, key_template: '{method}', algorithm: fixed_window, limits: [{rate: 10, period: 1s}], mode: off, local: true}
`

// response is what a test needs of a gateway response.
type response struct {
	status int
	header http.Header
	body   string
}

// send sends a request through g with the given headers.
func (g *gateway) send(t *testing.T, method, path string, header http.Header) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, g.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, string(body)}
}

// jsonOf is v as JSON, with map keys sorted.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// rateLimitFields is how a test shows a response's rate limit fields.
func rateLimitFields(h http.Header) string {
	return fmt.Sprintf("Retry-After: %q\nRateLimit-Policy: %q\nRateLimit: %q",
		h.Values("Retry-After"), h.Values("RateLimit-Policy"), h.Values("RateLimit"))
}

// limiter returns g's limiter for a policy limit; period is 0 for
// concurrency.
func (g *gateway) limiter(t *testing.T, policy, algorithm string, period time.Duration) ratelimit.Limiter {
	t.Helper()
	l := g.limiters.Limiter(local.ID{Policy: policy, Algorithm: algorithm, Period: period})
	if l == nil {
		t.Fatalf("no limiter for %s|%s|%v", policy, algorithm, period)
	}
	return l
}

// peek returns a limiter's state for key without changing it: a cost of 0.
func peek(l ratelimit.Limiter, key string) ratelimit.Decision {
	d, _ := l.Allow(context.Background(), ratelimit.Key(key), 0)
	return d
}

// TestRateLimitVectors checks ADR-0018's test vectors: the exact
// RateLimit-Policy, RateLimit and Retry-After of each, which replace the
// upstream's own fields whenever a gateway limit applies (RL-25).
func TestRateLimitVectors(t *testing.T) {
	u := serveUpstream(t, "httpbin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("RateLimit-Policy", `"upstream";q=1;w=1`)
		w.Header().Set("RateLimit", `"upstream";r=1;t=1`)
	})
	onlyPerIP := `
policies: [{name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 10, period: 1s}], local: true}]
`
	alice := http.Header{"X-Api-Key": {aliceKey}}
	type want struct {
		status            int
		retryAfter        string
		policy, rateLimit string
	}
	expect := func(t *testing.T, got response, w want) {
		t.Helper()
		wantFields := rateLimitFields(http.Header{"Retry-After": {w.retryAfter}, "Ratelimit-Policy": {w.policy}, "Ratelimit": {w.rateLimit}})
		if w.retryAfter == "" {
			wantFields = rateLimitFields(http.Header{"Ratelimit-Policy": {w.policy}, "Ratelimit": {w.rateLimit}})
		}
		if got.status != w.status || rateLimitFields(got.header) != wantFields {
			t.Errorf("got %d\n%s\nwant %d\n%s", got.status, rateLimitFields(got.header), w.status, wantFields)
		}
	}

	t.Run("1 one policy, allowed, sub-second reset", func(t *testing.T) {
		g := newGateway(t, newResolver(u), exampleRoutes(u)+onlyPerIP)
		expect(t, g.send(t, http.MethodGet, "/bytes/1", nil), want{200, "", `"per-ip";q=10;w=1`, `"per-ip";r=9;t=1`})
	})

	t.Run("2 one policy, denied", func(t *testing.T) {
		g := newGateway(t, newResolver(u), exampleRoutes(u)+onlyPerIP)
		for range 10 {
			g.send(t, http.MethodGet, "/bytes/1", nil)
		}
		expect(t, g.send(t, http.MethodGet, "/bytes/1", nil), want{429, "1", `"per-ip";q=10;w=1`, `"per-ip";r=0;t=1`})
	})

	t.Run("3 several policies and limits, allowed", func(t *testing.T) {
		g := newGateway(t, newResolver(u), exampleRoutes(u)+examplePolicies)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey, "bob": bobKey}))
		expect(t, g.send(t, http.MethodGet, "/bytes/1", alice), want{200, "",
			`"per-ip";q=200;w=1, "per-consumer/60s";q=100;w=60, "per-consumer/3600s";q=10000;w=3600, "per-key-daily";q=100000;w=86400`,
			`"per-ip";r=199;t=1, "per-consumer/60s";r=99;t=1, "per-consumer/3600s";r=499;t=1, "per-key-daily";r=99999;t=43200`})
	})

	t.Run("4 several policies, one limit denies, the rest refunded", func(t *testing.T) {
		g := newGateway(t, newResolver(u), exampleRoutes(u)+examplePolicies)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey, "bob": bobKey}))
		for i := range 100 {
			if got := g.send(t, http.MethodGet, "/bytes/1", alice); got.status != http.StatusOK {
				t.Fatalf("request %d: status %d", i+1, got.status)
			}
		}
		expect(t, g.send(t, http.MethodGet, "/bytes/1", alice), want{429, "1",
			`"per-ip";q=200;w=1, "per-consumer/60s";q=100;w=60, "per-consumer/3600s";q=10000;w=3600, "per-key-daily";q=100000;w=86400`,
			`"per-ip";r=100;t=1, "per-consumer/60s";r=0;t=60, "per-consumer/3600s";r=400;t=36, "per-key-daily";r=99900;t=43200`})
	})

	t.Run("5 per-replica quota and a fractional Retry-After", func(t *testing.T) {
		g := newGateway(t, newResolver(u), exampleRoutes(u)+`
ratelimit: {replicas: 3}
policies: [{name: per-route, level: route, match: {routes: ['*']}, key_template: '{route}', algorithm: fixed_window, limits: [{rate: 100, period: 1m}], local: true}]
`)
		g.clock.Advance(44*time.Second + 750*time.Millisecond) // 45 s into the window
		_, _ = g.limiter(t, "per-route", config.FixedWindow, time.Minute).Allow(t.Context(), ratelimit.Key("bytes"), 32)
		expect(t, g.send(t, http.MethodGet, "/bytes/1", nil), want{200, "", `"per-route";q=33;w=60`, `"per-route";r=0;t=15`})
		g.clock.Advance(500 * time.Millisecond)
		expect(t, g.send(t, http.MethodGet, "/bytes/1", nil), want{429, "15", `"per-route";q=33;w=60`, `"per-route";r=0;t=15`})
	})

	t.Run("6 concurrency denial", func(t *testing.T) {
		g := newGateway(t, newResolver(u), exampleRoutes(u)+examplePolicies)
		_, _ = g.limiter(t, "delay-inflight", config.Concurrency, 0).Allow(t.Context(), ratelimit.Key("delay"), 20)
		expect(t, g.send(t, http.MethodGet, "/delay/1", nil), want{429, "1", `"per-ip";q=200;w=1`, `"per-ip";r=200;t=0`})
	})

	t.Run("7 shadow only", func(t *testing.T) {
		g := newGateway(t, newResolver(u), exampleRoutes(u)+`
policies: [{name: bytes-per-agent, level: global, match: {routes: [bytes], methods: [GET]}, key_template: '{ip}:{header:User-Agent}', algorithm: gcra, limits: [{rate: 5, period: 1s}], mode: shadow, local: true}]
`)
		for range 5 {
			g.send(t, http.MethodGet, "/bytes/1", nil)
		}
		got := g.send(t, http.MethodGet, "/bytes/1", nil)
		wantFields := rateLimitFields(http.Header{"Ratelimit-Policy": {`"upstream";q=1;w=1`}, "Ratelimit": {`"upstream";r=1;t=1`}})
		if got.status != http.StatusOK || rateLimitFields(got.header) != wantFields {
			t.Errorf("got %d\n%s\nwant 200 with the upstream's fields\n%s", got.status, rateLimitFields(got.header), wantFields)
		}
		if m, s := scrape(t, g.metrics), `gw_ratelimit_decisions_total{decision="deny",mode="shadow",policy="bytes-per-agent"} 1`; !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	})
}

// onePolicy is a config whose route r on / to u has one policy on {ip}, a
// fixed window of 2 a minute, in mode.
func onePolicy(u *upstream, mode string) string {
	return oneUpstream(u) + `
policies: [{name: p, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: fixed_window, limits: [{rate: 2, period: 1m}], mode: ` + mode + `, local: true}]
`
}

// TestRateLimitModes: an enforce policy answers 429 once its limit is spent;
// a shadow policy decides and records but never rejects or adds fields; an
// off policy is never evaluated. Every decision is in the metrics and the
// access log, and 429s count as ratelimited (RL-25 – RL-27).
func TestRateLimitModes(t *testing.T) {
	u := newUpstream(t, "orders")
	for _, tt := range []struct {
		mode     string
		statuses []int
		fields   []string // RateLimit per response, "" for none
		metrics  []string
		logged   string // the third request's access log decisions
	}{
		{
			mode:     "enforce",
			statuses: []int{200, 200, 429},
			fields:   []string{`"p";r=1;t=60`, `"p";r=0;t=60`, `"p";r=0;t=60`},
			metrics: []string{
				`gw_ratelimit_decisions_total{decision="allow",mode="enforce",policy="p"} 2`,
				`gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="p"} 1`,
				`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="ratelimited",route="r",tenant="none"} 1`,
				`gw_ratelimit_check_duration_seconds_count{backend="local"} 3`,
			},
			logged: `[{"decision":"deny","mode":"enforce","policy":"p"}]`,
		},
		{
			mode:     "shadow",
			statuses: []int{200, 200, 200},
			fields:   []string{"", "", ""},
			metrics: []string{
				`gw_ratelimit_decisions_total{decision="allow",mode="shadow",policy="p"} 2`,
				`gw_ratelimit_decisions_total{decision="deny",mode="shadow",policy="p"} 1`,
				`gw_requests_total{code_class="2xx",method="GET",origin="upstream",reason="none",route="r",tenant="none"} 3`,
			},
			logged: `[{"decision":"deny","mode":"shadow","policy":"p"}]`,
		},
		{
			mode:     "off",
			statuses: []int{200, 200, 200},
			fields:   []string{"", "", ""},
			metrics:  []string{`gw_requests_total{code_class="2xx",method="GET",origin="upstream",reason="none",route="r",tenant="none"} 3`},
			logged:   `[]`,
		},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			before := u.requests.Load()
			g := newGateway(t, newResolver(u), onePolicy(u, tt.mode))
			for i, status := range tt.statuses {
				id := tt.mode + "-" + strconv.Itoa(i)
				got := g.send(t, http.MethodGet, "/", http.Header{"X-Request-Id": {id}})
				if got.status != status || got.header.Get("RateLimit") != tt.fields[i] {
					t.Errorf("request %d: %d with RateLimit %q, want %d with %q", i+1, got.status, got.header.Get("RateLimit"), status, tt.fields[i])
				}
				if tt.fields[i] != "" && got.header.Get("RateLimit-Policy") != `"p";q=2;w=60` {
					t.Errorf("request %d: RateLimit-Policy %q", i+1, got.header.Get("RateLimit-Policy"))
				}
				if status == http.StatusTooManyRequests {
					if got.header.Get("Retry-After") != "60" || got.header.Get("Content-Type") != "application/json" ||
						got.body != `{"error":"rate limited"}` || got.header.Get("X-Request-Id") != id {
						t.Errorf("429: Retry-After %q, %q %q, request ID %q", got.header.Get("Retry-After"), got.header.Get("Content-Type"), got.body, got.header.Get("X-Request-Id"))
					}
				} else if got.header.Get("Retry-After") != "" {
					t.Errorf("request %d: Retry-After %q on a %d", i+1, got.header.Get("Retry-After"), got.status)
				}
			}
			allowed := int64(0)
			for _, s := range tt.statuses {
				if s == http.StatusOK {
					allowed++
				}
			}
			if n := u.requests.Load() - before; n != allowed {
				t.Errorf("the upstream got %d requests, want %d", n, allowed)
			}
			m := scrape(t, g.metrics)
			for _, s := range tt.metrics {
				if !strings.Contains(m, "\n"+s+"\n") {
					t.Errorf("/metrics has no %s", s)
				}
			}
			if tt.mode == "off" && strings.Contains(m, "gw_ratelimit_decisions_total{") {
				t.Error("an off policy recorded decisions")
			}
			line := g.logs.access(t, tt.mode+"-2")
			if got := jsonOf(t, line["decisions"]); got != tt.logged {
				t.Errorf("access log decisions = %s, want %s", got, tt.logged)
			}
		})
	}
}

// TestRateLimitAllPolicies: a request must pass every enforce limit; one that
// any denies consumes nothing from the others, shadow ones included, and the
// fields list every enforce limit (ADR-0017).
func TestRateLimitAllPolicies(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u)+`
policies:
  - {name: a, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 5, period: 1s}], local: true}
  - {name: b, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: fixed_window, limits: [{rate: 2, period: 1m}], local: true}
  - {name: c, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 100, period: 1s}], mode: shadow, local: true}
`)
	for i, want := range []struct {
		status    int
		rateLimit string
	}{
		{200, `"a";r=4;t=1, "b";r=1;t=60`},
		{200, `"a";r=3;t=1, "b";r=0;t=60`},
		{429, `"a";r=3;t=1, "b";r=0;t=60`}, // b denies; a keeps its 3
		{429, `"a";r=3;t=1, "b";r=0;t=60`},
	} {
		got := g.send(t, http.MethodGet, "/", http.Header{"X-Request-Id": {"all-" + strconv.Itoa(i)}})
		if got.status != want.status || got.header.Get("RateLimit") != want.rateLimit ||
			got.header.Get("RateLimit-Policy") != `"a";q=5;w=1, "b";q=2;w=60` {
			t.Errorf("request %d: %d\n%s\nwant %d with RateLimit %q", i+1, got.status, rateLimitFields(got.header), want.status, want.rateLimit)
		}
	}
	if d := peek(g.limiter(t, "c", config.GCRA, time.Second), "127.0.0.1"); d.Remaining != 98 {
		t.Errorf("shadow c has %d left, want 98: only the 2 allowed requests count", d.Remaining)
	}
	line := g.logs.access(t, "all-2")
	if got, want := jsonOf(t, line["decisions"]), `[{"decision":"allow","mode":"enforce","policy":"a"},{"decision":"deny","mode":"enforce","policy":"b"},{"decision":"allow","mode":"shadow","policy":"c"}]`; got != want {
		t.Errorf("access log decisions = %s, want %s", got, want)
	}
}

// TestRateLimitTemplates: a policy whose key template cannot be filled for a
// request (no API key, a consumer without a tenant, no such header) does not
// apply to it; one that can be filled limits each value apart (RL-04).
func TestRateLimitTemplates(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u)+`
policies:
  - {name: by-consumer, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: fixed_window, limits: [{rate: 1, period: 1m}], local: true}
  - {name: by-tenant, level: tenant, match: {routes: ['*']}, key_template: 'tenant:{tenant}', algorithm: fixed_window, limits: [{rate: 1, period: 1m}], local: true}
  - {name: by-key, level: consumer, match: {routes: ['*']}, key_template: '{api_key}', algorithm: fixed_window, limits: [{rate: 1, period: 1m}], mode: shadow, local: true}
  - {name: by-client, level: global, match: {routes: ['*'], methods: [GET]}, key_template: '{header:X-Client}/{method}', algorithm: fixed_window, limits: [{rate: 1, period: 1m}], local: true}
`)
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey, "bob": bobKey}))
	for i, tt := range []struct {
		name     string
		method   string
		header   http.Header
		status   int
		policies string // RateLimit-Policy
	}{
		{"anonymous", "GET", nil, 200, ""},
		{"anonymous again", "GET", nil, 200, ""},
		{"bob, who has no tenant", "GET", http.Header{"X-Api-Key": {bobKey}}, 200, `"by-consumer";q=1;w=60`},
		{"bob again", "GET", http.Header{"X-Api-Key": {bobKey}}, 429, `"by-consumer";q=1;w=60`},
		{"alice", "GET", http.Header{"X-Api-Key": {aliceKey}}, 200, `"by-consumer";q=1;w=60, "by-tenant";q=1;w=60`},
		{"client x", "GET", http.Header{"X-Client": {"x"}}, 200, `"by-client";q=1;w=60`},
		{"client x again", "GET", http.Header{"X-Client": {"x"}}, 429, `"by-client";q=1;w=60`},
		{"client y", "GET", http.Header{"X-Client": {"y"}}, 200, `"by-client";q=1;w=60`},
		{"client x, another method", "POST", http.Header{"X-Client": {"x"}}, 200, ""},
		{"empty X-Client", "GET", http.Header{"X-Client": {""}}, 200, ""},
	} {
		got := g.send(t, tt.method, "/", tt.header)
		if got.status != tt.status || got.header.Get("RateLimit-Policy") != tt.policies {
			t.Errorf("%d %s: %d with RateLimit-Policy %q, want %d with %q", i, tt.name, got.status, got.header.Get("RateLimit-Policy"), tt.status, tt.policies)
		}
	}
	m := scrape(t, g.metrics)
	for _, s := range []string{
		`gw_ratelimit_decisions_total{decision="allow",mode="shadow",policy="by-key"} 2`,
		`gw_ratelimit_decisions_total{decision="deny",mode="shadow",policy="by-key"} 1`,
		`gw_ratelimit_decisions_total{decision="allow",mode="enforce",policy="by-tenant"} 1`,
	} {
		if !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}
	// Map keys are digests: neither the API keys nor their hashes are in the
	// logs or metrics.
	for _, k := range []string{aliceKey, bobKey} {
		if strings.Contains(g.logs.String()+m, k) {
			t.Errorf("%q appears in the logs or metrics", k)
		}
	}
}

// TestRateLimitPhases: identity-free policies run before API key resolution.
// A request they deny is answered 429 without resolving its key; one they
// admit that has an invalid key is answered 401 and keeps their deductions,
// so floods of bad keys meet the IP limits (ADR-0017).
func TestRateLimitPhases(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u)+`
policies: [{name: ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: fixed_window, limits: [{rate: 2, period: 1m}], local: true}]
`)
	bad := http.Header{"X-Api-Key": {aliceKey}} // no identity: every key is invalid
	for i, want := range []struct {
		status    int
		rateLimit string
		keyHash   string
	}{
		{401, `"ip";r=1;t=60`, config.KeyHash(aliceKey)},
		{401, `"ip";r=0;t=60`, config.KeyHash(aliceKey)},
		{429, `"ip";r=0;t=60`, ""},
	} {
		id := "phase-" + strconv.Itoa(i)
		got := g.send(t, http.MethodGet, "/", http.Header{"X-Api-Key": bad["X-Api-Key"], "X-Request-Id": {id}})
		if got.status != want.status || got.header.Get("RateLimit") != want.rateLimit {
			t.Errorf("request %d: %d with RateLimit %q, want %d with %q", i+1, got.status, got.header.Get("RateLimit"), want.status, want.rateLimit)
		}
		if line := g.logs.access(t, id); line["key_hash"] != want.keyHash {
			t.Errorf("request %d: key_hash %v, want %q", i+1, line["key_hash"], want.keyHash)
		}
	}
}

// TestRateLimitConcurrencyRelease: a concurrency slot is held while its
// request runs and released exactly once when it completes, times out or
// loses its client, with no goroutine left behind (RL-02). Another holder
// keeps a second slot throughout, so a second release would show.
func TestRateLimitConcurrencyRelease(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	arrived := make(chan struct{}, 10)
	u := serveUpstream(t, "orders", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/orders/ok" {
			return
		}
		arrived <- struct{}{}
		<-r.Context().Done() // until the gateway gives up on it
	})
	g := newGateway(t, newResolver(u), routeTo(u, "timeouts: {total: 300ms}")+`
policies: [{name: inflight, level: route, match: {routes: [r]}, key_template: '{route}', algorithm: concurrency, limits: [{rate: 2}]}]
`)
	inflight := g.limiter(t, "inflight", config.Concurrency, 0)
	if d, _ := inflight.Allow(t.Context(), ratelimit.Key("r"), 1); !d.Allowed {
		t.Fatal("the other holder got no slot")
	}
	free := func(what string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); peek(inflight, "r").Remaining == 0; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("after %s: the slot was not released", what)
			}
		}
		time.Sleep(50 * time.Millisecond) // time for a second release to land
		if d := peek(inflight, "r"); d.Remaining != 1 {
			t.Fatalf("after %s: %d free, want 1: released more than once", what, d.Remaining)
		}
	}

	if got := g.send(t, http.MethodGet, "/v1/orders/ok", nil); got.status != http.StatusOK {
		t.Fatalf("status %d", got.status)
	}
	free("a completed request")

	// While a request stalls, the slot is taken; the route's timeout ends it.
	done := make(chan response)
	go func() { done <- g.send(t, http.MethodGet, "/v1/orders/stall", nil) }()
	<-arrived
	if got := g.send(t, http.MethodGet, "/v1/orders/ok", nil); got.status != http.StatusTooManyRequests || got.header.Get("Retry-After") != "1" {
		t.Errorf("second request while one is in flight: %d, Retry-After %q; want 429, 1", got.status, got.header.Get("Retry-After"))
	}
	if got := <-done; got.status != http.StatusGatewayTimeout {
		t.Errorf("stalled request: %d, want 504", got.status)
	}
	free("a timeout")

	// A client that goes away.
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+"/v1/orders/stall", nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error)
	go func() {
		resp, err := g.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	<-arrived
	cancel()
	if err := <-errc; err == nil {
		t.Error("the cancelled request got a response")
	}
	free("a client disconnect")

	g.Close()
	u.Close()
}

// TestRateLimitReloadUnderLoad: while many requests run, snapshots are
// swapped over and over, with an unrelated change and with rate, burst and
// max-in-flight changes; no bucket and no in-flight count is reset (GW-29).
func TestRateLimitReloadUnderLoad(t *testing.T) {
	release := make(chan struct{})
	var held atomic.Int64
	u := serveUpstream(t, "orders", func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" && held.Add(1) <= 6 {
			<-release // the first 6; any later one was wrongly admitted
		}
	})
	yaml := func(version, rate, burst, inflight int) string {
		return fmt.Sprintf(`
version: %d
upstreams: [{name: orders, endpoints: [{address: 'orders:%s'}]}]
routes:
  - {name: hold, match: {path: /hold}, upstream: orders}
  - {name: fast, match: {path_prefix: /}, upstream: orders}
policies:
  - {name: per-ip, level: global, match: {routes: [fast]}, key_template: '{ip}', algorithm: gcra, limits: [{rate: %d, period: 1s, burst: %d}], local: true}
  - {name: hold-inflight, level: route, match: {routes: [hold]}, key_template: '{route}', algorithm: concurrency, limits: [{rate: %d}]}
`, version, u.port(), rate, burst, inflight)
	}
	// Both rates have a burst tolerance of 2 s (T·burst), and the clock
	// stands still, so once the bucket is spent neither admits anything
	// more; nor does either max in flight once 6 requests hold.
	g := newGateway(t, newResolver(u), yaml(1, 20, 40, 6))
	var snapshots []*config.Snapshot
	for v, c := range [][3]int{{10, 20, 5}, {10, 20, 5}, {20, 40, 6}, {20, 40, 6}} {
		snapshots = append(snapshots, snapshot(t, yaml(v+2, c[0], c[1], c[2])))
	}

	for i := range 40 {
		if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
			t.Fatalf("request %d: %d, want 200 within the burst", i+1, got.status)
		}
	}
	if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusTooManyRequests {
		t.Fatalf("request 41: %d, want 429", got.status)
	}
	var holds sync.WaitGroup
	for range 6 {
		holds.Go(func() {
			if got := g.send(t, http.MethodGet, "/hold", nil); got.status != http.StatusOK {
				t.Errorf("held request: %d, want 200", got.status)
			}
		})
	}
	for deadline := time.Now().Add(5 * time.Second); held.Load() != 6; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of 6 requests reached the upstream", held.Load())
		}
	}

	stop := make(chan struct{})
	var reloads sync.WaitGroup
	reloads.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				g.swap(t, snapshots[i%len(snapshots)])
			}
		}
	})
	var admitted atomic.Int64
	var load sync.WaitGroup
	for range 20 {
		load.Go(func() {
			for i := range 25 {
				path := "/"
				if i%5 == 0 {
					path = "/hold"
				}
				if got := g.send(t, http.MethodGet, path, nil); got.status != http.StatusTooManyRequests {
					admitted.Add(1)
				}
			}
		})
	}
	load.Wait()
	close(stop)
	reloads.Wait()
	if n := admitted.Load(); n != 0 {
		t.Errorf("%d requests admitted during reloads, want 0: a bucket or in-flight count was reset", n)
	}

	close(release)
	holds.Wait()
	g.swap(t, snapshots[2]) // max 6
	if d := peek(g.limiter(t, "hold-inflight", config.Concurrency, 0), "hold"); d.Remaining != 6 {
		t.Errorf("after the held requests: %d of 6 free, want 6", d.Remaining)
	}
}
