//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// countingRedis counts the limiter calls that reach the compose cluster.
type countingRedis struct {
	*redis.Client
	calls atomic.Int64
}

func (c *countingRedis) Call(ctx context.Context, cost int64, limits []redis.Limit) (redis.Result, error) {
	c.calls.Add(1)
	return c.Client.Call(ctx, cost, limits)
}

// TestDenyCacheRedis runs a gateway handler in the test against the compose
// Redis Cluster (ADR-0004) with its deny cache's clock in the test's hands:
// once Redis denies a key, requests for it make no Redis call until the
// entry expires, or until a new config or identity version clears the cache
// (RL-19, ADR-0069). Redis's own deny lasts about a minute of real time, far
// longer than the test, so every request here is denied; only whether it
// reached Redis changes.
func TestDenyCacheRedis(t *testing.T) {
	_, client, _ := scriptEnv(t)
	r := &countingRedis{Client: client}
	// Warm-up (ADR-0024): a fresh client's first call to a node loads the
	// slot map, dials and loads the script, which can overrun the budget.
	// This call has the requests' hash slot under another policy's key.
	const ip = "192.0.2.7"
	slotKey := redis.NoTenantKey(redis.KeyDigest(ratelimit.Key(config.IPKey(netip.MustParseAddr(ip)))), scriptName("warm"), time.Minute)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := client.Call(ctx, 1, []redis.Limit{gcraLimit(slotKey, "route", true, 1, time.Minute, 1)})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm-up call: %v", err)
		}
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	policy := scriptName("deny")
	snapshot := func(version int) *config.Snapshot {
		t.Helper()
		c, err := config.Load([]byte(`version: ` + strconv.Itoa(version) + `
ratelimit: {redis_budget: 1s}
upstreams: [{name: up, endpoints: [{address: '127.0.0.1:` + port + `'}]}]
routes: [{name: r, upstream: up, match: {path_prefix: /}}]
policies:
  - {name: ` + policy + `, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1, period: 1m}], failure_mode: fail_closed}
`))
		if err != nil {
			t.Fatal(err)
		}
		s, err := config.Compile(c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	identity := func(version int) *config.Identity {
		t.Helper()
		c, err := config.LoadIdentity([]byte("version: " + strconv.Itoa(version) + "\nconsumers: [{name: alice}]\n"))
		if err != nil {
			t.Fatal(err)
		}
		id, err := config.CompileIdentity(c)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	discard := slog.New(slog.DiscardHandler)
	pools := proxy.NewRegistry(proxy.Allowlist{netip.MustParsePrefix("127.0.0.0/8")}, net.DefaultResolver, discard)
	t.Cleanup(pools.Close)
	clock := ratelimit.NewFakeClock(time.Now())
	limiters := local.NewRegistry(clock)
	var current atomic.Pointer[config.Snapshot]
	var ids atomic.Pointer[config.Identity]
	swap := func(s *config.Snapshot) {
		t.Helper()
		if err := pools.Apply(s); err != nil {
			t.Fatal(err)
		}
		limiters.Apply(s)
		current.Store(s)
	}
	swap(snapshot(1))
	ids.Store(identity(1))
	reg := prometheus.NewRegistry()
	h := proxy.NewHandler(current.Load, ids.Load, nil, pools, limiters, r, false, 4096, nil, telemetry.NewRecorder(reg, discard, 0), discard)

	send := func(what string, status int, calls int64) {
		t.Helper()
		before := r.calls.Load()
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		req.RemoteAddr = ip + ":1234"
		h.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s: %d %s, want %d", what, w.Code, w.Body, status)
		}
		if got := r.calls.Load() - before; got != calls {
			t.Errorf("%s: %d Redis calls, want %d", what, got, calls)
		}
	}
	send("first", http.StatusOK, 1)
	send("denied in Redis", http.StatusTooManyRequests, 1)
	for range 20 {
		send("cached", http.StatusTooManyRequests, 0)
	}
	clock.Advance(10*time.Second - time.Millisecond)
	send("cached until the entry's last instant", http.StatusTooManyRequests, 0)
	clock.Advance(time.Millisecond)
	send("expired", http.StatusTooManyRequests, 1)
	send("cached again", http.StatusTooManyRequests, 0)
	swap(snapshot(2))
	send("config version 2", http.StatusTooManyRequests, 1)
	send("cached on config version 2", http.StatusTooManyRequests, 0)
	ids.Store(identity(2))
	send("identity version 2", http.StatusTooManyRequests, 1)
	send("cached on identity version 2", http.StatusTooManyRequests, 0)

	metrics := gatherText(t, reg)
	for _, m := range []string{`gw_ratelimit_deny_cache_total{result="hit"} 24`, `gw_ratelimit_deny_cache_total{result="insert"} 4`} {
		if !strings.Contains(metrics, "\n"+m+"\n") {
			t.Errorf("metrics have no %s", m)
		}
	}
}

// gatherText is what reg serves on /metrics, after a newline.
func gatherText(t *testing.T, reg prometheus.Gatherer) string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := make([]string, 0, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+`="`+l.GetValue()+`"`)
			}
			name := f.GetName()
			if len(labels) > 0 {
				name += "{" + strings.Join(labels, ",") + "}"
			}
			var v float64
			switch {
			case m.GetCounter() != nil:
				v = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				v = m.GetGauge().GetValue()
			default:
				continue
			}
			b.WriteString(name + " " + strconv.FormatFloat(v, 'g', -1, 64) + "\n")
		}
	}
	return b.String()
}

// TestGatewayDenyCache checks the deny cache end to end on compose
// gateway-1 and the compose Redis Cluster: shared-global (GCRA 3 per 10 s,
// keyed by X-Shared-Key; anonymous, so the only Redis policy) denies the 4th
// request in Redis, the requests right after it are denied without reaching
// Redis, as both the gateway's metrics and the cluster's EVALSHA count show,
// and once the deny's retry-after has passed the next request is decided in
// Redis again (RL-19, ADR-0069).
//
// Timing, chosen so that a host or Docker stall (ADR-0026, ADR-0027,
// ADR-0034: usually under 10 ms, at worst about 820 ms) cannot fail it: the
// 4th request's entry holds until about 3.33 s after the first admission
// (GCRA's 10 s tolerance plus one 3.33 s emission interval, minus the 10 s
// the three admissions added), less the deny call's own trip. Requests that
// must hit it are sent only within 1.8 s of the first admission, leaving
// 1.5 s for stalls; the request that must reach Redis again waits the whole
// Retry-After after the deny, and a longer wait changes nothing.
func TestGatewayDenyCache(t *testing.T) {
	_, _, rdb := scriptEnv(t)
	run := scriptName("denycache")
	client := &http.Client{Timeout: 10 * time.Second}
	send := func(sharedKey string) (int, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000/anything/shared", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Shared-Key", sharedKey)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, resp.Header
	}
	// Warm-up, as in TestGatewayGlobalLimit (ADR-0024): until the gateway
	// has the slot map, connections and script, a call can fail open.
	const policy = `"per-ip";q=5000;w=1, "shared-global";q=3;w=10`
	for i, deadline := 0, time.Now().Add(5*time.Second); ; i++ {
		_, h := send(run + "-warm-" + strconv.Itoa(i))
		if h.Get("RateLimit-Policy") == policy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm-up: RateLimit-Policy %q, want %q", h.Get("RateLimit-Policy"), policy)
		}
		time.Sleep(50 * time.Millisecond)
	}

	type counts struct{ hits, inserts, redisChecks, evalsha int64 }
	read := func() counts {
		t.Helper()
		_, m := get(t, "http://127.0.0.1:8001/metrics")
		return counts{
			hits:        metricInt(t, m, `gw_ratelimit_deny_cache_total{result="hit"}`),
			inserts:     metricInt(t, m, `gw_ratelimit_deny_cache_total{result="insert"}`),
			redisChecks: metricInt(t, m, `gw_ratelimit_check_duration_seconds_count{backend="redis"}`),
			evalsha:     evalshaCalls(t, rdb),
		}
	}

	first := time.Now()
	for i := 1; i <= 3; i++ {
		if status, _ := send(run); status != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i, status)
		}
	}
	before := read()
	status, h := send(run)
	if status != http.StatusTooManyRequests {
		t.Fatalf("4th request: %d, want 429", status)
	}
	denied := time.Now()
	cached := 0
	for ; cached < 10 && time.Since(first) < 1800*time.Millisecond; cached++ {
		if status, _ := send(run); status != http.StatusTooManyRequests {
			t.Fatalf("request %d after the deny: %d, want 429", cached+1, status)
		}
	}
	if cached == 0 {
		t.Fatalf("the 4th request answered %v after the first admission: no time left for cached denies", denied.Sub(first))
	}
	after := read()
	if got, want := (counts{after.hits - before.hits, after.inserts - before.inserts, after.redisChecks - before.redisChecks, after.evalsha - before.evalsha}), (counts{int64(cached), 1, 1, 1}); got != want {
		t.Errorf("after the deny and %d more requests: deltas %+v, want %+v (one Redis call, for the deny)", cached, got, want)
	}

	retry, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After %q: %v", h.Get("Retry-After"), err)
	}
	time.Sleep(time.Until(denied.Add(time.Duration(retry) * time.Second)))
	if status, _ := send(run); status != http.StatusOK {
		t.Errorf("after Retry-After: %d, want 200 from Redis", status)
	}
	if last := read(); last.hits != after.hits || last.redisChecks != after.redisChecks+1 || last.evalsha != after.evalsha+1 {
		t.Errorf("after Retry-After: %+v, then %+v; want one more Redis call and no hit", after, last)
	}
}

// metricInt returns the value of the series named exactly series in a
// /metrics text.
func metricInt(t *testing.T, metrics, series string) int64 {
	t.Helper()
	for line := range strings.SplitSeq(metrics, "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			return int64(f)
		}
	}
	t.Fatalf("/metrics has no %s", series)
	return 0
}

var evalshaStat = regexp.MustCompile(`(?m)^cmdstat_evalsha:calls=(\d+),`)

// evalshaCalls is the number of EVALSHA calls the cluster's primaries have
// served: the limiter's calls and refunds.
func evalshaCalls(t *testing.T, rdb *goredis.ClusterClient) int64 {
	t.Helper()
	var total atomic.Int64
	err := rdb.ForEachMaster(t.Context(), func(ctx context.Context, c *goredis.Client) error {
		info, err := c.Info(ctx, "commandstats").Result()
		if err != nil {
			return err
		}
		if m := evalshaStat.FindStringSubmatch(info); m != nil {
			n, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				return err
			}
			total.Add(n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total.Load()
}
