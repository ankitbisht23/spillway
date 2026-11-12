package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// fakeRedis is a slot-call backend for handler tests. Each key is a count of
// the cost units it holds out of its limit's capacity (burst for GCRA, rate
// for a sliding window), with no time; calls follow the script's write rule
// (docs/design.md "Limiter script contract").
type fakeRedis struct {
	mu          sync.Mutex
	used        map[string]int64
	calls       []string                 // one line per Call: cost, then each limit
	refunds     []string                 // one line per Refund: cost, admitted_us, then each key
	fail        map[string]struct{}      // hash tags whose calls fail as conn
	slow        map[string]time.Duration // hash tags whose calls answer this late, or time out at their deadline
	failRefunds bool                     // every Refund fails as conn
	deadlines   []time.Time              // of each Call's context
	arrivals    []time.Time              // when each Call arrived
	now         int64
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{used: map[string]int64{}, fail: map[string]struct{}{}, slow: map[string]time.Duration{}, now: 1_000_000}
}

// tagged returns m's value for the hash tag key has, if any.
func tagged[V any](m map[string]V, key string) (V, bool) {
	for tag, v := range m {
		if strings.Contains(key, tag) {
			return v, true
		}
	}
	var zero V
	return zero, false
}

func capacity(l rlredis.Limit) int64 {
	if l.Algorithm == config.GCRA {
		return l.Burst
	}
	return l.Rate
}

func (f *fakeRedis) Call(ctx context.Context, cost int64, limits []rlredis.Limit) (rlredis.Result, error) {
	f.mu.Lock()
	line := fmt.Sprint("cost ", cost)
	for _, l := range limits {
		mode := "shadow"
		if l.Enforce {
			mode = "enforce"
		}
		line += fmt.Sprintf(" | %s %s %s %s %d/%v burst %d", l.Key, l.Level, mode, l.Algorithm, l.Rate, l.Period, l.Burst)
	}
	f.calls = append(f.calls, line)
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	f.arrivals = append(f.arrivals, time.Now())
	delay, _ := tagged(f.slow, limits[0].Key)
	f.mu.Unlock()
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return rlredis.Result{}, &rlredis.Error{Kind: rlredis.KindTimeout, Node: "redis-1:6379", Err: ctx.Err()}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := tagged(f.fail, limits[0].Key); ok {
		return rlredis.Result{}, &rlredis.Error{Kind: rlredis.KindConn, Node: "redis-1:6379", Err: errors.New("connection refused")}
	}
	f.now += 1000
	res := rlredis.Result{Allowed: true, DenyingLimit: -1, NowUS: f.now, Limits: make([]ratelimit.Decision, len(limits))}
	for i, l := range limits {
		ok := f.used[l.Key]+cost <= capacity(l)
		res.Limits[i].Allowed = ok
		if !ok && l.Enforce {
			res.Allowed, res.DenyingLimit, res.RetryAfter = false, i, l.Period
		}
		if !ok && !l.Enforce {
			res.ShadowWouldDeny |= 1 << i
		}
	}
	for i, l := range limits {
		d := &res.Limits[i]
		if res.Allowed && d.Allowed {
			f.used[l.Key] += cost
		}
		f.state(l, d)
		if !d.Allowed {
			d.RetryAfter = l.Period
		}
	}
	return res, nil
}

// state fills d's remaining and reset from l's count: for GCRA, a reset of
// count·T, as the script's TAT − t.
func (f *fakeRedis) state(l rlredis.Limit, d *ratelimit.Decision) {
	d.Remaining = capacity(l) - f.used[l.Key]
	d.ResetAfter = l.Period
	if l.Algorithm == config.GCRA {
		d.ResetAfter = time.Duration(f.used[l.Key]*(l.Period.Microseconds()+l.Rate-1)/l.Rate) * time.Microsecond
	}
}

func (f *fakeRedis) Refund(_ context.Context, cost, admittedUS int64, limits []rlredis.Limit) (rlredis.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := fmt.Sprint("cost ", cost, " at ", admittedUS)
	if f.failRefunds {
		f.refunds = append(f.refunds, line+" failed")
		return rlredis.Result{}, &rlredis.Error{Kind: rlredis.KindConn, Node: "redis-1:6379", Err: errors.New("connection refused")}
	}
	res := rlredis.Result{Allowed: true, DenyingLimit: -1, NowUS: f.now, Limits: make([]ratelimit.Decision, len(limits))}
	for i, l := range limits {
		line += " | " + l.Key
		f.used[l.Key] = max(0, f.used[l.Key]-cost)
		res.Limits[i].Allowed = true
		f.state(l, &res.Limits[i])
	}
	f.refunds = append(f.refunds, line)
	return res, nil
}

// take returns the calls made since the last take.
func (f *fakeRedis) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

// takeRefunds waits until n refunds have arrived, refunds being
// asynchronous, and returns them; then no more may arrive.
func (f *fakeRedis) takeRefunds(t *testing.T, n int) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		f.mu.Lock()
		got := len(f.refunds)
		f.mu.Unlock()
		if got >= n || time.Now().After(deadline) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	refunds := f.refunds
	f.refunds = nil
	if len(refunds) != n {
		t.Errorf("%d refunds, want %d:\n%s", len(refunds), n, strings.Join(refunds, "\n"))
	}
	return refunds
}

// setFail makes calls in hash tag tag fail, or answer again.
func (f *fakeRedis) setFail(tag string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fail {
		f.fail[tag] = struct{}{}
	} else {
		delete(f.fail, tag)
	}
}

func (f *fakeRedis) usedOf(key string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used[key]
}

func (f *fakeRedis) setUsed(key string, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used[key] = n
}

// redisExample is ADR-0021's example config (its Example section).
const redisExample = `
ratelimit: {replicas: 3, redis_budget: 5ms}
policies:
  - {name: per-ip-flood, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 600, period: 1s}], local: true}
  - {name: delay-inflight, level: route, match: {routes: [delay]}, key_template: '{route}', algorithm: concurrency, limits: [{rate: 20}]}
  - name: tenant-default
    level: tenant
    match: {routes: ['*']}
    key_template: '{tenant}'
    algorithm: gcra
    limits: [{rate: 100, period: 1s, burst: 200}, {rate: 10000, period: 1h, burst: 500}]
    failure_mode: degraded
  - {name: consumer-minute, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: sliding_window, limits: [{rate: 1000, period: 1m}], failure_mode: fail_open}
  - {name: anything-writes, level: route, match: {routes: [anything], methods: [POST, PUT, PATCH, DELETE]}, key_template: '{route}:{ip}', algorithm: gcra, limits: [{rate: 50, period: 1s}], failure_mode: fail_closed}
  - {name: consumer-shadow, level: consumer, match: {routes: [bytes], methods: [GET]}, key_template: '{consumer}:{route}', algorithm: gcra, limits: [{rate: 5, period: 1s}], mode: shadow, failure_mode: fail_closed}
  - {name: global-bytes, level: global, match: {routes: [bytes]}, key_template: '{route}', algorithm: gcra, limits: [{rate: 2000, period: 1s}], failure_mode: fail_open}
  - {name: global-anything, level: global, match: {routes: [anything]}, key_template: '{route}', algorithm: sliding_window, limits: [{rate: 6000, period: 1m}], failure_mode: degraded}
`

// TestRedisCalls checks ADR-0021's example: which Redis policies apply to a
// request, how they are grouped into calls by hash slot, in which order, and
// with which keys and limits (RL-05, RL-06, RL-07, RL-13, RL-14). Redis
// limits carry the configured, cluster-wide numbers.
func TestRedisCalls(t *testing.T) {
	u := newUpstream(t, "httpbin")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), exampleRoutes(u)+redisExample, nil, f)
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey, "bob": bobKey}))
	alice, bob := http.Header{"X-Api-Key": {aliceKey}}, http.Header{"X-Api-Key": {bobKey}}

	for _, tt := range []struct {
		name, method, path string
		header             http.Header
		calls              []string
	}{
		{
			// One tenant-slot call with every tenant-, consumer- and
			// route-level policy, then the global one.
			"alice GET /bytes/1", http.MethodGet, "/bytes/1", alice, []string{
				"cost 1" +
					" | rl:v1:{t:acme}:tenant-default:1000:giszrYfBSKCiClunzV68qg tenant enforce gcra 100/1s burst 200" +
					" | rl:v1:{t:acme}:tenant-default:3600000:giszrYfBSKCiClunzV68qg tenant enforce gcra 10000/1h0m0s burst 500" +
					" | rl:v1:{t:acme}:consumer-minute:60000:K9gGyX8OAK8aH8Myj6djqQ consumer enforce sliding_window 1000/1m0s burst 0" +
					" | rl:v1:{t:acme}:consumer-shadow:1000:" + rlredis.Digest("alice:bytes") + " consumer shadow gcra 5/1s burst 5",
				"cost 1 | rl:v1:{g:global-bytes}:1000:J3CJ2RwL308uaGK6fkoHYA global enforce gcra 2000/1s burst 2000",
			},
		},
		{
			// No tenant: tenant-default does not apply ({tenant} is
			// unfilled), and each digest is its own {k:} call.
			"bob GET /bytes/1", http.MethodGet, "/bytes/1", bob, []string{
				"cost 1 | rl:v1:{k:" + rlredis.Digest("bob") + "}:consumer-minute:60000 consumer enforce sliding_window 1000/1m0s burst 0",
				"cost 1 | rl:v1:{k:" + rlredis.Digest("bob:bytes") + "}:consumer-shadow:1000 consumer shadow gcra 5/1s burst 5",
				"cost 1 | rl:v1:{g:global-bytes}:1000:J3CJ2RwL308uaGK6fkoHYA global enforce gcra 2000/1s burst 2000",
			},
		},
		{
			// Anonymous, cost 5 (RL-07).
			"anonymous POST /anything", http.MethodPost, "/anything", nil, []string{
				"cost 5 | rl:v1:{k:" + rlredis.Digest("anything:127.0.0.1") + "}:anything-writes:1000 route enforce gcra 50/1s burst 50",
				"cost 5 | rl:v1:{g:global-anything}:60000:" + rlredis.Digest("anything") + " global enforce sliding_window 6000/1m0s burst 0",
			},
		},
		{
			// Only local policies apply: no Redis call at all (RL-17).
			"anonymous GET /delay/1", http.MethodGet, "/delay/1", nil, nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := g.send(t, tt.method, tt.path, tt.header); got.status != http.StatusOK {
				t.Fatalf("status %d", got.status)
			}
			if got := f.take(); strings.Join(got, "\n") != strings.Join(tt.calls, "\n") {
				t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.calls, "\n"))
			}
		})
	}
}

// TestRedisSharedDigest: without a tenant, policies whose templates fill to
// the same string share one {k:} call, in config order (ADR-0021).
func TestRedisSharedDigest(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+`
policies:
  - {name: a, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 10, period: 1s}]}
  - {name: b, level: consumer, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 10, period: 1s}]}
  - {name: c, level: tenant, match: {routes: ['*']}, key_template: '{ip}', algorithm: sliding_window, limits: [{rate: 10, period: 1m}]}
`, nil, f)
	g.send(t, http.MethodGet, "/", nil)
	ip, route := rlredis.Digest("127.0.0.1"), rlredis.Digest("r")
	want := []string{
		"cost 1 | rl:v1:{k:" + ip + "}:a:1000 route enforce gcra 10/1s burst 10 | rl:v1:{k:" + ip + "}:c:60000 tenant enforce sliding_window 10/1m0s burst 0",
		"cost 1 | rl:v1:{k:" + route + "}:b:1000 consumer enforce gcra 10/1s burst 10",
	}
	if got := f.take(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// globalConfig has a local enforce and a local shadow policy, a tenant-level
// and a shadow consumer-level Redis policy in the tenant slot, and a global
// Redis policy.
const globalConfig = `
policies:
  - {name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 10, period: 1s}], local: true}
  - {name: per-ip-shadow, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 10, period: 1s}], mode: shadow, local: true}
  - {name: tenant, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 3, period: 1s}]}
  - {name: consumer-shadow, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, limits: [{rate: 1, period: 1s}], mode: shadow}
  - {name: global, level: global, match: {routes: ['*']}, key_template: '{route}', algorithm: sliding_window, limits: [{rate: 2, period: 1s}]}
`

// TestRedisGlobalAfterTenant: the global level is called only after the
// tenant-slot call allows; when it denies, the request is answered 429 and
// gives back everything it took: the tenant-slot limits it wrote (enforce,
// and shadow ones that would allow), and its local deductions (RL-05,
// RL-14, ADR-0021). The fields list the local limits, then the Redis ones in
// call order, as the client's state stands after the refunds (ADR-0018).
func TestRedisGlobalAfterTenant(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+globalConfig, nil, f)
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
	alice := func(id string) http.Header { return http.Header{"X-Api-Key": {aliceKey}, "X-Request-Id": {id}} }
	tenantKey := rlredis.TenantKey("acme", "tenant", time.Second, rlredis.Digest("acme"))
	shadowKey := rlredis.TenantKey("acme", "consumer-shadow", time.Second, rlredis.Digest("alice"))
	globalKey := rlredis.GlobalKey("global", time.Second, rlredis.Digest("r"))
	const policy = `"per-ip";q=10;w=1, "tenant";q=3;w=1, "global";q=2;w=1`

	for i, want := range []struct {
		status    int
		rateLimit string
		calls     int
		decisions string
	}{
		// T = 333,334 µs for tenant; the fake's reset is count·T.
		{200, `"per-ip";r=9;t=1, "tenant";r=2;t=1, "global";r=1;t=1`, 2,
			`[{"decision":"allow","mode":"enforce","policy":"per-ip"},{"decision":"allow","mode":"shadow","policy":"per-ip-shadow"},{"decision":"allow","mode":"enforce","policy":"tenant"},{"decision":"allow","mode":"shadow","policy":"consumer-shadow"},{"decision":"allow","mode":"enforce","policy":"global"}]`},
		// consumer-shadow would deny: recorded, never rejects, not written.
		{200, `"per-ip";r=8;t=1, "tenant";r=1;t=1, "global";r=0;t=1`, 2,
			`[{"decision":"allow","mode":"enforce","policy":"per-ip"},{"decision":"allow","mode":"shadow","policy":"per-ip-shadow"},{"decision":"allow","mode":"enforce","policy":"tenant"},{"decision":"deny","mode":"shadow","policy":"consumer-shadow"},{"decision":"allow","mode":"enforce","policy":"global"}]`},
		// global denies: tenant shows its unit back, per-ip too.
		{429, `"per-ip";r=8;t=1, "tenant";r=1;t=1, "global";r=0;t=1`, 2,
			`[{"decision":"allow","mode":"enforce","policy":"per-ip"},{"decision":"allow","mode":"shadow","policy":"per-ip-shadow"},{"decision":"allow","mode":"enforce","policy":"tenant"},{"decision":"deny","mode":"shadow","policy":"consumer-shadow"},{"decision":"deny","mode":"enforce","policy":"global"}]`},
	} {
		id := fmt.Sprint("global-", i)
		got := g.send(t, http.MethodGet, "/", alice(id))
		if got.status != want.status || got.header.Get("RateLimit-Policy") != policy || got.header.Get("RateLimit") != want.rateLimit {
			t.Errorf("request %d: %d\n%s\nwant %d with RateLimit %q", i+1, got.status, rateLimitFields(got.header), want.status, want.rateLimit)
		}
		if calls := f.take(); len(calls) != want.calls {
			t.Errorf("request %d: %d calls, want %d", i+1, len(calls), want.calls)
		}
		if line := jsonOf(t, g.logs.access(t, id)["decisions"]); line != want.decisions {
			t.Errorf("request %d: decisions %s\nwant %s", i+1, line, want.decisions)
		}
		if want.status == http.StatusTooManyRequests && (got.header.Get("Retry-After") != "1" || got.body != `{"error":"rate limited"}`) {
			t.Errorf("request %d: Retry-After %q, body %q", i+1, got.header.Get("Retry-After"), got.body)
		}
	}
	// The third request's tenant-slot call wrote tenant only, so only
	// tenant is refunded, at that call's time.
	if refunds := f.takeRefunds(t, 1); len(refunds) == 1 && refunds[0] != "cost 1 at 1005000 | "+tenantKey {
		t.Errorf("refund %q, want tenant's at 1005000", refunds[0])
	}
	if f.usedOf(tenantKey) != 2 || f.usedOf(shadowKey) != 1 || f.usedOf(globalKey) != 2 {
		t.Errorf("tenant %d, consumer-shadow %d, global %d used; want 2, 1, 2", f.usedOf(tenantKey), f.usedOf(shadowKey), f.usedOf(globalKey))
	}
	for _, policy := range []string{"per-ip", "per-ip-shadow"} {
		if d := peek(g.limiter(t, policy, config.GCRA, time.Second), "127.0.0.1"); d.Remaining != 8 {
			t.Errorf("%s has %d left, want 8: the denied request was refunded", policy, d.Remaining)
		}
	}

	// A tenant-slot deny ends the request before the global call, and
	// needs no Redis refund: a denied call writes nothing.
	f.setUsed(tenantKey, 3)
	got := g.send(t, http.MethodGet, "/", alice("global-3"))
	if got.status != http.StatusTooManyRequests || got.header.Get("RateLimit") != `"per-ip";r=8;t=1, "tenant";r=0;t=2` ||
		got.header.Get("RateLimit-Policy") != `"per-ip";q=10;w=1, "tenant";q=3;w=1` {
		t.Errorf("tenant deny: %d\n%s", got.status, rateLimitFields(got.header))
	}
	if calls := f.take(); len(calls) != 1 || !strings.Contains(calls[0], tenantKey) {
		t.Errorf("tenant deny: calls %q, want the tenant-slot call only", calls)
	}
	f.takeRefunds(t, 0)
	if line, want := jsonOf(t, g.logs.access(t, "global-3")["decisions"]),
		`[{"decision":"allow","mode":"enforce","policy":"per-ip"},{"decision":"allow","mode":"shadow","policy":"per-ip-shadow"},{"decision":"deny","mode":"enforce","policy":"tenant"},{"decision":"deny","mode":"shadow","policy":"consumer-shadow"}]`; line != want {
		t.Errorf("tenant deny: decisions %s\nwant %s", line, want)
	}

	m := scrape(t, g.metrics)
	for _, s := range []string{
		`gw_ratelimit_decisions_total{decision="allow",mode="enforce",policy="tenant"} 3`,
		`gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="tenant"} 1`,
		`gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="global"} 1`,
		`gw_ratelimit_decisions_total{decision="deny",mode="shadow",policy="consumer-shadow"} 3`,
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="ratelimited",route="r",tenant="none"} 2`,
	} {
		if !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}
}

// TestRedisLocalDenyFirst: a request a local policy denies is answered
// before any Redis call (ADR-0021).
func TestRedisLocalDenyFirst(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+`
policies:
  - {name: once, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: fixed_window, limits: [{rate: 1, period: 1h}], local: true}
  - {name: shared, level: route, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 10, period: 1s}]}
`, nil, f)
	if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
		t.Errorf("first request: %d, want 200", got.status)
	}
	f.take()
	if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusTooManyRequests {
		t.Errorf("second request: %d, want 429", got.status)
	}
	if calls := f.take(); len(calls) != 0 {
		t.Errorf("a locally denied request made Redis calls: %q", calls)
	}
}

// TestRedisCallFails: a failed call allows every fail_open policy in it, the
// default failure mode, recorded as fail_open and left out of the fields,
// and the next call still runs (RL-23, ADR-0021).
func TestRedisCallFails(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	f.fail["{t:acme}"] = struct{}{}
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+globalConfig, nil, f)
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
	got := g.send(t, http.MethodGet, "/", http.Header{"X-Api-Key": {aliceKey}, "X-Request-Id": {"fails"}})
	if got.status != http.StatusOK || got.header.Get("RateLimit-Policy") != `"per-ip";q=10;w=1, "global";q=2;w=1` || got.header.Get("RateLimit") != `"per-ip";r=9;t=1, "global";r=1;t=1` {
		t.Errorf("%d\n%s", got.status, rateLimitFields(got.header))
	}
	if calls := f.take(); len(calls) != 2 {
		t.Errorf("%d calls, want the failed tenant-slot call and the global one", len(calls))
	}
	if line, want := jsonOf(t, g.logs.access(t, "fails")["decisions"]),
		`[{"decision":"allow","mode":"enforce","policy":"per-ip"},{"decision":"allow","mode":"shadow","policy":"per-ip-shadow"},{"decision":"fail_open","mode":"enforce","policy":"tenant"},{"decision":"fail_open","mode":"shadow","policy":"consumer-shadow"},{"decision":"allow","mode":"enforce","policy":"global"}]`; line != want {
		t.Errorf("decisions %s\nwant %s", line, want)
	}
	if m := scrape(t, g.metrics); !strings.Contains(m, "\n"+`gw_ratelimit_decisions_total{decision="fail_open",mode="enforce",policy="tenant"} 1`+"\n") {
		t.Error("/metrics has no fail_open decision for tenant")
	}
}

// TestDecisionTime: in dev mode, a response whose Redis calls all answered
// carries the now_us of the last one, on a 2xx and a 429 alike; a request
// with a failed call or without Redis calls gets none, nor does any response
// with dev mode off, even when something upstream of the recorder set one
// (ADR-0021).
func TestDecisionTime(t *testing.T) {
	u := newUpstream(t, "orders")
	cfg := fmt.Sprintf(`
upstreams: [{name: orders, endpoints: [{address: 'orders:%s'}]}]
routes:
  - {name: r, upstream: orders, match: {path_prefix: /}, response_headers: {set: [{name: spillway-decision-time, value: '1'}]}}
policies:
  - {name: local, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}
  - {name: tenant, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 2, period: 1s}], failure_mode: fail_open}
  - {name: global, level: global, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], failure_mode: fail_open}
`, u.port())
	alice := http.Header{"X-Api-Key": {aliceKey}}
	tenantKey := rlredis.TenantKey("acme", "tenant", time.Second, rlredis.Digest("acme"))

	t.Run("on", func(t *testing.T) {
		f := newFakeRedis()
		g := newDevGateway(t, newResolver(u), cfg, nil, f, true)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		for _, tt := range []struct {
			name   string
			header http.Header
			setup  func()
			status int
			want   string
		}{
			// The fake's TIME moves 1 ms per call: the tenant-slot call at
			// 1001000, the global one at 1002000.
			{"allowed", alice, func() {}, http.StatusOK, "1002000"},
			// The tenant-slot call denies and is the last that ran.
			{"denied", alice, func() { f.setUsed(tenantKey, 2) }, http.StatusTooManyRequests, "1003000"},
			{"failed call", alice, func() { f.setUsed(tenantKey, 0); f.setFail("{t:acme}", true) }, http.StatusOK, ""},
			// Anonymous: no tenant, and {route} fills, so only global runs.
			{"one call", nil, func() { f.setFail("{t:acme}", false) }, http.StatusOK, "1005000"},
		} {
			tt.setup()
			got := g.send(t, http.MethodGet, "/", tt.header)
			if got.status != tt.status || got.header.Get("Spillway-Decision-Time") != tt.want || len(got.header.Values("Spillway-Decision-Time")) > 1 {
				t.Errorf("%s: %d with Spillway-Decision-Time %q, want %d with %q", tt.name, got.status, got.header.Values("Spillway-Decision-Time"), tt.status, tt.want)
			}
		}

		// Only local policies apply: no Redis call, no header.
		g.apply(t, oneUpstream(u)+`
policies:
  - {name: local, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}
`)
		if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK || got.header.Get("Spillway-Decision-Time") != "" {
			t.Errorf("local only: %d with Spillway-Decision-Time %q, want 200 with none", got.status, got.header.Get("Spillway-Decision-Time"))
		}
	})

	t.Run("off", func(t *testing.T) {
		f := newFakeRedis()
		g := newDevGateway(t, newResolver(u), cfg, nil, f, false)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		if got := g.send(t, http.MethodGet, "/", alice); got.status != http.StatusOK || len(f.take()) != 2 || got.header.Values("Spillway-Decision-Time") != nil {
			t.Errorf("%d with Spillway-Decision-Time %q, want 200 with none", got.status, got.header.Values("Spillway-Decision-Time"))
		}
	})
}
