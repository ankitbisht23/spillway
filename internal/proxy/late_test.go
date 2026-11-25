package proxy

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// lateDelay is how long after it arrives a late call is decided: well after
// the tests' 20 ms budget, so the request has its answer first.
const lateDelay = 100 * time.Millisecond

// waitUsed waits until key holds n units in f.
func waitUsed(t *testing.T, f *fakeRedis, key string, n int64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); f.usedOf(key) != n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %d, want %d", key, f.usedOf(key), n)
		}
	}
}

// waitMetrics waits until g's /metrics has every line of want.
func waitMetrics(t *testing.T, g *gateway, want ...string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		m, missing := scrape(t, g.metrics), false
		for _, s := range want {
			missing = missing || !strings.Contains(m, "\n"+s+"\n")
		}
		if !missing || time.Now().After(deadline) {
			break
		}
	}
	hasMetrics(t, g, want...)
}

// TestLateRefund: a call that times out while in flight answers its request
// by its failure modes at the budget, as before. If the request ends there
// (a fail_closed 503 or a degraded deny) and the late reply admitted it,
// what the reply wrote is refunded in the background, once, as of its
// now_us; a request that was let through keeps its late admission
// (RL-23, ADR-0079).
func TestLateRefund(t *testing.T) {
	u := newUpstream(t, "orders")
	alice := http.Header{"X-Api-Key": {aliceKey}}
	tenantKey := rlredis.TenantKey("acme", "tenant", time.Second, rlredis.Digest("acme"))
	globalKey := rlredis.GlobalKey("global", time.Second, rlredis.Digest("r"))
	closedCfg := oneUpstream(u) + `
ratelimit: {redis_budget: 20ms}
policies:
  - {name: tenant, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 10, period: 1s}]}
  - {name: global, level: global, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 10, period: 1s}], failure_mode: fail_closed}
`
	lateGateway := func(t *testing.T, cfg, tag string) (*gateway, *fakeRedis) {
		t.Helper()
		f := newFakeRedis()
		f.slow[tag], f.late[tag] = lateDelay, true
		g := newGatewayWith(t, newResolver(u), cfg, nil, f)
		g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
		return g, f
	}

	t.Run("fail_closed", func(t *testing.T) {
		// The tenant call admits at once; the global call times out, so the
		// request is a 503. The tenant call is refunded as before, and the
		// global call's late admission once its reply comes: two refunds,
		// one per call.
		g, f := lateGateway(t, closedCfg, "{g:")
		if got := g.send(t, http.MethodGet, "/", alice); got.status != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503 from global", got.status)
		}
		refunds := f.takeRefunds(t, 2)
		slices.Sort(refunds) // both are asynchronous
		want := []string{"cost 1 at 1001000 | " + tenantKey, "cost 1 at 1002000 | " + globalKey}
		if strings.Join(refunds, "\n") != strings.Join(want, "\n") {
			t.Errorf("refunds:\n%s\nwant:\n%s", strings.Join(refunds, "\n"), strings.Join(want, "\n"))
		}
		if f.usedOf(tenantKey) != 0 || f.usedOf(globalKey) != 0 {
			t.Errorf("tenant holds %d, global %d; want both refunded", f.usedOf(tenantKey), f.usedOf(globalKey))
		}
		waitMetrics(t, g,
			`gw_ratelimit_late_refunds_total{result="refunded"} 1`,
			`gw_ratelimit_late_refunds_total{result="failed"} 0`,
			`gw_ratelimit_refund_failures_total 0`,
			`gw_redis_errors_total{kind="timeout"} 1`,
		)
	})

	t.Run("degraded", func(t *testing.T) {
		// Redis admits 2, each replica 1 while degraded. The first request
		// is admitted by its degraded limiter: a 2xx keeps its late
		// admission. The second is denied by it: its late admission is
		// refunded.
		cfg := oneUpstream(u) + `
ratelimit: {redis_budget: 20ms, replicas: 2}
policies:
  - {name: deg, level: route, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 2, period: 1h, burst: 2}], failure_mode: degraded}
`
		key := rlredis.NoTenantKey(rlredis.Digest("r"), "deg", time.Hour)
		g, f := lateGateway(t, cfg, "{k:")
		if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
			t.Fatalf("first: %d, want 200 from the degraded limiter", got.status)
		}
		waitUsed(t, f, key, 1)            // the late reply admitted it
		time.Sleep(50 * time.Millisecond) // time for a refund that must not come
		f.takeRefunds(t, 0)
		if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusTooManyRequests {
			t.Fatalf("second: %d, want 429 from the degraded limiter", got.status)
		}
		if refunds := f.takeRefunds(t, 1); len(refunds) == 1 && refunds[0] != "cost 1 at 1002000 | "+key {
			t.Errorf("refund %q, want the second request's late admission", refunds[0])
		}
		if n := f.usedOf(key); n != 1 {
			t.Errorf("Redis holds %d, want 1: the 2xx's admission", n)
		}
		waitMetrics(t, g, `gw_ratelimit_late_refunds_total{result="refunded"} 1`)
	})

	t.Run("not admitted", func(t *testing.T) {
		// A late reply that denies wrote nothing; one that fails says
		// nothing about what ran. Neither is refunded.
		g, f := lateGateway(t, closedCfg, "{g:")
		f.setUsed(globalKey, 10)
		if got := g.send(t, http.MethodGet, "/", alice); got.status != http.StatusServiceUnavailable {
			t.Fatalf("denied late: %d, want 503", got.status)
		}
		f.takeRefunds(t, 1) // the tenant call's
		f.setFail("{g:", true)
		if got := g.send(t, http.MethodGet, "/", alice); got.status != http.StatusServiceUnavailable {
			t.Fatalf("failed late: %d, want 503", got.status)
		}
		f.takeRefunds(t, 1)                         // the tenant call's
		time.Sleep(lateDelay + 50*time.Millisecond) // both late replies are in
		f.takeRefunds(t, 0)
		if n := f.usedOf(globalKey); n != 10 {
			t.Errorf("global holds %d, want 10", n)
		}
		hasMetrics(t, g, `gw_ratelimit_late_refunds_total{result="refunded"} 0`)
	})

	t.Run("failure", func(t *testing.T) {
		// A late refund that fails is counted and dropped, never retried.
		g, f := lateGateway(t, closedCfg, "{g:")
		f.failRefunds = true
		if got := g.send(t, http.MethodGet, "/", alice); got.status != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", got.status)
		}
		f.takeRefunds(t, 2) // the tenant call's and the late one, both failed
		waitMetrics(t, g,
			`gw_ratelimit_late_refunds_total{result="failed"} 1`,
			`gw_ratelimit_late_refunds_total{result="refunded"} 0`,
			`gw_ratelimit_refund_failures_total 2`,
			`gw_redis_errors_total{kind="conn"} 2`,
		)
		time.Sleep(50 * time.Millisecond)
		f.takeRefunds(t, 0)
	})

	t.Run("lease", func(t *testing.T) {
		// A late reservation's grant has no lease to go to: all of it is
		// refunded, lease_k 5, not the request's cost.
		tag := "{k:" + rlredis.Digest("127.0.0.1") + "}"
		g, f := lateGateway(t, oneUpstream(u)+"ratelimit: {redis_budget: 20ms}\n"+leasePolicy, tag)
		if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503 from the fail_closed batched policy", got.status)
		}
		if calls := f.take(); len(calls) != 1 || !strings.HasSuffix(calls[0], "lease 5") {
			t.Errorf("calls %q, want one reservation", calls)
		}
		if refunds := f.takeRefunds(t, 1); len(refunds) == 1 && refunds[0] != fmt.Sprint("cost 5 at 1001000 | ", leaseKey) {
			t.Errorf("refund %q, want the grant of 5", refunds[0])
		}
		if n := f.usedOf(leaseKey); n != 0 {
			t.Errorf("Redis holds %d, want 0", n)
		}
		hasMetrics(t, g, `gw_ratelimit_lease_total{result="error"} 1`, `gw_ratelimit_late_refunds_total{result="refunded"} 1`)
	})
}
