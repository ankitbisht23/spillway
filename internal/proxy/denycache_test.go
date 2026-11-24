package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// windowPolicy is one enforce Redis policy per client IP: the fake answers
// its deny with a retry-after of its period, 1 min, above the deny cache's
// 10 s.
const windowPolicy = `
policies:
  - {name: window, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: sliding_window, limits: [{rate: 2, period: 1m}]}
`

// TestDenyCache: after an enforce limit denies in Redis, the replica denies
// that key locally, without a Redis call, for min(retry-after, 10 s); a hit
// answers as a Redis deny does (429, Retry-After from the entry's end,
// RateLimit with r=0 and the reset the deny reported), records the deny
// decision, marked cached in the access log, and its own metrics, and no
// Redis check duration (RL-19, ADR-0069).
func TestDenyCache(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+windowPolicy, nil, f)
	send := func(id string, wantStatus int, wantCalls int, retryAfter, rateLimit string) {
		t.Helper()
		got := g.send(t, http.MethodGet, "/", http.Header{"X-Request-Id": {id}})
		if got.status != wantStatus || got.header.Get("Retry-After") != retryAfter || got.header.Get("RateLimit") != rateLimit {
			t.Errorf("%s: %d\n%s\nwant %d, Retry-After %q, RateLimit %q", id, got.status, rateLimitFields(got.header), wantStatus, retryAfter, rateLimit)
		}
		if calls := f.take(); len(calls) != wantCalls {
			t.Errorf("%s: %d Redis calls, want %d", id, len(calls), wantCalls)
		}
	}
	send("a1", http.StatusOK, 1, "", `"window";r=1;t=60`)
	send("a2", http.StatusOK, 1, "", `"window";r=0;t=60`)
	send("redis-deny", http.StatusTooManyRequests, 1, "60", `"window";r=0;t=60`)
	send("cached", http.StatusTooManyRequests, 0, "10", `"window";r=0;t=60`)
	g.clock.Advance(10*time.Second - time.Millisecond)
	send("cached-last", http.StatusTooManyRequests, 0, "1", `"window";r=0;t=51`)
	g.clock.Advance(time.Millisecond)
	send("expired", http.StatusTooManyRequests, 1, "60", `"window";r=0;t=60`) // and cached again

	for id, want := range map[string]string{
		"redis-deny": `[{"decision":"deny","mode":"enforce","policy":"window"}]`,
		"cached":     `[{"cached":true,"decision":"deny","mode":"enforce","policy":"window"}]`,
		"expired":    `[{"decision":"deny","mode":"enforce","policy":"window"}]`,
	} {
		if got := jsonOf(t, g.logs.access(t, id)["decisions"]); got != want {
			t.Errorf("%s: decisions %s, want %s", id, got, want)
		}
	}
	m := scrape(t, g.metrics)
	for _, s := range []string{
		`gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="window"} 4`,
		`gw_ratelimit_deny_cache_total{result="hit"} 2`,
		`gw_ratelimit_deny_cache_total{result="insert"} 2`,
		`gw_ratelimit_deny_cache_total{result="full"} 0`,
		`gw_ratelimit_deny_cache_entries 1`,
		`gw_ratelimit_check_duration_seconds_count{backend="redis"} 4`, // the requests that called Redis
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="ratelimited",route="r",tenant="none"} 4`,
	} {
		if !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}
}

// clockRedis moves the gateway's clock by d during every call, as a slow
// Redis would, before the call reaches Redis.
type clockRedis struct {
	Redis
	clock *ratelimit.FakeClock
	d     time.Duration
}

func (c *clockRedis) Call(ctx context.Context, cost int64, limits []rlredis.Limit) (rlredis.Result, error) {
	c.clock.Advance(c.d)
	return c.Redis.Call(ctx, cost, limits)
}

// TestDenyCacheFromSend: an entry holds from the deny's call was sent, not
// from when it answered, so it never outlives Redis's own deny (ADR-0069).
func TestDenyCacheFromSend(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	r := &clockRedis{Redis: f, d: 400 * time.Millisecond}
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+`
policies:
  - {name: gcra, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1, period: 1s}]}
`, nil, r)
	r.clock = g.clock
	for i, want := range []struct {
		advance time.Duration
		status  int
		calls   int
	}{
		{0, http.StatusOK, 1},
		{0, http.StatusTooManyRequests, 1},                      // sent at s, retry-after 1 s, answered at s+400 ms
		{599 * time.Millisecond, http.StatusTooManyRequests, 0}, // s+999 ms
		{time.Millisecond, http.StatusTooManyRequests, 1},       // s+1 s: Redis again
	} {
		g.clock.Advance(want.advance)
		if got := g.send(t, http.MethodGet, "/", nil); got.status != want.status {
			t.Errorf("request %d: %d, want %d", i+1, got.status, want.status)
		}
		if calls := f.take(); len(calls) != want.calls {
			t.Errorf("request %d: %d Redis calls, want %d", i+1, len(calls), want.calls)
		}
	}
}

// TestDenyCacheEnforceOnly: only an enforce limit's Redis deny is cached;
// a shadow would-deny, a degraded limiter's deny and a fail_closed 503 are
// not, so the next request still calls Redis (RL-19, ADR-0069).
func TestDenyCacheEnforceOnly(t *testing.T) {
	u := newUpstream(t, "orders")
	for _, tt := range []struct {
		name, policy string
		fail         bool // Redis fails for the first requests
		statuses     []int
	}{
		{"shadow", `{name: p, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: sliding_window, limits: [{rate: 1, period: 1m}], mode: shadow}`,
			false, []int{200, 200, 200}},
		{"degraded", `{name: p, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: sliding_window, limits: [{rate: 1, period: 1m}], failure_mode: degraded}`,
			true, []int{200, 429}},
		{"fail_closed", `{name: p, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: sliding_window, limits: [{rate: 1, period: 1m}], failure_mode: fail_closed}`,
			true, []int{503, 503}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRedis()
			f.setFail("{k:", tt.fail)
			g := newGatewayWith(t, newResolver(u), oneUpstream(u)+"ratelimit: {replicas: 1}\npolicies:\n  - "+tt.policy+"\n", nil, f)
			for i, want := range tt.statuses {
				if got := g.send(t, http.MethodGet, "/", nil); got.status != want {
					t.Fatalf("request %d: %d, want %d", i+1, got.status, want)
				}
			}
			f.setFail("{k:", false)
			f.take()
			// Redis answers again: this request is decided there.
			if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK {
				t.Errorf("after Redis is back: %d, want 200", got.status)
			}
			if calls := f.take(); len(calls) != 1 {
				t.Errorf("after the would-deny or failure-mode answers: %d Redis calls, want 1", len(calls))
			}
			if m, s := scrape(t, g.metrics), `gw_ratelimit_deny_cache_total{result="insert"} 0`; !strings.Contains(m, "\n"+s+"\n") {
				t.Errorf("/metrics has no %s", s)
			}
		})
	}
}

// identityV compiles an identity of version v with alice's key.
func identityV(t *testing.T, v int) *config.Identity {
	t.Helper()
	sum := sha256.Sum256([]byte(aliceKey))
	c, err := config.LoadIdentity(fmt.Appendf(nil, "version: %d\nconsumers: [{name: alice, tenant: acme}]\napi_keys:\n  - {prefix: '%s', hash: %s, consumer: alice}\n",
		v, aliceKey[:config.KeyPrefixLen], hex.EncodeToString(sum[:])))
	if err != nil {
		t.Fatal(err)
	}
	id, err := config.CompileIdentity(c)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestDenyCacheClears: a new config version or a new identity version
// clears the deny cache, so the next request for a denied key calls Redis
// again (RL-19, ADR-0069).
func TestDenyCacheClears(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	cfg := func(v int) string { return fmt.Sprintf("version: %d\n", v) + oneUpstream(u) + windowPolicy }
	g := newGatewayWith(t, newResolver(u), cfg(1), nil, f)
	g.identity.Store(identityV(t, 1))
	alice := http.Header{"X-Api-Key": {aliceKey}}
	tenantKey := rlredis.TenantKey("acme", "window", time.Minute, rlredis.Digest("127.0.0.1"))
	f.setUsed(tenantKey, 2) // full: every request is denied in Redis
	denied := func(what string, wantCalls int) {
		t.Helper()
		if got := g.send(t, http.MethodGet, "/", alice); got.status != http.StatusTooManyRequests {
			t.Errorf("%s: %d, want 429", what, got.status)
		}
		if calls := f.take(); len(calls) != wantCalls {
			t.Errorf("%s: %d Redis calls, want %d", what, len(calls), wantCalls)
		}
	}
	denied("first deny", 1)
	denied("cached", 0)
	g.apply(t, cfg(2))
	denied("after config version 2", 1)
	denied("cached on version 2", 0)
	g.identity.Store(identityV(t, 2))
	denied("after identity version 2", 1)
	denied("cached on identity version 2", 0)
}

// TestDenyCacheGenerations: the first request on a newer snapshot or
// identity starts an empty generation; a request still on the older pair
// gets none, so it can neither be denied by nor repopulate the cache
// (ADR-0069, "Clearing").
func TestDenyCacheGenerations(t *testing.T) {
	c := newDenyCache()
	lim := rlredis.Limit{Key: "rl:v1:{k:x}:p:1000", Rate: 1, Burst: 1}
	e := denyEntry{until: t0.Add(time.Second), cost: 1, rate: 1, burst: 1}
	g1 := c.generation(3, 2)
	if r := g1.insert(lim.Key, e, t0); r != denyCacheInsert {
		t.Fatalf("insert: %s", r)
	}
	if c.generation(3, 2) != g1 || c.entries() != 1 {
		t.Fatal("the same pair got another generation")
	}
	g2 := c.generation(4, 2) // a new snapshot
	if _, ok := g2.lookup(lim, 1, t0); ok || c.entries() != 0 {
		t.Error("a new snapshot's generation holds the old one's entry")
	}
	if c.generation(3, 2) != nil || c.generation(4, 1) != nil {
		t.Error("a request on an older snapshot or identity got a generation")
	}
	g3 := c.generation(4, 5) // a new identity
	if g3 == g2 || c.generation(4, 5) != g3 {
		t.Error("a new identity did not start one generation")
	}
	// A request that took g2 before the swap caches its deny there, where
	// no request on the new pair looks.
	g2.insert(lim.Key, e, t0)
	if _, ok := c.generation(4, 5).lookup(lim, 1, t0); ok {
		t.Error("an insert into a replaced generation is visible")
	}
}

// TestDenyCacheLookup: an entry denies a request until its end, if the
// request costs at least what was denied and the limit is the one the deny
// was made for; an expired entry is removed (ADR-0069).
func TestDenyCacheLookup(t *testing.T) {
	g := newDenyCache().generation(1, 1)
	lim := rlredis.Limit{Key: "rl:v1:{k:x}:p:1000", Rate: 10, Burst: 20}
	g.insert(lim.Key, denyEntry{until: t0.Add(time.Second), cost: 5, rate: 10, burst: 20}, t0)
	other := lim
	other.Rate = 11 // an override that started since
	for _, tt := range []struct {
		name string
		lim  rlredis.Limit
		cost int64
		at   time.Duration
		hit  bool
	}{
		{"same cost", lim, 5, 0, true},
		{"dearer", lim, 6, 999 * time.Millisecond, true},
		{"cheaper", lim, 4, 0, false},
		{"another rate", other, 5, 0, false},
		{"another key", rlredis.Limit{Key: "rl:v1:{k:y}:p:1000", Rate: 10, Burst: 20}, 5, 0, false},
		{"expired", lim, 5, time.Second, false},
	} {
		if _, ok := g.lookup(tt.lim, tt.cost, t0.Add(tt.at)); ok != tt.hit {
			t.Errorf("%s: hit %v, want %v", tt.name, ok, tt.hit)
		}
	}
	if g.entries.Load() != 0 {
		t.Errorf("%d entries after the expired lookup, want 0", g.entries.Load())
	}
}

// TestDenyCacheBound: a shard holds at most 1,024 entries, 65,536 in all;
// an insert into a shard full of live entries is skipped and evicts none,
// and one into a full shard with expired entries removes them first
// (ADR-0069, "Size cap").
func TestDenyCacheBound(t *testing.T) {
	g := newDenyCache().generation(1, 1)
	live := denyEntry{until: t0.Add(time.Second), cost: 1}
	var keys []string
	full := ""
	for i := 0; full == "" && i < 1_000_000; i++ {
		key := fmt.Sprint("rl:v1:{k:", i, "}:p:1000")
		switch g.insert(key, live, t0) {
		case denyCacheInsert:
			keys = append(keys, key)
		case denyCacheFull:
			full = key
		}
	}
	if full == "" {
		t.Fatal("no insert was skipped")
	}
	total := 0
	for i := range g.shards {
		if n := len(g.shards[i].m); n > denyCacheShardSize {
			t.Errorf("shard %d holds %d entries", i, n)
		} else {
			total += n
		}
	}
	if s := g.shard(full); len(s.m) != denyCacheShardSize || total != len(keys) || g.entries.Load() != int64(total) || total > denyCacheShards*denyCacheShardSize {
		t.Errorf("full shard %d entries, %d in all, counted %d, inserted %d", len(s.m), total, g.entries.Load(), len(keys))
	}
	for _, k := range keys {
		if _, ok := g.lookup(rlredis.Limit{Key: k}, 1, t0); !ok {
			t.Fatalf("live entry %s was evicted", k)
		}
	}
	// Once they expire, the full shard makes room.
	end := t0.Add(time.Second)
	if r := g.insert(full, denyEntry{until: end.Add(time.Second), cost: 1}, end); r != denyCacheInsert {
		t.Fatalf("insert after expiry: %s", r)
	}
	if n := len(g.shard(full).m); n != 1 {
		t.Errorf("full shard holds %d entries after the insert, want 1", n)
	}
	if want := int64(total - denyCacheShardSize + 1); g.entries.Load() != want {
		t.Errorf("%d entries, want %d", g.entries.Load(), want)
	}
}

// TestDenyCacheConcurrent runs lookups, inserts and generation changes at
// once (with -race in make test): the count of entries stays exact.
func TestDenyCacheConcurrent(t *testing.T) {
	c := newDenyCache()
	var seq struct {
		sync.Mutex
		snapshot uint64
	}
	seq.snapshot = 1
	current := func() uint64 { seq.Lock(); defer seq.Unlock(); return seq.snapshot }
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 1)) //nolint:gosec // G404: test keys, not secrets
			for i := range 5000 {
				g := c.generation(current(), 1)
				if g == nil {
					continue // a newer snapshot is in use
				}
				lim := rlredis.Limit{Key: fmt.Sprint("k", r.IntN(4096))}
				now := t0.Add(time.Duration(i) * time.Millisecond)
				if _, ok := g.lookup(lim, 1, now); !ok {
					g.insert(lim.Key, denyEntry{until: now.Add(time.Duration(r.IntN(20)) * time.Millisecond), cost: 1}, now)
				}
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			seq.Lock()
			seq.snapshot++
			seq.Unlock()
			time.Sleep(100 * time.Microsecond)
		}
	})
	wg.Wait()
	g := c.cur.Load()
	total := 0
	for i := range g.shards {
		total += len(g.shards[i].m)
	}
	if int64(total) != g.entries.Load() {
		t.Errorf("%d entries in the shards, counted %d", total, g.entries.Load())
	}
}

// TestDenyCacheConcurrentRequests sends requests from several clients
// while the config is swapped (with -race in make test): each is answered
// 200 or 429, and a denied key is cached again after each swap.
func TestDenyCacheConcurrentRequests(t *testing.T) {
	u := newUpstream(t, "orders")
	f := newFakeRedis()
	g := newGatewayWith(t, newResolver(u), oneUpstream(u)+windowPolicy, nil, f)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 40 {
				if got := g.send(t, http.MethodGet, "/", nil); got.status != http.StatusOK && got.status != http.StatusTooManyRequests {
					t.Errorf("status %d", got.status)
				}
			}
		})
	}
	wg.Go(func() {
		for v := range 10 {
			g.apply(t, fmt.Sprintf("version: %d\n", v+2)+oneUpstream(u)+windowPolicy)
			time.Sleep(time.Millisecond)
		}
	})
	wg.Wait()
	f.take()
	g.send(t, http.MethodGet, "/", nil) // denied in Redis, or from the cache
	g.send(t, http.MethodGet, "/", nil)
	if calls := f.take(); len(calls) > 1 {
		t.Errorf("%d Redis calls for two requests for a denied key, want at most 1", len(calls))
	}
}
