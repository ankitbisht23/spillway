package proxy

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ankitbisht23/spillway/internal/config"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// leasePolicy is one batched enforce Redis policy per client IP: GCRA
// 100/s (the fake's capacity 100, T = 10 ms), lease_k 5.
const leasePolicy = `
policies:
  - {name: batched, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 100, period: 1s}], lease_k: 5, failure_mode: fail_closed}
`

var leaseKey = rlredis.NoTenantKey(rlredis.Digest("127.0.0.1"), "batched", time.Second)

// leaseTokens is the unspent tokens of h's lease for key, -1 for none.
func leaseTokens(h *Handler, key string) int64 {
	g := h.leases.cur.Load()
	if g == nil {
		return -1
	}
	s := g.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.m[key]; e != nil {
		return e.tokens
	}
	return -1
}

// TestLease: the first request reserves k in its call and keeps k − cost;
// the next ones are served from the lease without a Redis call until it
// cannot hold their cost, and then one reserves again. Served requests are
// allowed and recorded as such, with RateLimit's r the grant's remaining
// plus the lease's tokens left (RL-18, ADR-0069).
func TestLease(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+leasePolicy, nil, f)
	reserve := "cost 1 | " + leaseKey + " route enforce gcra 100/1s burst 100 lease 5"
	for i, want := range []struct {
		calls     []string
		rateLimit string
	}{
		{[]string{reserve}, `"batched";r=99;t=1`}, // granted 5: Redis has 95 left, the lease 4
		{nil, `"batched";r=98;t=1`},
		{nil, `"batched";r=97;t=1`},
		{nil, `"batched";r=96;t=1`},
		{nil, `"batched";r=95;t=1`},
		{[]string{reserve}, `"batched";r=94;t=1`}, // the lease is empty: granted 5 more
	} {
		got := g.send(t, http.MethodGet, "/", http.Header{"X-Request-Id": {fmt.Sprint("r", i+1)}})
		if got.status != http.StatusOK || got.header.Get("RateLimit") != want.rateLimit {
			t.Errorf("request %d: %d\n%s\nwant 200, RateLimit %q", i+1, got.status, rateLimitFields(got.header), want.rateLimit)
		}
		if calls := f.take(); !slices.Equal(calls, want.calls) {
			t.Errorf("request %d: calls %q, want %q", i+1, calls, want.calls)
		}
	}
	if got := f.usedOf(leaseKey); got != 10 {
		t.Errorf("Redis holds %d units, want the two grants of 5", got)
	}
	if got := jsonOf(t, g.logs.access(t, "r2")["decisions"]); got != `[{"decision":"allow","mode":"enforce","policy":"batched"}]` {
		t.Errorf("a served request's decisions: %s", got)
	}
	hasMetrics(t, g,
		`gw_ratelimit_lease_total{result="local"} 4`,
		`gw_ratelimit_lease_total{result="granted"} 2`,
		`gw_ratelimit_lease_total{result="partial"} 0`,
		`gw_ratelimit_lease_tokens_dropped_total{reason="reserve"} 0`,
		`gw_ratelimit_decisions_total{decision="allow",mode="enforce",policy="batched"} 6`,
		`gw_ratelimit_check_duration_seconds_count{backend="redis"} 2`, // only the requests that called Redis
	)
}

// TestLeaseExpiry: a lease serves for 1 s from its reservation's send time;
// then the next request reserves again and the tokens left are dropped
// (ADR-0069, "Lease life"). In dev mode a served request's decision time is
// the grant's Redis TIME plus the time since the grant arrived.
func TestLeaseExpiry(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	r := &clockRedis{Redis: f, d: 2 * time.Millisecond} // the call is sent 2 ms before its grant arrives
	g := newDevGateway(t, newResolver(u), oneUpstream(u)+leasePolicy, nil, r, true)
	r.clock = g.clock
	for i, want := range []struct {
		advance time.Duration
		calls   int
		time    string
	}{
		{0, 1, "1001000"},
		{300 * time.Millisecond, 0, "1301000"}, // 300 ms after the grant arrived
		{697 * time.Millisecond, 0, "1998000"}, // 999 ms after the send
		{time.Millisecond, 1, "1002000"},       // 1 s after the send: expired, reserves again
	} {
		g.clock.Advance(want.advance)
		got := g.send(t, http.MethodGet, "/", nil)
		if got.status != http.StatusOK || got.header.Get("Spillway-Decision-Time") != want.time {
			t.Errorf("request %d: %d, decision time %q; want 200, %q", i+1, got.status, got.header.Get("Spillway-Decision-Time"), want.time)
		}
		if calls := f.take(); len(calls) != want.calls {
			t.Errorf("request %d: %d calls, want %d", i+1, len(calls), want.calls)
		}
	}
	hasMetrics(t, g, `gw_ratelimit_lease_tokens_dropped_total{reason="expired"} 2`)
}

// TestLeasePartialAndDenied: a reservation gets what is left when that is
// less than k (partial); one that finds less than its cost is denied as a
// strict check would be, writes nothing, and its deny is cached (RL-19).
func TestLeasePartialAndDenied(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+leasePolicy, nil, f)
	f.setUsed(leaseKey, 97)
	for i, want := range []struct {
		status, calls int
	}{
		{http.StatusOK, 1},              // granted 3
		{http.StatusOK, 0},              // served: 1 left
		{http.StatusOK, 0},              // served: 0 left
		{http.StatusTooManyRequests, 1}, // reserves, denied
		{http.StatusTooManyRequests, 0}, // the deny cache
	} {
		if got := g.send(t, http.MethodGet, "/", nil); got.status != want.status {
			t.Errorf("request %d: %d, want %d", i+1, got.status, want.status)
		}
		if calls := f.take(); len(calls) != want.calls {
			t.Errorf("request %d: %d calls, want %d", i+1, len(calls), want.calls)
		}
	}
	if got := f.usedOf(leaseKey); got != 100 {
		t.Errorf("Redis holds %d, want 100", got)
	}
	hasMetrics(t, g,
		`gw_ratelimit_lease_total{result="partial"} 1`,
		`gw_ratelimit_lease_total{result="local"} 2`,
		`gw_ratelimit_lease_total{result="denied"} 1`,
		`gw_ratelimit_deny_cache_total{result="insert"} 1`,
		`gw_ratelimit_deny_cache_total{result="hit"} 1`,
	)
}

// TestLeaseReset: every config or identity swap clears the table, dropping
// its tokens; the next request reserves again (ADR-0069, "Generation").
func TestLeaseReset(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	cfg := func(v int) string { return fmt.Sprintf("version: %d\n", v) + oneUpstream(u) + leasePolicy }
	g := newGatewayWith(t, newResolver(u), cfg(1), nil, f)
	g.identity.Store(identityV(t, 1))
	send := func(what string, wantCalls int) {
		t.Helper()
		if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
			t.Errorf("%s: %d", what, got.status)
		}
		if calls := f.take(); len(calls) != wantCalls {
			t.Errorf("%s: %d calls, want %d", what, len(calls), wantCalls)
		}
	}
	send("first", 1)
	send("served", 0)
	g.apply(t, cfg(2))
	send("after config version 2", 1)
	send("served on version 2", 0)
	g.identity.Store(identityV(t, 2))
	send("after identity version 2", 1)
	hasMetrics(t, g, `gw_ratelimit_lease_tokens_dropped_total{reason="reset"} 6`) // 3 left at each swap
}

// TestLeasePutBack: a request that took its cost from a lease and is then
// denied (here a fail_closed 503 from a later call) puts it back; one whose
// own reservation was granted leaves the whole grant in the lease, never
// refunded in Redis (ADR-0069).
func TestLeasePutBack(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	cfg := func(v int) string {
		return fmt.Sprintf("version: %d\n", v) + oneUpstream(u) + `
policies:
  - {name: batched, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 100, period: 1s}], lease_k: 5}
  - {name: global, level: global, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], failure_mode: fail_closed}
`
	}
	g := newGatewayWith(t, newResolver(u), cfg(1), nil, f)
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
	alice := http.Header{"X-Api-Key": {aliceKey}}
	key := rlredis.TenantKey("acme", "batched", time.Second, rlredis.Digest("acme"))
	send := func(what string, status int, tokens int64) {
		t.Helper()
		if got := g.send(t, http.MethodGet, "/", alice); got.status != status {
			t.Errorf("%s: %d, want %d", what, got.status, status)
		}
		if got := leaseTokens(g.h, key); got != tokens {
			t.Errorf("%s: the lease holds %d, want %d", what, got, tokens)
		}
	}
	send("granted", http.StatusOK, 4)
	f.setFail("{g:global}", true)
	send("served, then a 503", http.StatusServiceUnavailable, 4)
	send("again", http.StatusServiceUnavailable, 4)
	g.apply(t, cfg(2)) // empties the table
	send("own grant, then a 503", http.StatusServiceUnavailable, 5)
	if got := f.usedOf(key); got != 10 {
		t.Errorf("Redis holds %d for the batched policy, want both grants of 5", got)
	}
	f.setFail("{g:global}", false)
	f.take()
	send("served after the outage", http.StatusOK, 4)
	if calls := f.take(); len(calls) != 1 || strings.Contains(calls[0], "{t:acme}") {
		t.Errorf("calls %q, want the global one only", calls)
	}
	f.takeRefunds(t, 0) // nothing to refund: the leased policy has no Redis deduction of its own
}

// TestLeaseFailure: a reservation that fails follows the policy's failure
// mode exactly as a strict check does and leaves nothing in flight; a live
// lease keeps serving while Redis is down (ADR-0069, "Redis failure").
func TestLeaseFailure(t *testing.T) {
	u := newUpstream(t, "orders")
	for _, tt := range []struct {
		mode   string
		status int
	}{{"fail_closed", http.StatusServiceUnavailable}, {"fail_open", http.StatusOK}} {
		t.Run(tt.mode, func(t *testing.T) {
			f := newFakeRedis()
			g := newGatewayWith(t, newResolver(u), oneUpstream(u)+strings.Replace(leasePolicy, "fail_closed", tt.mode, 1), nil, f)
			tag := "{k:" + rlredis.Digest("127.0.0.1") + "}"
			f.setFail(tag, true)
			if got := g.send(t, http.MethodGet, "/", nil); got.status != tt.status {
				t.Errorf("failed reservation: %d, want %d", got.status, tt.status)
			}
			f.setFail(tag, false)
			f.take()
			if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
				t.Errorf("next: %d", got.status)
			}
			if calls := f.take(); len(calls) != 1 || !strings.HasSuffix(calls[0], "lease 5") {
				t.Errorf("next request's calls %q, want a reservation", calls)
			}
			f.setFail(tag, true)
			if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
				t.Errorf("served during the outage: %d", got.status)
			}
			if calls := f.take(); len(calls) != 0 {
				t.Errorf("a served request called Redis: %q", calls)
			}
			hasMetrics(t, g, `gw_ratelimit_lease_total{result="error"} 1`, `gw_ratelimit_lease_total{result="local"} 1`)
		})
	}
}

// TestLeaseStrict: leases are for enforce policies and costs up to k only;
// a shadow policy, or a route whose cost is above k, is checked strictly
// (ADR-0069).
func TestLeaseStrict(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), fmt.Sprintf(`
upstreams: [{name: orders, endpoints: [{address: 'orders:%s'}]}]
routes:
  - {name: cheap, upstream: orders, match: {path_prefix: /cheap}}
  - {name: dear, upstream: orders, match: {path_prefix: /dear}, cost: 6}
policies:
  - {name: shadow, level: route, match: {routes: [cheap]}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 100, period: 1s}], lease_k: 5, mode: shadow}
  - {name: batched, level: route, match: {routes: [dear]}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 100, period: 1s}], lease_k: 5}
`, u.port()), nil, f)
	for _, path := range []string{"/cheap", "/cheap", "/dear", "/dear"} {
		if got := g.send(t, http.MethodGet, path, nil); got.status != http.StatusOK {
			t.Errorf("%s: %d", path, got.status)
		}
		if calls := f.take(); len(calls) != 1 || strings.Contains(calls[0], "lease") {
			t.Errorf("%s: calls %q, want one strict call", path, calls)
		}
	}
}

// TestLeaseInflight: while one request's reservation for a key is in
// flight, another request on the replica checks the policy strictly in its
// own call instead of waiting (ADR-0069, "In flight").
func TestLeaseInflight(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+"ratelimit: {redis_budget: 1s}\n"+leasePolicy, nil, f)
	tag := "{k:" + rlredis.Digest("127.0.0.1") + "}"
	f.mu.Lock()
	f.slow[tag] = 300 * time.Millisecond
	f.mu.Unlock()
	var wg sync.WaitGroup
	wg.Go(func() {
		if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
			t.Errorf("reserving request: %d", got.status)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		n := len(f.arrivals)
		f.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
	}
	if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
		t.Errorf("concurrent request: %d", got.status)
	}
	wg.Wait()
	calls := f.take()
	if len(calls) != 2 || !strings.HasSuffix(calls[0], "lease 5") || strings.Contains(calls[1], "lease") {
		t.Errorf("calls %q, want a reservation then a strict call", calls)
	}
	hasMetrics(t, g, `gw_ratelimit_lease_total{result="inflight"} 1`, `gw_ratelimit_lease_total{result="granted"} 1`)
}

// gcraLimits is one GCRA limit of rate per second with burst, as a request
// and a grant see it.
func gcraLimits(rate, burst int64) ([]rlredis.Limit, []leaseLimit) {
	return []rlredis.Limit{{Algorithm: config.GCRA, Rate: rate, Period: time.Second, Burst: burst}},
		[]leaseLimit{{rate: rate, burst: burst, period: time.Second}}
}

// TestLeaseTable: the table's rules on their own: changed limits drop the
// lease (an override that starts or ends), a put-back never goes above k
// nor into a lease that is reserving or expired, and a replaced generation
// serves nothing and drops what it held or is granted.
func TestLeaseTable(t *testing.T) {
	var drops []string
	tbl := newLeaseTable(func(reason string, n int64) { drops = append(drops, fmt.Sprint(reason, " ", n)) })
	g := tbl.generation(1, 1)
	req, grant := gcraLimits(100, 100)
	if s := g.acquire("k", req, 1, t0).step; s != leaseReserve {
		t.Fatalf("first: step %v", s)
	}
	if s := g.acquire("k", req, 1, t0).step; s != leaseStrict {
		t.Fatalf("while reserving: step %v", s)
	}
	g.granted("k", 1, 5, t0, t0, 1, grant)
	if take := g.acquire("k", req, 2, t0); take.step != leaseServe || take.tokens != 2 {
		t.Fatalf("serve: %+v", take)
	}
	g.putBack("k", 5, 4, t0)
	if take := g.acquire("k", req, 1, t0); take.tokens != 4 {
		t.Errorf("after a put-back of 4 into 2: %d left after 1, want 5 capped at k, less 1", take.tokens)
	}
	other, _ := gcraLimits(50, 100)
	if s := g.acquire("k", other, 1, t0).step; s != leaseReserve {
		t.Errorf("changed limits: step %v, want a reservation", s)
	}
	g.putBack("k", 5, 1, t0) // reserving: not put back
	g.ended("k")
	if s := g.acquire("k", req, 1, t0).step; s != leaseReserve {
		t.Errorf("after an ended reservation: step %v, want another", s)
	}
	g.granted("k", 1, 3, t0, t0, 1, grant)
	g.putBack("k", 5, 1, t0.Add(leaseLife)) // expired: not put back
	if take := g.acquire("k", req, 1, t0); take.tokens != 1 {
		t.Errorf("after put-backs into a reserving and an expired lease: %d left, want 1", take.tokens)
	}

	g2 := tbl.generation(2, 1)
	if g2 == g || !g.dead.Load() {
		t.Fatal("no new generation")
	}
	if s := g.acquire("k", req, 1, t0).step; s != leaseStrict {
		t.Errorf("replaced generation: step %v", s)
	}
	if tbl.generation(1, 1) != nil {
		t.Error("an older pair got a generation")
	}
	g.granted("k", 1, 5, t0, t0, 1, grant) // a reservation from before the swap
	want := []string{"reset 4", "reset 1", "reset 4"}
	if !slices.Equal(drops, want) {
		t.Errorf("drops %q, want %q", drops, want)
	}
}

// TestLeaseTableCap: a shard holds 1,024 leases; a new key in a full shard
// first removes the expired ones that no reservation uses, and is checked
// strictly if the shard is still full (ADR-0069, "Size cap").
func TestLeaseTableCap(t *testing.T) {
	g := newLeaseTable(nil).generation(1, 1)
	req, grant := gcraLimits(100, 100)
	var keys []string
	for i := 0; len(keys) <= leaseShardSize; i++ {
		if k := fmt.Sprint("key-", i); g.shard(k) == &g.shards[0] {
			keys = append(keys, k)
		}
	}
	for i, k := range keys[:leaseShardSize] {
		if s := g.acquire(k, req, 1, t0).step; s != leaseReserve {
			t.Fatalf("key %d: step %v", i, s)
		}
		if i > 0 { // the first stays reserving
			g.granted(k, 1, 5, t0, t0, 1, grant)
		}
	}
	if take := g.acquire(keys[leaseShardSize], req, 1, t0.Add(leaseLife-time.Nanosecond)); take.step != leaseStrict || take.result != leaseFull {
		t.Errorf("full shard: %+v", take)
	}
	if s := g.acquire(keys[leaseShardSize], req, 1, t0.Add(leaseLife)).step; s != leaseReserve {
		t.Errorf("after the leases expired: step %v", s)
	}
	if n := len(g.shards[0].m); n != 2 {
		t.Errorf("%d leases left, want the reserving one and the new one", n)
	}
}

// TestLeaseBound is the property test of ADR-0069's Tests section: random
// arrivals on several simulated replicas, each with its own lease table, in
// front of one simulated Redis running the script's GCRA with lease groups;
// calls reach Redis and answer later, in any order, and some requests are
// denied later and put their cost back. In every window, admissions stay
// within GCRA's bound plus replicas × k, and no replica ever holds more
// than k unspent units, counting a granted reservation still on its way.
func TestLeaseBound(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 5).Draw(rt, "replicas")
		k := rapid.Int64Range(1, 10).Draw(rt, "k")
		rate := rapid.Int64Range(1, 50).Draw(rt, "rate")
		burst := rapid.Int64Range(k, 50).Draw(rt, "burst")
		period := time.Second
		tUS := (period.Microseconds() + rate - 1) / rate
		tau := tUS * burst
		req, view := gcraLimits(rate, burst)

		type call struct {
			replica   int
			reserve   bool
			cost      int64
			sent      time.Time
			executed  bool
			allowed   bool
			atUS      int64 // Redis time of the execution
			grant     int64
			laterDeny bool
			remaining int64
		}
		tables := make([]*leaseGeneration, n)
		for i := range tables {
			tables[i] = newLeaseTable(nil).generation(1, 1)
		}
		var nowUS, tat int64
		now := func() time.Time { return t0.Add(time.Duration(nowUS) * time.Microsecond) }
		var pending []*call
		var admitted []int64 // µs

		steps := rapid.IntRange(1, 400).Draw(rt, "steps")
		for range steps {
			switch rapid.IntRange(0, 3).Draw(rt, "op") {
			case 0: // time passes
				nowUS += rapid.Int64Range(0, 50_000).Draw(rt, "advance")
			case 1: // requests arrive at a replica at once
				r := rapid.IntRange(0, n-1).Draw(rt, "replica")
				for range rapid.IntRange(1, 10).Draw(rt, "arrivals") {
					cost := rapid.Int64Range(1, k).Draw(rt, "cost")
					laterDeny := rapid.IntRange(0, 9).Draw(rt, "laterDeny") == 0
					switch take := tables[r].acquire("key", req, cost, now()); take.step {
					case leaseServe:
						if laterDeny {
							tables[r].putBack("key", k, cost, now())
						} else {
							for range cost {
								admitted = append(admitted, nowUS)
							}
						}
					default:
						pending = append(pending, &call{replica: r, reserve: take.step == leaseReserve, cost: cost, sent: now(), laterDeny: laterDeny})
					}
				}
			case 2: // a sent call runs in Redis
				var waiting []*call
				for _, c := range pending {
					if !c.executed {
						waiting = append(waiting, c)
					}
				}
				if len(waiting) == 0 {
					continue
				}
				c := waiting[rapid.IntRange(0, len(waiting)-1).Draw(rt, "execute")]
				c.executed, c.atUS = true, nowUS
				s := max(tat, nowUS)
				if s+c.cost*tUS-nowUS <= tau {
					units := c.cost
					if c.reserve {
						units = min(k, (tau-(s-nowUS))/tUS)
					}
					c.allowed, c.grant, tat = true, units, s+units*tUS
					c.remaining = (tau - (tat - nowUS)) / tUS
				}
			case 3: // a call's answer reaches its replica
				var done []int
				for i, c := range pending {
					if c.executed {
						done = append(done, i)
					}
				}
				if len(done) == 0 {
					continue
				}
				i := done[rapid.IntRange(0, len(done)-1).Draw(rt, "deliver")]
				c := pending[i]
				pending = slices.Delete(pending, i, i+1)
				g := tables[c.replica]
				switch {
				case c.reserve && c.allowed:
					v := slices.Clone(view)
					v[0].remaining = c.remaining
					g.granted("key", c.cost, c.grant, c.sent, now(), c.atUS, v)
				case c.reserve:
					g.ended("key")
				}
				if c.allowed && c.laterDeny && c.reserve {
					g.putBack("key", k, c.cost, now())
				} else if c.allowed && !c.laterDeny {
					for range c.cost {
						admitted = append(admitted, c.atUS)
					}
				}
				// A strict call later denied would be refunded in Redis
				// (ADR-0028); not refunding only admits less.
			}
			for r, g := range tables {
				var held int64
				for _, c := range pending {
					if c.replica == r && c.reserve && c.allowed {
						held += c.grant - c.cost
					}
				}
				s := g.shard("key")
				s.mu.Lock()
				if e := s.m["key"]; e != nil {
					held += e.tokens
				}
				s.mu.Unlock()
				if held > k {
					rt.Fatalf("replica %d holds %d unspent units, granted ones on their way included; k = %d", r, held, k)
				}
			}
		}
		slices.Sort(admitted)
		for _, w := range []int64{0, 1_000, 10_000, 100_000, 1_000_000, 10_000_000} {
			bound := burst + w*rate/period.Microseconds() + int64(n)*k
			j := 0
			for i, at := range admitted {
				for j < len(admitted) && admitted[j] <= at+w {
					j++
				}
				if got := int64(j - i); got > bound {
					rt.Fatalf("%d admitted in [%d, %d] µs; bound burst %d + w·rate/period %d + replicas × k %d", got, at, at+w, burst, w*rate/period.Microseconds(), int64(n)*k)
				}
			}
		}
	})
}
