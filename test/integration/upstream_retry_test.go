//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// TestGatewayRetries runs a gateway on the host in front of the compose
// mock upstream upstream-1 (published on 9101) and a second endpoint that
// fails, and checks that retries hide the failure
// and stay within the upstream's budget (GW-12, GW-13, GW-15, ADR-0030):
//
//   - with the second endpoint answering 503, closing connections, or
//     refusing them, every request succeeds, and exactly the requests that
//     started on it are retried;
//   - an upstream whose only endpoint answers 503 gets the retries its
//     budget grants, 0.1 × requests + 0.1/s × 10 s, and no more;
//   - the upstream gets Spillway-Deadline-Ms from the gateway, never the
//     client's.
//
// Upstreams are balanced round robin, so that half of the requests start
// on the failing endpoint and each retry has one candidate left, and
// outlier ejection is held off, so that the failing endpoint stays in
// rotation.
func TestGatewayRetries(t *testing.T) {
	fault, tight := newFaultProxy(t, "127.0.0.1:9103"), newFaultProxy(t, "127.0.0.1:9103")
	refused := freeAddr(t) // nothing listens there
	g := startGateway(t, os.Getenv(redis.AddrMapEnv), fmt.Sprintf(`
version: 1
upstreams:
  - name: faulty
    lb: round_robin
    endpoints: [{address: '127.0.0.1:9101'}, {address: '%s'}]
    outlier_detection: {consecutive_failures: 1000}
  - name: refused
    lb: round_robin
    endpoints: [{address: '127.0.0.1:9101'}, {address: '%s'}]
    outlier_detection: {consecutive_failures: 1000}
  - name: tight
    endpoints: [{address: '%s'}]
    retry_budget: {ratio: 0.1, min_per_sec: 0.1}
routes:
  - {name: faulty, upstream: faulty, match: {path_prefix: /faulty}, strip_prefix: /faulty, retry: {attempts: 3}, timeouts: {per_try: 1s}}
  - {name: refused, upstream: refused, match: {path_prefix: /refused}, strip_prefix: /refused, retry: {attempts: 3}, timeouts: {connect: 1s}}
  - {name: tight, upstream: tight, match: {path_prefix: /tight}, strip_prefix: /tight, retry: {attempts: 2}}
`, fault.addr, refused, tight.addr))
	retries := func(upstream, outcome string) float64 {
		return g.metrics(t).value(fmt.Sprintf(`gw_retries_total{outcome=%q,upstream=%q}`, outcome, upstream))
	}
	// send sends n requests to path and returns the statuses other than
	// want.
	send := func(path string, n, want int) []int {
		t.Helper()
		var other []int
		for range n {
			if code, _, _ := g.get(t, path); code != want {
				other = append(other, code)
			}
		}
		return other
	}

	for _, tt := range []struct {
		name string
		mode int32
	}{{"503", fault503}, {"connection closed", faultClose}} {
		fault.mode.Store(tt.mode)
		before, sent := fault.traffic.Load(), retries("faulty", "sent")
		if other := send("/faulty/status/200", 30, http.StatusOK); len(other) != 0 {
			t.Errorf("%s: %d of 30 requests failed: %v", tt.name, len(other), other)
		}
		// Go's transport may send a request again, below the attempt
		// level, on a reused connection the fault closed: at least 15.
		if n, r := fault.traffic.Load()-before, retries("faulty", "sent")-sent; n < 15 || r != 15 {
			t.Errorf("%s: %d attempts reached the failing endpoint and %v retries were sent; want 15 or more, and 15", tt.name, n, r)
		}
	}
	fault.mode.Store(faultNone)

	sent := retries("refused", "sent")
	if other := send("/refused/status/200", 30, http.StatusOK); len(other) != 0 {
		t.Errorf("refusing endpoint: %d of 30 requests failed: %v", len(other), other)
	}
	if r := retries("refused", "sent") - sent; r != 15 {
		t.Errorf("refusing endpoint: %v retries sent, want 15", r)
	}

	// The budget binds: request i's retry is granted while R + 1 ≤ 0.1 × i
	// + 1, at i = 1, 10 and 20. The 20 requests take far less than the
	// budget's 10 s window.
	tight.mode.Store(fault503)
	start := time.Now()
	if other := send("/tight/status/200", 20, http.StatusServiceUnavailable); len(other) != 0 {
		t.Errorf("all-failing upstream: %d of 20 requests not 503: %v", len(other), other)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("20 requests took %v; the budget's window is 10 s", elapsed)
	}
	m := g.metrics(t)
	for series, want := range map[string]float64{
		`gw_retries_total{outcome="sent",upstream="tight"}`:                                                            3,
		`gw_retries_total{outcome="budget_exhausted",upstream="tight"}`:                                                17,
		`gw_upstream_duration_seconds_count{attempt="1",upstream="tight"}`:                                             20,
		`gw_upstream_duration_seconds_count{attempt="2",upstream="tight"}`:                                             3,
		`gw_requests_total{code_class="5xx",method="GET",origin="upstream",reason="none",route="tight",tenant="none"}`: 20,
	} {
		if got := m.value(series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if n := tight.traffic.Load(); n != 23 {
		t.Errorf("all-failing upstream got %d attempts, want 23", n)
	}

	// Deadline propagation: go-httpbin echoes the request headers.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.url+"/faulty/headers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Spillway-Deadline-Ms", "99999")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var echo struct {
		Headers map[string][]string `json:"headers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&echo); err != nil {
		t.Fatal(err)
	}
	got := echo.Headers["Spillway-Deadline-Ms"]
	if ms, err := strconv.Atoi(firstOf(got)); len(got) != 1 || err != nil || ms <= 900 || ms > 1000 {
		t.Errorf("upstream got Spillway-Deadline-Ms %q, want one value from 901 to 1000 (the route's 1 s per-try)", got)
	}
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
