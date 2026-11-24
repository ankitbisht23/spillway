//go:build integration

package integration

import (
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestGatewayLease checks batched mode end to end on compose gateway-1 and
// the compose Redis Cluster: batched-global (GCRA 10/s, burst 200, lease_k
// 20, keyed by X-Batched-Key) driven at twice its rate, 20 requests per
// second for 3 s, admits every request within its burst and decides fewer
// than one in five in Redis, as both the gateway's lease counts and the
// cluster's EVALSHA count show (RL-18, ADR-0069).
//
// Timing: a lease lasts 1 s and holds 20 units, a second's worth of these
// requests, so the run needs about 3 reservations, 4 if a lease expires
// with a unit left. A host or Docker stall (ADR-0026, ADR-0027, ADR-0034:
// at worst about 820 ms) delays requests and can expire a lease early,
// costing at most one more reservation per stall; the pass rule, fewer
// than 12 calls for 60 requests, leaves room for several.
func TestGatewayLease(t *testing.T) {
	_, _, rdb := scriptEnv(t)
	run := scriptName("lease")
	client := &http.Client{Timeout: 10 * time.Second}
	send := func(key string) (int, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000/anything/batched", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Batched-Key", key)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, resp.Header
	}
	// Warm-up, as in TestGatewayDenyCache (ADR-0024).
	const policy = `"per-ip";q=5000;w=1, "batched-global";q=10;w=1`
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

	type counts struct{ local, reserved, redisChecks, evalsha int64 }
	read := func() counts {
		t.Helper()
		_, m := get(t, "http://127.0.0.1:8001/metrics")
		var reserved int64
		for _, r := range []string{"granted", "partial", "denied", "error", "inflight", "full"} {
			reserved += metricInt(t, m, `gw_ratelimit_lease_total{result="`+r+`"}`)
		}
		return counts{
			local:       metricInt(t, m, `gw_ratelimit_lease_total{result="local"}`),
			reserved:    reserved,
			redisChecks: metricInt(t, m, `gw_ratelimit_check_duration_seconds_count{backend="redis"}`),
			evalsha:     evalshaCalls(t, rdb),
		}
	}

	const requests = 60
	before := read()
	start := time.Now()
	for i := range requests {
		time.Sleep(time.Until(start.Add(time.Duration(i) * 50 * time.Millisecond)))
		if status, h := send(run); status != http.StatusOK {
			t.Fatalf("request %d: %d (RateLimit %q), want 200 within the burst", i+1, status, h.Get("RateLimit"))
		}
	}
	after := read()
	d := counts{after.local - before.local, after.reserved - before.reserved, after.redisChecks - before.redisChecks, after.evalsha - before.evalsha}
	t.Logf("%d requests in %v: %+v", requests, time.Since(start).Round(time.Millisecond), d)
	if d.local+d.reserved != requests || d.redisChecks != d.reserved || d.evalsha != d.reserved {
		t.Errorf("deltas %+v: want local + reserved = %d, and one Redis call per reservation", d, requests)
	}
	if d.reserved*5 >= requests {
		t.Errorf("%d Redis calls for %d requests, want fewer than one in five", d.reserved, requests)
	}
}
