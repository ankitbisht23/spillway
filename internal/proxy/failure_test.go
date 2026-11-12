package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// degraded returns g's degraded limiter for a Redis policy limit.
func (g *gateway) degraded(t *testing.T, policy, algorithm string, period time.Duration) ratelimit.Limiter {
	t.Helper()
	l := g.limiters.Limiter(local.ID{Policy: policy, Algorithm: algorithm, Period: period, Degraded: true})
	if l == nil {
		t.Fatalf("no degraded limiter for %s|%s|%v", policy, algorithm, period)
	}
	return l
}

// hasMetrics reports every line of want missing from g's /metrics.
func hasMetrics(t *testing.T, g *gateway, want ...string) {
	t.Helper()
	m := scrape(t, g.metrics)
	for _, s := range want {
		if !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}
}

// TestRedisOutage checks ADR-0021's example during a Redis outage: alice's
// GET /bytes/1 is limited by tenant-default's degraded limiters at
// limit ÷ 3 replicas, consumer-minute and global-bytes allow, and
// consumer-shadow records fail_closed without effect; a POST /anything gets
// 503 from anything-writes. A request a failure mode ends gives back what
// it took (RL-23, RL-31).
func TestRedisOutage(t *testing.T) {
	u := newUpstream(t, "httpbin")
	f := newFakeRedis()
	for _, tag := range []string{"{t:", "{k:", "{g:"} {
		f.setFail(tag, true)
	}
	g := newGatewayWith(t, newResolver(u), exampleRoutes(u)+redisExample, nil, f)
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
	alice := func(id string) http.Header { return http.Header{"X-Api-Key": {aliceKey}, "X-Request-Id": {id}} }
	const policy = `"per-ip-flood";q=200;w=1, "tenant-default/1s";q=33;w=1, "tenant-default/3600s";q=3333;w=3600`
	degradedDecisions := func(tenant string) string {
		return `[{"decision":"allow","mode":"enforce","policy":"per-ip-flood"},{"decision":"` + tenant + `","mode":"enforce","policy":"tenant-default"},` +
			`{"decision":"fail_open","mode":"enforce","policy":"consumer-minute"},{"decision":"fail_closed","mode":"shadow","policy":"consumer-shadow"}`
	}

	// 66 admitted: the per-replica burst of the 1 s limit.
	for i := range 66 {
		got := g.send(t, http.MethodGet, "/bytes/1", alice(fmt.Sprint("outage-", i)))
		if got.status != http.StatusOK || got.header.Get("RateLimit-Policy") != policy || !strings.Contains(got.header.Get("RateLimit"), fmt.Sprintf(`"tenant-default/1s";r=%d;`, 65-i)) {
			t.Fatalf("request %d: %d\n%s", i+1, got.status, rateLimitFields(got.header))
		}
	}
	if line, want := jsonOf(t, g.logs.access(t, "outage-0")["decisions"]), degradedDecisions("allow")+`,{"decision":"fail_open","mode":"enforce","policy":"global-bytes"}]`; line != want {
		t.Errorf("decisions %s\nwant %s", line, want)
	}
	ip := peek(g.limiter(t, "per-ip-flood", config.GCRA, time.Second), "127.0.0.1").Remaining
	hour := g.degraded(t, "tenant-default", config.GCRA, time.Hour)
	hourKey := rlredis.TenantKey("acme", "tenant-default", time.Hour, rlredis.Digest("acme"))

	// The 67th is denied by the degraded 1 s limit, before the global call;
	// its local and degraded deductions are given back.
	got := g.send(t, http.MethodGet, "/bytes/1", alice("outage-deny"))
	if got.status != http.StatusTooManyRequests || got.header.Get("Retry-After") != "1" || got.header.Get("RateLimit-Policy") != policy || got.body != `{"error":"rate limited"}` {
		t.Errorf("67th request: %d %q\n%s", got.status, got.body, rateLimitFields(got.header))
	}
	if line, want := jsonOf(t, g.logs.access(t, "outage-deny")["decisions"]), degradedDecisions("deny")+`]`; line != want {
		t.Errorf("decisions %s\nwant %s", line, want)
	}
	if d := peek(g.limiter(t, "per-ip-flood", config.GCRA, time.Second), "127.0.0.1"); d.Remaining != ip {
		t.Errorf("per-ip-flood has %d left, want %d: the denied request was refunded", d.Remaining, ip)
	}
	if d := peek(hour, hourKey); d.Remaining != 166-66 {
		t.Errorf("tenant-default's degraded hour limit has %d left, want 100", d.Remaining)
	}

	// anything-writes is enforce and fail_closed: 503, and global-anything's
	// call is never sent.
	f.take()
	got = g.send(t, http.MethodPost, "/anything", http.Header{"X-Request-Id": {"outage-503"}})
	if got.status != http.StatusServiceUnavailable || got.body != `{"error":"rate limit unavailable"}` || got.header.Get("Retry-After") != "1" ||
		got.header.Get("Content-Type") != "application/json" || got.header.Get("RateLimit-Policy") != `"per-ip-flood";q=200;w=1` {
		t.Errorf("POST /anything: %d %q\n%s", got.status, got.body, rateLimitFields(got.header))
	}
	if calls := f.take(); len(calls) != 1 || !strings.Contains(calls[0], "anything-writes") {
		t.Errorf("calls %q, want anything-writes' only", calls)
	}
	if line, want := jsonOf(t, g.logs.access(t, "outage-503")["decisions"]),
		`[{"decision":"allow","mode":"enforce","policy":"per-ip-flood"},{"decision":"fail_closed","mode":"enforce","policy":"anything-writes"}]`; line != want {
		t.Errorf("decisions %s\nwant %s", line, want)
	}
	if d := peek(g.limiter(t, "per-ip-flood", config.GCRA, time.Second), "127.0.0.1"); d.Remaining != ip {
		t.Errorf("per-ip-flood has %d left after the 503, want %d", d.Remaining, ip)
	}
	f.takeRefunds(t, 0)

	hasMetrics(t, g,
		`gw_requests_total{code_class="5xx",method="POST",origin="gateway",reason="fail_closed",route="anything",tenant="none"} 1`,
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="ratelimited",route="bytes",tenant="none"} 1`,
		`gw_ratelimit_decisions_total{decision="allow",mode="enforce",policy="tenant-default"} 66`,
		`gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="tenant-default"} 1`,
		`gw_ratelimit_decisions_total{decision="fail_open",mode="enforce",policy="global-bytes"} 66`,
		`gw_ratelimit_decisions_total{decision="fail_closed",mode="shadow",policy="consumer-shadow"} 67`,
		`gw_ratelimit_decisions_total{decision="fail_closed",mode="enforce",policy="anything-writes"} 1`,
		`gw_redis_errors_total{kind="conn"} 134`, // 66 × 2 calls, the 67th's first, the POST's first
		`gw_redis_errors_total{kind="script"} 0`,
		`gw_redis_errors_total{kind="timeout"} 0`,
		`gw_ratelimit_check_duration_seconds_count{backend="redis"} 68`,
	)
}

// precedenceConfig puts every failure mode in alice's tenant-slot call: an
// enforce degraded policy that admits 1 per second per replica, an enforce
// fail_closed one unless closed is false, a shadow fail_closed and a shadow
// degraded one; then a global fail_open call.
func precedenceConfig(closed bool) string {
	mode := "enforce"
	if !closed {
		mode = "off"
	}
	return `
policies:
  - {name: deg, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 1, period: 1s}], failure_mode: degraded}
  - {name: closed, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, limits: [{rate: 100, period: 1s}], failure_mode: fail_closed, mode: ` + mode + `}
  - {name: shadow-closed, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, limits: [{rate: 100, period: 1s}], failure_mode: fail_closed, mode: shadow}
  - {name: shadow-deg, level: route, match: {routes: ['*']}, key_template: '{route}', algorithm: sliding_window, limits: [{rate: 1, period: 1m}], failure_mode: degraded, mode: shadow}
  - {name: open, level: global, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 100, period: 1s}]}
`
}

// TestRedisFailurePrecedence: within a failed call, an enforce fail_closed
// policy makes a 503 and nothing else in the call is evaluated; otherwise the
// degraded limiters run, and an enforce one that denies makes a 429; shadow
// policies never change the response (RL-23, ADR-0021).
func TestRedisFailurePrecedence(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	f.setFail("{t:acme}", true)
	f.setFail("{g:", true)
	alice := func(id string) http.Header { return http.Header{"X-Api-Key": {aliceKey}, "X-Request-Id": {id}} }

	t.Run("fail_closed first", func(t *testing.T) {
		g := newGatewayWith(t, newResolver(u), oneUpstream(u)+precedenceConfig(true), nil, f)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		for i := range 2 {
			id := fmt.Sprint("closed-", i)
			if got := g.send(t, http.MethodGet, "/", alice(id)); got.status != http.StatusServiceUnavailable || got.header.Get("RateLimit-Policy") != "" {
				t.Errorf("request %d: %d\n%s", i+1, got.status, rateLimitFields(got.header))
			}
			if line, want := jsonOf(t, g.logs.access(t, id)["decisions"]),
				`[{"decision":"fail_closed","mode":"enforce","policy":"closed"},{"decision":"fail_closed","mode":"shadow","policy":"shadow-closed"}]`; line != want {
				t.Errorf("request %d: decisions %s\nwant %s", i+1, line, want)
			}
		}
		// The degraded limiters were never evaluated.
		if d := peek(g.degraded(t, "deg", config.GCRA, time.Second), rlredis.TenantKey("acme", "deg", time.Second, rlredis.Digest("acme"))); d.Remaining != 1 {
			t.Errorf("deg's degraded limiter has %d left, want 1", d.Remaining)
		}
		if calls := f.take(); len(calls) != 2 {
			t.Errorf("%d calls, want the two tenant-slot calls only", len(calls))
		}
	})

	t.Run("degraded", func(t *testing.T) {
		g := newGatewayWith(t, newResolver(u), oneUpstream(u)+precedenceConfig(false), nil, f)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		f.take()
		for i, want := range []struct {
			status    int
			calls     int
			decisions string
		}{
			{http.StatusOK, 2, `[{"decision":"allow","mode":"enforce","policy":"deg"},{"decision":"fail_closed","mode":"shadow","policy":"shadow-closed"},{"decision":"allow","mode":"shadow","policy":"shadow-deg"},{"decision":"fail_open","mode":"enforce","policy":"open"}]`},
			{http.StatusTooManyRequests, 1, `[{"decision":"deny","mode":"enforce","policy":"deg"},{"decision":"fail_closed","mode":"shadow","policy":"shadow-closed"},{"decision":"deny","mode":"shadow","policy":"shadow-deg"}]`},
		} {
			id := fmt.Sprint("degraded-", i)
			got := g.send(t, http.MethodGet, "/", alice(id))
			if got.status != want.status || got.header.Get("RateLimit-Policy") != `"deg";q=1;w=1` {
				t.Errorf("request %d: %d\n%s", i+1, got.status, rateLimitFields(got.header))
			}
			if line := jsonOf(t, g.logs.access(t, id)["decisions"]); line != want.decisions {
				t.Errorf("request %d: decisions %s\nwant %s", i+1, line, want.decisions)
			}
			if calls := f.take(); len(calls) != want.calls {
				t.Errorf("request %d: %d calls, want %d", i+1, len(calls), want.calls)
			}
		}
	})

	t.Run("shadow only", func(t *testing.T) {
		g := newGatewayWith(t, newResolver(u), oneUpstream(u)+`
policies:
  - {name: shadow-closed, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 1, period: 1s}], failure_mode: fail_closed, mode: shadow}
  - {name: shadow-deg, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 1, period: 1s}], failure_mode: degraded, mode: shadow}
`, nil, f)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		for i := range 3 {
			if got := g.send(t, http.MethodGet, "/", alice(fmt.Sprint("shadow-", i))); got.status != http.StatusOK {
				t.Errorf("request %d: %d, want 200", i+1, got.status)
			}
		}
		hasMetrics(t, g,
			`gw_ratelimit_decisions_total{decision="fail_closed",mode="shadow",policy="shadow-closed"} 3`,
			`gw_ratelimit_decisions_total{decision="deny",mode="shadow",policy="shadow-deg"} 2`,
		)
	})

	t.Run("later call", func(t *testing.T) {
		// The tenant-slot call answers and writes; the global call fails
		// with an enforce fail_closed policy: 503, and the tenant-slot
		// deduction is refunded.
		f := newFakeRedis()
		f.setFail("{g:", true)
		g := newGatewayWith(t, newResolver(u), oneUpstream(u)+`
policies:
  - {name: tenant, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 10, period: 1s}], failure_mode: fail_closed}
  - {name: global, level: global, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 10, period: 1s}], failure_mode: fail_closed}
`, nil, f)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		got := g.send(t, http.MethodGet, "/", alice("later"))
		if got.status != http.StatusServiceUnavailable || got.header.Get("RateLimit") != `"tenant";r=10;t=0` {
			t.Errorf("%d\n%s", got.status, rateLimitFields(got.header))
		}
		tenantKey := rlredis.TenantKey("acme", "tenant", time.Second, rlredis.Digest("acme"))
		if refunds := f.takeRefunds(t, 1); len(refunds) == 1 && refunds[0] != "cost 1 at 1001000 | "+tenantKey {
			t.Errorf("refund %q, want tenant's", refunds[0])
		}
		if line, want := jsonOf(t, g.logs.access(t, "later")["decisions"]),
			`[{"decision":"allow","mode":"enforce","policy":"tenant"},{"decision":"fail_closed","mode":"enforce","policy":"global"}]`; line != want {
			t.Errorf("decisions %s\nwant %s", line, want)
		}
	})
}

// TestRedisBudget: a request's Redis calls share one budget from the start
// of the first (RL-22). A call that times out is not retried, a call the
// budget left no time for is not sent, and both follow their failure modes
// and count as timeouts.
func TestRedisBudget(t *testing.T) {
	u := newUpstream(t, "orders")
	alice := func(id string) http.Header { return http.Header{"X-Api-Key": {aliceKey}, "X-Request-Id": {id}} }
	const budget = 50 * time.Millisecond
	cfg := oneUpstream(u) + `
ratelimit: {redis_budget: 50ms}
policies:
  - {name: tenant, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 10, period: 1s}]}
  - {name: global, level: global, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 10, period: 1s}], failure_mode: fail_closed}
`

	t.Run("spent", func(t *testing.T) {
		f := newFakeRedis()
		f.slow["{t:acme}"] = time.Second
		g := newGatewayWith(t, newResolver(u), cfg, nil, f)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		start := time.Now()
		got := g.send(t, http.MethodGet, "/", alice("spent"))
		elapsed := time.Since(start)
		if got.status != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503 from global, whose call was never sent", got.status)
		}
		if elapsed < budget || elapsed > budget+time.Second/2 {
			t.Errorf("answered after %v, want just over the %v budget", elapsed, budget)
		}
		if calls := f.take(); len(calls) != 1 || !strings.Contains(calls[0], "{t:acme}") {
			t.Errorf("calls %q, want the tenant-slot call once", calls)
		}
		if line, want := jsonOf(t, g.logs.access(t, "spent")["decisions"]),
			`[{"decision":"fail_open","mode":"enforce","policy":"tenant"},{"decision":"fail_closed","mode":"enforce","policy":"global"}]`; line != want {
			t.Errorf("decisions %s\nwant %s", line, want)
		}
		hasMetrics(t, g,
			`gw_redis_errors_total{kind="timeout"} 2`,
			`gw_redis_errors_total{kind="conn"} 0`,
			`gw_ratelimit_check_duration_seconds_count{backend="redis"} 1`,
		)
	})

	t.Run("shared", func(t *testing.T) {
		// The first call takes 30 ms of the budget; the second gets the same
		// deadline, so 20 ms.
		f := newFakeRedis()
		f.slow["{t:acme}"] = 30 * time.Millisecond
		g := newGatewayWith(t, newResolver(u), cfg, nil, f)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		start := time.Now()
		if got := g.send(t, http.MethodGet, "/", alice("shared")); got.status != http.StatusOK {
			t.Errorf("status %d", got.status)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		// The budget starts before the first call arrives, and after the
		// request was sent: bounds that no host stall can move.
		if len(f.deadlines) != 2 || !f.deadlines[0].Equal(f.deadlines[1]) || f.deadlines[0].Sub(start) < budget || f.deadlines[0].Sub(f.arrivals[0]) > budget {
			t.Errorf("deadlines %v, want both one %v budget after the first call (sent after %v, arrived %v)", f.deadlines, budget, start, f.arrivals)
		}
	})
}

// TestRedisDegradedState: a degraded limiter keeps its state across reloads
// and across outages, so a second outage starts where the first left off
// (RL-23, GW-29, ADR-0021).
func TestRedisDegradedState(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	cfg := oneUpstream(u) + `
ratelimit: {replicas: 2}
policies:
  - {name: deg, level: route, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 4, period: 1h, burst: 4}], failure_mode: degraded}
`
	g := newGatewayWith(t, newResolver(u), cfg, nil, f)
	send := func(want int, policy string) {
		t.Helper()
		if got := g.send(t, http.MethodGet, "/", nil); got.status != want || got.header.Get("RateLimit-Policy") != policy {
			t.Errorf("%d with RateLimit-Policy %q, want %d with %q", got.status, got.header.Get("RateLimit-Policy"), want, policy)
		}
	}
	const perReplica, cluster = `"deg";q=2;w=3600`, `"deg";q=4;w=3600`

	f.setFail("{k:", true) // first outage: 2 per replica
	send(http.StatusOK, perReplica)
	send(http.StatusOK, perReplica)
	send(http.StatusTooManyRequests, perReplica)
	f.setFail("{k:", false) // Redis decides again
	send(http.StatusOK, cluster)
	g.apply(t, cfg) // a reload keeps the degraded state
	f.setFail("{k:", true)
	send(http.StatusTooManyRequests, perReplica)
}

// TestRedisRefundFailure: a Redis refund that fails is counted, not retried
// (ADR-0021).
func TestRedisRefundFailure(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	f.failRefunds = true
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+globalConfig, nil, f)
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
	f.setUsed(rlredis.GlobalKey("global", time.Second, rlredis.Digest("r")), 2)
	if got := g.send(t, http.MethodGet, "/", http.Header{"X-Api-Key": {aliceKey}}); got.status != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 from global", got.status)
	}
	f.takeRefunds(t, 1)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if strings.Contains(scrape(t, g.metrics), "\ngw_ratelimit_refund_failures_total 1\n") || time.Now().After(deadline) {
			break
		}
	}
	hasMetrics(t, g, `gw_ratelimit_refund_failures_total 1`, `gw_redis_errors_total{kind="conn"} 1`)
}
