//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// TestGatewayCircuitBreaker runs a gateway on the host in front of the
// compose mock upstreams upstream-1 and upstream-2 (published on 9101 and
// 9102), each reached through a proxy in this test that can fail it, and
// checks the upstream's circuit breaker (GW-14, ADR-0030):
//
//   - with both endpoints answering 503, the breaker opens once its window
//     holds min_requests attempts, and the gateway then answers 503
//     circuit_open itself, with Retry-After, sending nothing upstream;
//   - after open_for, a failing probe opens it again;
//   - once the endpoints recover, after open_for, half_open_probes
//     successful probes close it and every request succeeds.
//
// Outlier ejection is held off, so that both endpoints stay in rotation and
// the breaker alone protects the upstream.
func TestGatewayCircuitBreaker(t *testing.T) {
	f1, f2 := newFaultProxy(t, "127.0.0.1:9101"), newFaultProxy(t, "127.0.0.1:9102")
	g := startGateway(t, os.Getenv(redis.AddrMapEnv), fmt.Sprintf(`
version: 1
upstreams:
  - name: all
    lb: round_robin
    endpoints: [{address: '%s'}, {address: '%s'}]
    outlier_detection: {consecutive_failures: 1000}
    circuit: {min_requests: 10, open_for: 2s, half_open_probes: 3}
routes:
  - {name: all, upstream: all, match: {path_prefix: /all}, strip_prefix: /all, retry: {attempts: 2}}
`, f1.addr, f2.addr))
	const open = `{"error":"upstream circuit open"}`
	state := func() float64 {
		v, ok := g.metrics(t)[`gw_circuit_state{upstream="all"}`]
		if !ok {
			t.Fatal("no gw_circuit_state series for upstream all")
		}
		return v
	}
	traffic := func() int64 { return f1.traffic.Load() + f2.traffic.Load() }
	setFault := func(mode int32) {
		f1.mode.Store(mode)
		f2.mode.Store(mode)
	}

	for i := range 10 {
		if code, _, _ := g.get(t, "/all/status/200"); code != http.StatusOK {
			t.Fatalf("all up, request %d: %d, want 200", i+1, code)
		}
	}
	if s := state(); s != 0 {
		t.Fatalf("all up: gw_circuit_state %v, want 0", s)
	}

	// Every endpoint fails: each request makes 2 attempts, so the 5th
	// request's second attempt is the 10th failure, half of the 20
	// attempts in the window, which opens it.
	setFault(fault503)
	for i := range 5 {
		if code, _, body := g.get(t, "/all/status/200"); code != http.StatusServiceUnavailable || body == open {
			t.Fatalf("failing, request %d: %d %q, want the upstream's 503", i+1, code, body)
		}
	}
	opened := time.Now()
	if s := state(); s != 1 {
		t.Fatalf("after 10 failed attempts of 20: gw_circuit_state %v, want 1 (open)", s)
	}
	g.waitLog(t, `"msg":"upstream circuit open","upstream":"all","from":"closed","error_rate":0.5,"requests":20`)
	before := traffic()
	for i := range 20 {
		code, h, body := g.get(t, "/all/status/200")
		if ra := h.Get("Retry-After"); code != http.StatusServiceUnavailable || body != open || ra != "1" && ra != "2" {
			t.Fatalf("open, request %d: %d %q Retry-After %q; want 503 %s with Retry-After 1 or 2", i+1, code, body, ra, open)
		}
	}
	if n := traffic() - before; n != 0 {
		t.Errorf("open: %d requests reached the upstream, want 0", n)
	}
	if time.Since(opened) >= 2*time.Second {
		t.Fatal("the open checks outlasted open_for; the host stalled")
	}

	// After open_for, still failing: the probe fails and it opens again.
	time.Sleep(time.Until(opened.Add(2100 * time.Millisecond)))
	if code, _, body := g.get(t, "/all/status/200"); code != http.StatusServiceUnavailable || body == open {
		t.Errorf("failing probe: %d %q, want the upstream's 503", code, body)
	}
	reopened := time.Now()
	g.waitLog(t, `"msg":"upstream circuit half-open","upstream":"all"`)
	g.waitLog(t, `"msg":"upstream circuit open","upstream":"all","from":"half_open"`)
	if code, _, body := g.get(t, "/all/status/200"); code != http.StatusServiceUnavailable || body != open {
		t.Errorf("after a failed probe: %d %q, want 503 %s", code, body, open)
	}
	if n := traffic() - before; n != 1 {
		t.Errorf("the failing probe made %d attempts, want 1", n)
	}

	// Recovery: after open_for, 3 probes pass and close it.
	setFault(faultNone)
	time.Sleep(time.Until(reopened.Add(2100 * time.Millisecond)))
	for i := range 20 {
		if code, _, body := g.get(t, "/all/status/200"); code != http.StatusOK {
			t.Fatalf("recovered, request %d: %d %q, want 200", i+1, code, body)
		}
	}
	if s := state(); s != 0 {
		t.Errorf("recovered: gw_circuit_state %v, want 0 (closed)", s)
	}
	g.waitLog(t, `"msg":"upstream circuit closed","upstream":"all"`)
	m := g.metrics(t)
	if v := m.value(`gw_requests_total{code_class="5xx",method="GET",origin="gateway",reason="circuit_open",route="all",tenant="none"}`); v != 21 {
		t.Errorf("circuit_open answers %v, want 21", v)
	}
}
